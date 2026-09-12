"""Local ED2K test fixture; never a production/public server.

Only accepts this machine's chosen interface address. Supplies the login,
search, and source responses needed by two real aMule engines to exercise a
direct peer transfer without depending on public server availability.
"""
import argparse
import json
import socket
import socketserver
import struct
import threading
from pathlib import Path


OP_LOGINREQUEST = 0x01
OP_SEARCHREQUEST = 0x16
OP_GETSOURCES = 0x19
OP_SEARCHRESULT = 0x33
OP_IDCHANGE = 0x40
OP_FOUNDSOURCES = 0x42

TAGTYPE_STRING = 0x02
TAGTYPE_UINT32 = 0x03

FT_FILENAME = 0x01
FT_FILESIZE = 0x02
FT_SOURCES = 0x15


def read_exact(connection, size):
    result = bytearray()
    while len(result) < size:
        block = connection.recv(size - len(result))
        if not block:
            raise EOFError
        result.extend(block)
    return bytes(result)


def named_tag(tag_type, name, value):
    """Encode the compact one-byte-name form used by old ED2K tags."""
    if not 0 <= name <= 0xFF:
        raise ValueError('tag name must fit in one byte')
    return bytes((tag_type | 0x80, name)) + value


def string_tag(name, value):
    encoded = value.encode('utf-8')
    if len(encoded) > 0xFFFF:
        raise ValueError('tag string is too long')
    return named_tag(TAGTYPE_STRING, name, struct.pack('<H', len(encoded)) + encoded)


def uint32_tag(name, value):
    if not 0 <= value <= 0xFFFFFFFF:
        raise ValueError('tag integer is out of range')
    return named_tag(TAGTYPE_UINT32, name, struct.pack('<I', value))


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
                opcode = packet[0]
                if opcode == OP_LOGINREQUEST and len(packet) >= 23:
                    payload = bytes((OP_IDCHANGE,)) + socket.inet_aton(self.client_address[0])
                    self.request.sendall(b'\xe3' + struct.pack('<I', len(payload)) + payload)
                    with self.server.login_lock:
                        self.server.login_count += 1
                        self.server.client_ids.add(packet[1:17])
                elif opcode == OP_SEARCHREQUEST:
                    with self.server.login_lock:
                        self.server.search_count += 1
                        self.server.search_requests.append(packet[1:])
                    self.request.sendall(self.server.search_result_frame())
                elif opcode == OP_GETSOURCES:
                    requested_hash = packet[1:17]
                    with self.server.login_lock:
                        self.server.get_sources_count += 1
                    if len(requested_hash) == 16:
                        frame = self.server.found_sources_frame(requested_hash)
                        if frame is not None:
                            self.request.sendall(frame)
        except (EOFError, OSError):
            return


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, *args, **kwargs):
        self.login_lock = threading.Lock()
        self.login_count = 0
        self.client_ids = set()
        self.search_count = 0
        self.get_sources_count = 0
        self.search_requests = []
        self.search_hash = None
        self.search_name = ''
        self.search_size = 0
        self.search_peer_ip = ''
        self.search_peer_port = 0
        super().__init__(*args, **kwargs)

    def configure_search(self, file_hash, name, size, peer_ip, peer_port):
        """Set the one owned result served by this fixture."""
        if len(file_hash) != 16:
            raise ValueError('search hash must contain 16 bytes')
        socket.inet_aton(str(peer_ip))
        if not name or not 0 < size <= 0xFFFFFFFF or not 0 < peer_port <= 0xFFFF:
            raise ValueError('invalid search fixture metadata')
        with self.login_lock:
            self.search_hash = bytes(file_hash)
            self.search_name = str(name)
            self.search_size = int(size)
            self.search_peer_ip = str(peer_ip)
            self.search_peer_port = int(peer_port)

    def search_result_frame(self):
        with self.login_lock:
            file_hash = self.search_hash
            name = self.search_name
            size = self.search_size
            peer_ip = self.search_peer_ip
            peer_port = self.search_peer_port
        if file_hash is None:
            body = struct.pack('<I', 0)
        else:
            tags = b''.join((
                string_tag(FT_FILENAME, name),
                uint32_tag(FT_FILESIZE, size),
                uint32_tag(FT_SOURCES, 1),
            ))
            body = (struct.pack('<I', 1) + file_hash + socket.inet_aton(peer_ip) +
                    struct.pack('<H', peer_port) + struct.pack('<I', 3) + tags)
        payload = bytes((OP_SEARCHRESULT,)) + body
        return b'\xe3' + struct.pack('<I', len(payload)) + payload

    def found_sources_frame(self, requested_hash):
        with self.login_lock:
            file_hash = self.search_hash
            peer_ip = self.search_peer_ip
            peer_port = self.search_peer_port
        if file_hash is None or requested_hash != file_hash:
            return None
        body = (file_hash + b'\x01' + socket.inet_aton(peer_ip) +
                struct.pack('<H', peer_port))
        payload = bytes((OP_FOUNDSOURCES,)) + body
        return b'\xe3' + struct.pack('<I', len(payload)) + payload


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
