"""Exercise the interactive search screen through a real pseudo-terminal.

The test uses only an installed prefix and an HTTP server bound to localhost.
It verifies source progress, stable result selection while a slower source
changes ordering, explicit download enqueueing, cancellation, and terminal
attribute restoration.

Usage: python3 scripts/test-search-tui.py /absolute/install/prefix
"""

import fcntl
import hashlib
import http.server
import json
import os
from pathlib import Path
import pty
import re
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
from urllib.parse import urlsplit
from xml.sax.saxutils import escape

sys.dont_write_bytecode = True

root = Path(__file__).resolve().parent.parent
prefix = Path(sys.argv[1]).resolve() if len(sys.argv) == 2 else None
if prefix is None or not (prefix / "bin/wirectl").is_file():
    raise SystemExit("usage: python3 scripts/test-search-tui.py /absolute/install/prefix")

(root / ".test-data").mkdir(exist_ok=True)
work = Path(tempfile.mkdtemp(prefix="search-tui-", dir=root / ".test-data"))
state = work / "state"
downloads = work / "downloads"
wirectl = prefix / "bin/wirectl"
command = [str(wirectl), "download", "--data-dir", str(state)]
env = {
    **os.environ,
    "PATH": str(prefix / "bin") + ":/usr/bin:/bin",
    "TERM": "xterm-256color",
    "LC_ALL": "C",
    "LANG": "C",
    "WIRECTL_DOWNLOAD_HOME": str(state),
    "TMPDIR": str(work),
}

# Two owned, valid v1 hashes.  The fast source initially puts B second; the
# delayed source raises B's seed count and moves it to the first row.
HASH_A = "0123456789abcdef0123456789abcdef01234567"
HASH_B = "89abcdef0123456789abcdef0123456789abcdef"


def result_id(info_hash):
    return hashlib.sha256(("bt:" + info_hash).encode()).hexdigest()[:12]


RESULT_A = result_id(HASH_A)
RESULT_B = result_id(HASH_B)


def cli(*args, timeout=60, check=True):
    result = subprocess.run(
        command + list(args),
        env=env,
        text=True,
        capture_output=True,
        timeout=timeout,
    )
    if check and result.returncode:
        raise RuntimeError(
            "command failed ({}): {}\n{}".format(
                result.returncode, " ".join(args), result.stderr.strip()
            )
        )
    return result


def jobs():
    output = cli("list", "--json").stdout
    return json.loads(output)["jobs"]


def wait_for(predicate, label, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.05)
    raise RuntimeError("timed out: " + label)


def rss_feed(items):
    body = ["<?xml version=\"1.0\"?><rss version=\"2.0\"><channel>"]
    body.append("<title>owned local search fixture</title>")
    for name, info_hash, seeds in items:
        magnet = "magnet:?xt=urn:btih:{}&amp;dn={}".format(info_hash, escape(name))
        body.extend(
            [
                "<item>",
                "<title>{}</title>".format(escape(name)),
                "<link>{}</link>".format(magnet),
                "<guid>urn:owned:{}</guid>".format(info_hash),
                "<seeders>{}</seeders>".format(seeds),
                "<leechers>0</leechers>",
                "</item>",
            ]
        )
    body.append("</channel></rss>")
    return "".join(body).encode()


class SearchFixtureServer(http.server.ThreadingHTTPServer):
    daemon_threads = True


class SearchHandler(http.server.BaseHTTPRequestHandler):
    server_version = "owned-search-fixture/1"

    def do_GET(self):
        path = urlsplit(self.path).path
        fixture = self.server.fixture
        if path == "/fast":
            self.server.fast_seen.set()
            data = rss_feed(
                [
                    ("owned result A", HASH_A, 9),
                    ("owned result B", HASH_B, 1),
                ]
            )
        elif path == "/slow":
            self.server.slow_seen.set()
            # Keep this request open long enough for the TUI to render the two
            # fast results and accept j before the order changes.
            time.sleep(2)
            data = rss_feed(
                [
                    ("owned result A", HASH_A, 9),
                    ("owned result B", HASH_B, 20),
                ]
            )
        elif path == "/cancel":
            self.server.cancel_seen.set()
            # The request context is cancelled by the daemon; the handler may
            # still be sleeping, which also proves the client did not wait for
            # an upstream response before returning from q.
            time.sleep(10)
            data = rss_feed([("cancel result", HASH_A, 1)])
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/rss+xml")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *_):
        pass


