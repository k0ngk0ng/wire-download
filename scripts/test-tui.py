"""Verify the installed CLI in a real pseudo-terminal using an owned HTTP fixture."""
import fcntl
import http.server
import json
import os
from pathlib import Path
import pty
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time

root = Path(__file__).resolve().parent.parent
prefix = Path(sys.argv[1]).resolve()
(root / '.test-data').mkdir(exist_ok=True)
work = Path(tempfile.mkdtemp(prefix='tui-', dir=root / '.test-data'))
state = work / 'state'
env = {**os.environ, 'PATH': '/usr/bin:/bin', 'TERM': 'xterm-256color',
       'WIRECTL_DOWNLOAD_HOME': str(state), 'TMPDIR': str(work)}
command = [str(prefix / 'bin/wirectl'), 'download']
payload = b'owned terminal integration fixture\n' * 65536


def cli(*args):
    return subprocess.check_output(command + list(args), env=env, text=True, timeout=100)


def jobs():
    return json.loads(cli('list', '--json'))['jobs']


def wait(predicate, timeout=45):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if predicate():
            return
        time.sleep(.1)
    raise AssertionError('Terminal integration condition timed out')


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        offset = int(self.headers.get('Range', 'bytes=0-').split('=')[1].split('-')[0])
        self.send_response(206 if offset else 200)
        self.send_header('Content-Length', str(len(payload) - offset))
        self.send_header('Accept-Ranges', 'bytes')
        if offset:
            self.send_header('Content-Range', f'bytes {offset}-{len(payload)-1}/{len(payload)}')
        self.end_headers()
        try:
            for start in range(offset, len(payload), 16384):
                self.wfile.write(payload[start:start + 16384])
                self.wfile.flush()
                time.sleep(.2)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 100, 0, 0))
original_terminal = termios.tcgetattr(slave)
chunks = []
process = None
started = False


def capture():
    try:
        while True:
            data = os.read(master, 65536)
            if not data:
                return
            chunks.append(data)
    except OSError:
        pass


try:
    cli('init', '--downloads', str(work / 'downloads'))
    path = state / 'config.json'
    config = json.loads(path.read_text())
    sockets = []
    used = set()
    for key in ('aria2_port', 'amule_ec_port', 'bt_port', 'ed2k_port', 'kad_port', 'auth_proxy_port'):
        while True:
            sock = socket.socket()
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
            if port < 65532 and all(abs(port - other) > 3 for other in used):
                break
            sock.close()
        sockets.append(sock)
        used.add(port)
        config[key] = port
    config['server_list_urls'] = []
    path.write_text(json.dumps(config))
    for sock in sockets:
        sock.close()
    cli('daemon', 'start')
    started = True
    threading.Thread(target=capture, daemon=True).start()
    process = subprocess.Popen(command + [f'http://127.0.0.1:{server.server_port}/fixture.bin'],
                               stdin=slave, stdout=slave, stderr=slave, env=env)
    wait(lambda: b'PROGRESS' in b''.join(chunks) and '░'.encode() in b''.join(chunks))
    wait(lambda: jobs() and 0 < jobs()[0]['progress'] < 100)
    os.write(master, b'p')
    wait(lambda: jobs()[0]['status'] == 'paused')
    os.write(master, b'r')
    wait(lambda: jobs()[0]['status'] == 'active')
    os.write(master, b'd')
    wait(lambda: b'y confirms' in b''.join(chunks))
    os.write(master, b'n')
    time.sleep(.3)
    assert jobs()[0]['status'] != 'removed', 'Unconfirmed removal was executed'
    os.write(master, b'd')
    time.sleep(.3)
    os.write(master, b'y')
    wait(lambda: jobs()[0]['status'] == 'removed')
    os.write(master, b'q')
    assert process.wait(timeout=10) == 0
    assert termios.tcgetattr(slave) == original_terminal, 'TTY attributes were not restored'
    wait(lambda: b'\x1b[?1049l' in b''.join(chunks) and b'\x1b[?25h' in b''.join(chunks), timeout=5)
    output = b''.join(chunks)
    assert b'\x1b[?1049h' in output and b'\x1b[?1049l' in output
    assert b'\x1b[?25l' in output and b'\x1b[?25h' in output
    assert all(value == 'ok' for value in json.loads(cli('list', '--json'))['engines'].values())
    (work / 'result.json').write_text(json.dumps({'passed': True, 'checks': [
        'automatic dashboard and live progress', 'keyboard pause and resume',
        'removal requires confirmation', 'TTY and cursor restored',
        'daemon remains healthy after quitting dashboard']}, indent=2) + '\n')
    print('PASS real terminal progress, keyboard controls, confirmation, cleanup, and independent daemon')
finally:
    if process is not None and process.poll() is None:
        process.terminate()
        process.wait(timeout=15)
    if started:
        cli('daemon', 'stop')
    server.shutdown()
    (work / 'terminal.ansi').write_bytes(b''.join(chunks))
    os.close(slave)
    os.close(master)
    print('Test evidence:', work)
