"""Real ED2K acceptance test using two installed engines and a local server.

Usage: python3 scripts/test-ed2k.py PREFIX --address LOCAL_NON_LOOPBACK_IPV4
Does not contact public ED2K servers. Test profiles/data stay in .test-data.
"""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time

sys.dont_write_bytecode = True
from ed2k_test_server import Server, Handler

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('prefix', type=Path)
parser.add_argument('--address', required=True)
parser.add_argument('--timeout', type=int, default=180)
args = parser.parse_args()
address = ipaddress.IPv4Address(args.address)
if address.is_loopback or address.is_unspecified or address.is_multicast:
    parser.error('Use an IPv4 address assigned to this machine; aMule excludes loopback sources')
prefix = args.prefix.resolve()
root = Path(__file__).resolve().parent.parent
(root / '.test-data').mkdir(exist_ok=True)
work = Path(tempfile.mkdtemp(prefix='ed2k-', dir=root / '.test-data'))
server = Server((str(address), 0), Handler)  # bind verifies this is a local address
threading.Thread(target=server.serve_forever, daemon=True).start()
base_env = {**os.environ, 'PATH': '/usr/bin:/bin', 'TMPDIR': str(work)}
configs = {}
started = []
ports = set()
evidence = {'passed': False, 'address': str(address), 'steps': []}


def cli(profile, *command):
    result = subprocess.run([str(prefix / 'bin/wirectl'), 'download', '--data-dir',
                             str(work / profile), *command], env=base_env,
                            capture_output=True, text=True, timeout=100)
    if result.returncode:
        raise RuntimeError(f'{profile} {command[0]}: {result.stderr.strip()}')
    return result.stdout


def ec(profile, command):
    result = subprocess.run([str(prefix / 'libexec/wirectl-download/bin/amulecmd'),
                             '--config-file=../amule/remote.conf',
                             '--command=' + command], cwd=work / profile,
                            env={**base_env, 'LC_ALL': 'C'}, text=True,
                            capture_output=True, check=True, timeout=35)
    return result.stdout


def wait_for(check, label, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            evidence['steps'].append(label)
            print('PASS', label, flush=True)
            return result
        time.sleep(1)
    raise RuntimeError('Timed out: ' + label)


def prepare(profile):
    destination = work / (profile + '-files')
    cli(profile, 'init', '--downloads', str(destination))
    path = work / profile / 'config.json'
    config = json.loads(path.read_text())
    for key in ('aria2_port', 'amule_ec_port', 'bt_port', 'ed2k_port', 'kad_port', 'auth_proxy_port'):
        while True:
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            if port <= 65532 and all(abs(port - p) > 3 for p in ports):
                ports.add(port)
                break
        config[key] = port
    config['server_list_urls'] = []
    path.write_text(json.dumps(config))
    amule = path.parent / 'amule'
    amule.mkdir()
    # Local test-only settings: retain production source validation, but allow
    # private interface addresses for these two owned peers. Kad is disconnected
    # through EC immediately after each daemon starts below.
    (amule / 'amule.conf').write_text('[eMule]\nFilterLanIPs=0\n')
    met = b'\xe0' + struct.pack('<I', 1) + address.packed
    met += struct.pack('<HI', server.server_address[1], 0)
    (amule / 'server.met').write_bytes(met)
    destination.mkdir()
    configs[profile] = config
    return destination


try:
    seed_files = prepare('seed')
    download_files = prepare('download')
    payload = hashlib.sha256(b'wirectl owned ED2K fixture').digest() * 4096
    filename = 'ed2k-fixture.bin'
    (seed_files / filename).write_bytes(payload)
    for profile in ('seed', 'download'):
        cli(profile, 'daemon', 'start')
        started.append(profile)
        ec(profile, 'disconnect kad')
    def shared_hash():
        output = ec('seed', 'show shared')
        (work / 'seed-shared.txt').write_text(output)
        match = re.search(r'(?im)^\s*(?:>\s*)?([0-9a-f]{32})[^\r\n]*ed2k-fixture\.bin', output)
        return match.group(1).lower() if match else None

    digest = wait_for(shared_hash, 'seed hashes and shares complete fixture', 30)
    wait_for(lambda: len(server.client_ids) >= 2, 'both engines log into local ED2K server', 20)
    link = (f'ed2k://|file|{filename}|{len(payload)}|{digest}|/'
            f'|sources,{address}:{configs["seed"]["ed2k_port"]}|/')
    cli('download', 'add', '--detach', link)

    def complete():
        snapshot = json.loads(cli('download', 'list', '--json'))
        (work / 'last-status.json').write_text(json.dumps(snapshot, indent=2))
        return any(j['engine_id'] == digest and j['status'] == 'complete'
                   for j in snapshot['jobs'])

    wait_for(complete, 'real ED2K transfer reaches complete state', args.timeout)
    assert (download_files / filename).read_bytes() == payload, 'downloaded bytes differ'
    evidence['steps'].append('downloaded bytes exactly match generated source')
    evidence['passed'] = True
    print('PASS downloaded bytes exactly match generated source', flush=True)
except Exception as error:
    evidence['error'] = str(error)
    raise
finally:
    for profile in reversed(started):
        try:
            cli(profile, 'daemon', 'stop')
        except Exception as error:
            evidence.setdefault('cleanup_errors', []).append(str(error))
    server.shutdown()
    server.server_close()
    evidence['server_logins'] = server.login_count
    evidence['distinct_server_clients'] = len(server.client_ids)
    (work / 'result.json').write_text(json.dumps(evidence, indent=2) + '\n')
    print('Test evidence:', work, flush=True)