def snapshot_bytes(chunks):
    return b"".join(chunks)


ansi_re = re.compile(rb"\x1b\[[0-?]*[ -/]*[@-~]")


def plain(data):
    return ansi_re.sub(b"", data)


def last_frame(data):
    # Search.render homes the cursor before every frame.  Looking at the last
    # frame prevents an assertion from accidentally matching stale output.
    return data.split(b"\x1b[H")[-1]


def source_complete(data, source):
    """Return whether the latest rendered frame marks a source complete."""
    source_line = re.compile(rb"^" + re.escape(source.encode()) + rb"\s+complete\b", re.MULTILINE)
    return source_line.search(plain(last_frame(data))) is not None


def start_pty(args):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 120, 0, 0))
    original = termios.tcgetattr(slave)
    chunks = []

    def capture():
        try:
            while True:
                data = os.read(master, 65536)
                if not data:
                    return
                chunks.append(data)
        except OSError:
            pass

    threading.Thread(target=capture, daemon=True).start()
    process = subprocess.Popen(
        command + list(args),
        stdin=slave,
        stdout=slave,
        stderr=slave,
        env=env,
    )
    return master, slave, original, chunks, process


def finish_pty(master, slave, original, chunks, process, terminal_file):
    try:
        if process.poll() is None:
            try:
                os.write(master, b"q")
            except OSError:
                pass
            try:
                process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                process.terminate()
                process.wait(timeout=8)
        data = snapshot_bytes(chunks)
        terminal_file.write_bytes(data)
        if process.returncode != 0:
            raise RuntimeError("search TUI exited with status {}".format(process.returncode))
        if termios.tcgetattr(slave) != original:
            raise AssertionError("TTY attributes were not restored")
        return data
    finally:
        os.close(slave)
        os.close(master)


