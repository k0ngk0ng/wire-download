"""Verify a new CLI against a real legacy daemon using only a local fixture.

Usage: python3 scripts/test-daemon-compat.py NEW_PLUGIN OLD_PLUGIN [REGRESSED_PLUGIN]
All temporary state is under this repository and removed after success.
"""
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time


root = Path(__file__).resolve().parent.parent
new, old = (str(Path(p).resolve()) for p in sys.argv[1:3])
regressed = str(Path(sys.argv[3]).resolve()) if len(sys.argv) > 3 else None
(root / '.test-data').mkdir(exist_ok=True)
work = Path(tempfile.mkdtemp(prefix='dc-', dir=root / '.test-data'))
state, downloads = work / 's', work / 'downloads'
env = {**os.environ, 'TMPDIR': str(work), 'WIRECTL_DOWNLOAD_HOME': str(state)}
payload = b'wire-download daemon compatibility fixture\n' * 1024
requests = []


def cli(binary, *args, check=True):
    result = subprocess.run([binary, *args], env=env, text=True,
                            capture_output=True, timeout=140)
    if check and result.returncode:
        raise AssertionError(result.stdout + result.stderr)
    return result


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        requests.append(self.path)
        self.send_response(200)
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
started = success = False
try:
    cli(old, 'init', '--downloads', str(downloads))
    config_path = state / 'config.json'
    config = json.loads(config_path.read_text())
    reserved, ports = [], set()
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
    config['server_list_urls'] = []
    config['trackers'] = []
    config_path.write_text(json.dumps(config))
    for sock in reserved:
        sock.close()
    cli(old, 'daemon', 'start')
    started = True
    old_pid = (state / 'daemon.pid').read_text()
    url = f'http://127.0.0.1:{server.server_port}/fixture.bin'
    if regressed:
        result = cli(regressed, 'add', '--detach', url, check=False)
        assert result.returncode and 'daemon HTTP 404' in result.stderr, result
        assert not requests, 'Regressed CLI unexpectedly downloaded the fixture'
        print('PASS reproduced original 404 with the pre-fix CLI and legacy daemon', flush=True)
    cli(new, 'add', '--detach', url)
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        jobs = json.loads(cli(new, 'list', '--json').stdout)['jobs']
        if len(jobs) == 1 and jobs[0]['status'] == 'complete':
            break
        time.sleep(.2)
    assert len(jobs) == 1 and jobs[0]['status'] == 'complete', jobs
    assert (downloads / 'fixture.bin').read_bytes() == payload
    assert (state / 'daemon.pid').read_text() == old_pid, 'Legacy daemon was replaced during submission'
    print('PASS new CLI + unchanged legacy daemon downloaded and verified local bytes', flush=True)
    result = cli(new, 'add', '--detach', 'https://x.com/a/status/123', check=False)
    assert result.returncode and 'daemon restart' in result.stderr and 'upgrade' in result.stderr, result
    assert len(json.loads(cli(new, 'list', '--json').stdout)['jobs']) == 1, 'Video became an HTML job'
    print('PASS legacy video request explains recovery and creates no HTML task', flush=True)
    count = len(requests)
    cli(new, 'daemon', 'restart')
    assert (state / 'daemon.pid').read_text() != old_pid, 'Restart kept the old daemon'
    time.sleep(3)
    after = json.loads(cli(new, 'list', '--json').stdout)['jobs']
    assert len(after) == 1 and after[0]['id'] == jobs[0]['id'] and after[0]['status'] == 'complete', after
    assert len(requests) == count, 'Restart downloaded a completed task again'
    assert (downloads / 'fixture.bin').read_bytes() == payload
    print('PASS restart upgrades legacy daemon and preserves completed task/file', flush=True)
    cli(new, 'daemon', 'stop')
    started = False
    cli(new, 'daemon', 'restart')
    started = True
    assert json.loads(cli(new, 'list', '--json').stdout)['jobs'][0]['id'] == jobs[0]['id']
    print('PASS restart starts an already stopped daemon', flush=True)
    success = True
finally:
    if started:
        cli(new, 'daemon', 'stop')
    server.shutdown()
    server.server_close()
    if success:
        shutil.rmtree(work)
        print('PASS temporary fixture and state removed', flush=True)
    else:
        print('Failure evidence:', work, flush=True)
