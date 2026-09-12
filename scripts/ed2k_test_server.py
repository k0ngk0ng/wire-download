"""Local ED2K test fixture; never a production/public server.

Only accepts this machine's chosen interface address. Supplies the ordinary
login acknowledgement needed by two real aMule engines to exercise a direct
peer transfer without depending on public server availability.
"""
import argparse
import json
import socket
import socketserver
import struct
import threading
from pathlib import Path


def read_exact(connection, size):
    result = bytearray()
    while len(result) < size:
        block = connection.recv(size - len(result))
        if not block:
            raise EOFError
        result.extend(block)
    return bytes(result)


class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        print("Accepted local test connection", self.client_address, flush=True)
        if self.client_address[0] != self.server.server_address[0]:
            return
        self.request.settimeout(120)
        try:
            while True:
                header = read_exact(self.request, 5)
                length = struct.unpack('<I', header[1:])[0]
                print('Packet', hex(header[0]), length, flush=True)
                if header[0] != 0xE3 or not 1 <= length <= 4 * 1024 * 1024:
                    return
                packet = read_exact(self.request, length)
                if packet[0] == 0x01 and len(packet) >= 23:  # OP_LOGINREQUEST
                    payload = b'\x40' + socket.inet_aton(self.client_address[0])
                    self.request.sendall(b'\xe3' + struct.pack('<I', len(payload)) + payload)
                    with self.server.login_lock:
                        self.server.login_count += 1
                        self.server.client_ids.add(packet[1:17])
        except (EOFError, OSError):
            return


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, *args, **kwargs):
        self.login_lock = threading.Lock()
        self.login_count = 0
        self.client_ids = set()
        super().__init__(*args, **kwargs)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--address', required=True)
    parser.add_argument('--ready-file', required=True)
    parser.add_argument('--port', type=int, default=0)
    args = parser.parse_args()
    with Server((args.address, args.port), Handler) as server:
        Path(args.ready_file).write_text(json.dumps({'address': args.address, 'port': server.server_address[1]}))
        print('Local ED2K fixture ready', server.server_address, flush=True)
        server.serve_forever()