def prepare_config(server_port):
    cli("init", "--downloads", str(downloads))
    path = state / "config.json"
    config = json.loads(path.read_text())
    # Keep every control port private to this test and reserve them until the
    # configuration is written, avoiding accidental collisions with a daemon
    # already running on the host.
    reservations = []
    used = set()
    try:
        for key in (
            "aria2_port",
            "amule_ec_port",
            "bt_port",
            "ed2k_port",
            "kad_port",
            "auth_proxy_port",
        ):
            while True:
                sock = socket.socket()
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
                if 1024 <= port <= 65532 and all(abs(port - other) > 3 for other in used):
                    break
                sock.close()
            reservations.append(sock)
            used.add(port)
            config[key] = port
        # No public server list is needed: the daemon uses its bundled
        # bootstrap data for this test.
        config["server_list_urls"] = []
        path.write_text(json.dumps(config), encoding="utf-8")
    finally:
        for sock in reservations:
            sock.close()

    sources = [
        {
            "id": "fast",
            "name": "fast",
            "type": "rss",
            "url": "http://127.0.0.1:{}/fast?q={{query}}".format(server_port),
            "enabled": True,
        },
        {
            "id": "slow",
            "name": "slow",
            "type": "rss",
            "url": "http://127.0.0.1:{}/slow?q={{query}}".format(server_port),
            "enabled": True,
        },
        {
            "id": "cancel",
            "name": "cancel",
            "type": "rss",
            "url": "http://127.0.0.1:{}/cancel?q={{query}}".format(server_port),
            "enabled": True,
        },
    ]
    source_path = state / "search-sources.json"
    fd = os.open(source_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(fd, (json.dumps(sources, indent=2) + "\n").encode())
    finally:
        os.close(fd)


evidence = {
    "passed": False,
    "prefix": str(prefix),
    "work": str(work),
    "checks": [],
}
server = SearchFixtureServer(("127.0.0.1", 0), SearchHandler)
server.fixture = True
server.fast_seen = threading.Event()
server.slow_seen = threading.Event()
server.cancel_seen = threading.Event()
server_thread = threading.Thread(target=server.serve_forever, daemon=True)
server_thread.start()
started = False
first_job_source = None

try:
    prepare_config(server.server_port)
    cli("daemon", "start", timeout=100)
    started = True

    # Session 1: the fast feed renders two rows.  j selects the second row;
    # two seconds later the slow feed changes seed counts and reorders them.
    master, slave, original, chunks, process = start_pty(
        [
            "search",
            "--type",
            "magnet",
            "--source",
            "fast,slow",
            "--timeout",
            "30s",
            "owned",
        ]
    )
    try:
        wait_for(lambda: server.fast_seen.is_set(), "fast source request")
        wait_for(
            lambda: RESULT_A.encode() in plain(snapshot_bytes(chunks))
            and RESULT_B.encode() in plain(snapshot_bytes(chunks))
            and b"fast" in plain(snapshot_bytes(chunks)),
            "fast source progress and two results",
        )
        if source_complete(snapshot_bytes(chunks), "slow"):
            raise AssertionError("slow source completed before keyboard selection")
        os.write(master, b"j")
        wait_for(
            lambda: b"\x1b[7m" in last_frame(snapshot_bytes(chunks))
            and RESULT_B.encode() in last_frame(snapshot_bytes(chunks)).split(b"\x1b[7m", 1)[1].split(b"\r\n", 1)[0],
            "second result selection",
        )
        wait_for(lambda: server.slow_seen.is_set(), "slow source request")
        wait_for(
            lambda: source_complete(snapshot_bytes(chunks), "slow")
            and RESULT_B.encode() in last_frame(snapshot_bytes(chunks)),
            "slow source reorder",
            timeout=15,
        )
        frame = last_frame(snapshot_bytes(chunks))
        selected_part = frame.split(b"\x1b[7m", 1)[1].split(b"\r\n", 1)[0]
        if RESULT_B.encode() not in selected_part:
            raise AssertionError("selection moved with row instead of stable result ID")
        os.write(master, b"\r")
        wait_for(lambda: b"Queued" in snapshot_bytes(chunks), "explicit selected-result download")
        wait_for(
            lambda: any(HASH_B.lower() in job.get("source", "").lower() for job in jobs()),
            "download list retains selected source",
            timeout=20,
        )
        first_job_source = next(
            job["source"] for job in jobs() if HASH_B.lower() in job.get("source", "").lower()
        )
        evidence["checks"].extend(
            [
                "PTY source progress and two local RSS results",
                "stable second-row selection survived asynchronous seed reorder",
                "Enter enqueued the selected magnet and list.Source retained it",
            ]
        )
    finally:
        first_data = finish_pty(master, slave, original, chunks, process, work / "session-one.ansi")
        if b"\x1b[?1049h" not in first_data or b"\x1b[?1049l" not in first_data:
            raise AssertionError("search TUI did not enter and leave the alternate screen")

    # Session 2: cancellation must stop the daemon-side search, restore the
    # terminal, and leave the first queued task intact.
    master, slave, original, chunks, process = start_pty(
        [
            "search",
            "--type",
            "magnet",
            "--source",
            "cancel",
            "--timeout",
            "30s",
            "cancel-me",
        ]
    )
    try:
        wait_for(lambda: server.cancel_seen.is_set(), "slow cancellation source request")
        search_match = wait_for(
            lambda: re.search(r"\b[0-9a-f]{16}\b", plain(snapshot_bytes(chunks)).decode("utf-8", "ignore")),
            "search session ID",
        )
        search_id = search_match.group(0)
        os.write(master, b"q")
        wait_for(lambda: process.poll() is not None, "q exits search TUI", timeout=12)
        if process.returncode != 0:
            raise RuntimeError("cancelled search TUI exited with status {}".format(process.returncode))
        wait_for(
            lambda: json.loads(cli("search", "results", "--json", search_id, timeout=10).stdout).get("status")
            == "cancelled",
            "daemon search eventually cancelled",
            timeout=15,
        )
        remaining = jobs()
        if not any(job.get("source") == first_job_source for job in remaining):
            raise AssertionError("queued download disappeared after cancelling another search")
        evidence["checks"].extend(
            [
                "q cancelled the in-flight daemon search",
                "TTY attributes restored after cancellation",
                "previous download remained in daemon list",
            ]
        )
    finally:
        finish_pty(master, slave, original, chunks, process, work / "session-two.ansi")

    evidence["passed"] = True
    (work / "result.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
    print("PASS real search PTY progress, stable selection, enqueue, cancellation, and TTY cleanup", flush=True)
except Exception as error:
    evidence["error"] = str(error)
    raise
finally:
    if started:
        try:
            cli("daemon", "stop", timeout=100)
        except Exception as error:
            evidence.setdefault("cleanup_errors", []).append(str(error))
    server.shutdown()
    server.server_close()
    server_thread.join(timeout=5)
    (work / "result.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
    print("Test evidence:", work, flush=True)
