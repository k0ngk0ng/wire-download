"""Exercise an installed release with no package-manager tools on PATH.

Usage: python3 scripts/test-install.py /absolute/install/prefix
All test state and downloads stay under this repository's .test-data directory.
"""
import hashlib
import http.server
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time


root = Path(__file__).resolve().parent.parent
prefix = Path(sys.argv[1]).resolve()
(root / '.test-data').mkdir(exist_ok=True)
work = Path(tempfile.mkdtemp(prefix='installed-', dir=root / '.test-data'))
state = work / 'state'
downloads = work / 'downloads'
env = {**os.environ, 'PATH': '/usr/bin:/bin', 'WIRECTL_DOWNLOAD_HOME': str(state),
       'TMPDIR': str(work)}
command = [str(prefix / 'bin/wirectl'), 'download']


def cli(*args):
    result = subprocess.run(command + list(args), env=env, check=True,
                            text=True, capture_output=True, timeout=100)
    return result.stdout


def jobs():
    return json.loads(cli('list', '--json'))['jobs']


def wait_for(predicate, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        snapshot = jobs()
        if predicate(snapshot):
            return snapshot
        time.sleep(.3)
    raise AssertionError('Timed out waiting for expected task state')


payload = b'wirectl offline installation fixture\n' * 524288


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/private.bin' and self.headers.get('Cookie') != 'session=owned-browser-session':
            self.send_response(401)
            self.send_header('Content-Length', '0')
            self.end_headers()
            return
        start = 0
        if self.headers.get('Range'):
            start = int(self.headers['Range'].split('=')[1].split('-')[0])
        if start >= len(payload):
            self.send_response(416)
            self.end_headers()
            return
        self.send_response(206 if start else 200)
        self.send_header('Accept-Ranges', 'bytes')
        if start:
            self.send_header('Content-Range', f'bytes {start}-{len(payload)-1}/{len(payload)}')
        self.send_header('Content-Length', str(len(payload) - start))
        self.end_headers()
        try:
            for offset in range(start, len(payload), 65536):
                self.wfile.write(payload[offset:offset+65536])
                self.wfile.flush()
                time.sleep(.04)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
started = False
try:
    cli('init', '--downloads', str(downloads))
    config_path = state / 'config.json'
    config = json.loads(config_path.read_text())
    # Reserve distinct test control/P2P ports without using production defaults.
    reserved = []
    ports = set()
    for key in ('aria2_port', 'amule_ec_port', 'bt_port', 'ed2k_port', 'kad_port', 'auth_proxy_port'):
        while True:
            sock = socket.socket()
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
            if port <= 65532 and all(abs(port - p) > 3 for p in ports):
                break
            sock.close()
        reserved.append(sock)
        ports.add(port)
        config[key] = port
    # Bootstrap from bundled data so this acceptance test needs no Internet.
    config['server_list_urls'] = []
    config_path.write_text(json.dumps(config))
    for sock in reserved:
        sock.close()
    doctor = cli('doctor')
    assert doctor.count(str(prefix / 'libexec/wirectl-download/bin')) == 3, doctor
    print('PASS installed engines resolved with minimal PATH', flush=True)
    cli('daemon', 'start')
    started = True
    assert (state / 'daemon.sock').stat().st_mode & 0o777 == 0o600
    cli('add', '--detach', f'http://127.0.0.1:{server.server_port}/fixture.bin')
    snapshot = wait_for(lambda js: js and 0 < js[0]['progress'] < 100)
    task_id = snapshot[0]['id']
    cli('pause', task_id)
    wait_for(lambda js: js[0]['status'] == 'paused')
    cli('daemon', 'stop')
    started = False
    cli('daemon', 'start')
    started = True
    snapshot = wait_for(lambda js: js and js[0]['status'] == 'paused')
    assert snapshot[0]['id'] == task_id
    print('PASS pause and daemon restart preserve task identity/state', flush=True)
    cli('resume', task_id)
    wait_for(lambda js: js[0]['status'] == 'complete')
    assert hashlib.sha256((downloads / 'fixture.bin').read_bytes()).digest() == hashlib.sha256(payload).digest()
    cli('remove', task_id)
    assert (downloads / 'fixture.bin').is_file()
    print('PASS resumed download matches bytes; removal retains completed file', flush=True)
    session = {'origin': f'http://127.0.0.1:{server.server_port}',
               'user_agent': 'owned browser fixture',
               'cookies': [{'name': 'session', 'value': 'owned-browser-session',
                            'domain': '127.0.0.1', 'path': '/', 'httpOnly': True}]}
    subprocess.run(command + ['login', '--import'], env=env, input=json.dumps(session),
                   text=True, capture_output=True, check=True, timeout=10)
    cli('add', '--detach', f'http://127.0.0.1:{server.server_port}/private.bin')
    auth_jobs = wait_for(lambda js: any(j['name'] == 'private.bin' and 0 < j['progress'] < 100 for j in js))
    auth_id = next(j['id'] for j in auth_jobs if j['name'] == 'private.bin')
    cli('pause', auth_id)
    cli('daemon', 'stop')
    started = False
    cli('daemon', 'start')
    started = True
    cli('resume', auth_id)
    wait_for(lambda js: any(j['id'] == auth_id and j['status'] == 'complete' for j in js))
    assert (downloads / 'private.bin').read_bytes() == payload
    assert 'owned-browser-session' not in (state / 'aria2/session.txt').read_text()
    assert 'owned-browser-session' not in (state / 'jobs.json').read_text()
    cli('logout', session['origin'])
    assert not list((state / 'auth').glob('*.json'))
    print('PASS authenticated HTTP, restart/resume, private cookie storage and logout', flush=True)
finally:
    if started:
        cli('daemon', 'stop')
    server.shutdown()
    print('Test evidence:', work, flush=True)
