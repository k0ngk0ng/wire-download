"""Exercise the installed BitTorrent path through wirectl and its daemon.

The fixture is entirely local: one generated torrent, one HTTP tracker, one
aria2 seeder, and three independent wirectl leecher profiles.  The profiles
use a local torrent file, an HTTP torrent URL, and a magnet URI respectively.
Each profile is paused, stopped, restarted, resumed, and checked byte-for-byte.

Usage: python3 scripts/test-bt-recovery.py /absolute/install/prefix
"""

import argparse
import base64
import hashlib
import http.server
from xml.sax.saxutils import escape
import json
import os
from pathlib import Path
import re
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import traceback
from urllib.parse import parse_qs, quote, urlsplit
from urllib.request import Request, urlopen

sys.dont_write_bytecode = True

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("prefix", type=Path)
parser.add_argument("--timeout", type=int, default=180)
parser.add_argument(
    "--only",
    action="append",
    choices=("torrent", "http-torrent", "magnet", "http-torrent-auth"),
    help="run only the selected profile (repeat for multiple profiles)",
)
parser.add_argument("--search", action="store_true", help="discover the torrent/magnet through two local RSS indexes before downloading")
args = parser.parse_args()
if args.search and (not args.only or any(x not in ("http-torrent", "magnet") for x in args.only)):
    parser.error("--search requires --only http-torrent and/or --only magnet")

root = Path(__file__).resolve().parent.parent
prefix = args.prefix.resolve()
if not (prefix / "bin/wirectl").is_file():
    parser.error(f"not an installed prefix: {prefix}")
(root / ".test-data").mkdir(exist_ok=True)
work = Path(tempfile.mkdtemp(prefix="bt-", dir=root / ".test-data"))
base_env = {
    **os.environ,
    "PATH": "/usr/bin:/bin",
    "LC_ALL": "C",
    "LANG": "C",
    "TMPDIR": str(work),
}
wirectl = prefix / "bin/wirectl"
aria2c = prefix / "libexec/wirectl-download/bin/aria2c"
evidence = {
    "passed": False,
    "prefix": str(prefix),
    "work": str(work),
    "profiles": {},
    "tracker_requests": [],
}
used_ports = set()


def bencode(value):
    if isinstance(value, bytes):
        return str(len(value)).encode() + b":" + value
    if isinstance(value, str):
        return bencode(value.encode())
    if isinstance(value, int):
        return f"i{value}e".encode()
    if isinstance(value, dict):
        out = bytearray(b"d")
        keys = sorted(value, key=lambda key: key if isinstance(key, bytes) else key.encode())
        for key in keys:
            out.extend(bencode(key))
            out.extend(bencode(value[key]))
        out.extend(b"e")
        return bytes(out)
    raise TypeError(f"unsupported bencode value: {type(value)!r}")


def free_port():
    while True:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        if port >= 1024 and port <= 65532 and port not in used_ports:
            used_ports.add(port)
            return port


def distinct_ports(names):
    ports = {}
    for name in names:
        while True:
            port = free_port()
            if all(abs(port - other) > 4 for other in ports.values()):
                ports[name] = port
                break
    return ports


class Tracker:
    def __init__(self, seed_port):
        self.seed_port = seed_port
        self.requests = []
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                parsed = urlsplit(self.path)
                if parsed.path != "/announce":
                    self.send_error(404)
                    return
                query = parse_qs(parsed.query, keep_blank_values=True)
                outer.requests.append(
                    {
                        "path": self.path,
                        "event": query.get("event", [""])[0],
                        "peer_id": query.get("peer_id", [""])[0],
                    }
                )
                peers = b"\x7f\x00\x00\x01" + struct.pack(">H", outer.seed_port)
                body = bencode(
                    {
                        b"interval": 1,
                        b"min interval": 1,
                        b"complete": 1,
                        b"incomplete": 1,
                        b"peers": peers,
                    }
                )
                self.send_response(200)
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server.server_port}/announce"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


class FixtureServer:
    def __init__(self, torrent):
        self.torrent = torrent
        self.requests = []
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_HEAD(self):
                outer.serve(self, False)

            def do_GET(self):
                outer.serve(self, True)

            def log_message(self, *_):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def serve(self, handler, body):
        path = urlsplit(handler.path).path
        if path in {"/rss-one", "/rss-two", "/rss-offline"}:
            self.requests.append({"path": handler.path, "private": False, "authorized": False})
            if path == "/rss-offline":
                handler.send_error(503)
                return
            data = self.feed.encode()
            handler.send_response(200)
            handler.send_header("Content-Type", "application/rss+xml")
            handler.send_header("Content-Length", str(len(data)))
            handler.end_headers()
            if body:
                handler.wfile.write(data)
            return
        if path not in {"/fixture.torrent", "/private-fixture.torrent"}:
            handler.send_error(404)
            return
        private = path == "/private-fixture.torrent"
        authorized = handler.headers.get("Cookie") == "session=owned-browser-session"
        self.requests.append({"path": handler.path, "private": private, "authorized": authorized})
        if private and not authorized:
            handler.send_response(401)
            handler.send_header("Content-Length", "0")
            handler.end_headers()
            return
        handler.send_response(200)
        handler.send_header("Content-Type", "application/x-bittorrent")
        handler.send_header("Content-Length", str(len(self.torrent)))
        handler.end_headers()
        if body:
            handler.wfile.write(self.torrent)

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server.server_port}/fixture.torrent"

    @property
    def private_url(self):
        return f"http://127.0.0.1:{self.server.server_port}/private-fixture.torrent"

    @property
    def origin(self):
        return f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


class Seed:
    def __init__(self, binary, directory, rpc_port, bt_port, tracker_url):
        self.directory = directory
        self.rpc_port = rpc_port
        self.secret = "wirectl-bt-seed-secret"
        self.log_path = directory / "aria2.log"
        self.log_file = self.log_path.open("w")
        self.process = subprocess.Popen(
            [
                str(binary),
                "--no-conf=true",
                "--enable-rpc=true",
                "--rpc-listen-all=false",
                f"--rpc-listen-port={rpc_port}",
                f"--rpc-secret={self.secret}",
                f"--dir={directory}",
                f"--listen-port={bt_port}",
                "--enable-dht=false",
                "--enable-dht6=false",
                "--enable-peer-exchange=false",
                "--bt-enable-lpd=false",
                "--bt-external-ip=127.0.0.1",
                f"--bt-tracker={tracker_url}",
                "--seed-ratio=0",
                "--seed-time=600",
                "--file-allocation=none",
                "--check-integrity=true",
                "--allow-overwrite=false",
                "--max-download-result=10000",
                "--summary-interval=0",
                "--console-log-level=notice",
            ],
            env=base_env,
            stdout=self.log_file,
            stderr=subprocess.STDOUT,
        )

    def call(self, method, params=None, timeout=5):
        params = list(params or [])
        params.insert(0, "token:" + self.secret)
        payload = json.dumps(
            {"jsonrpc": "2.0", "id": "wirectl-bt-test", "method": "aria2." + method, "params": params}
        ).encode()
        request = Request(
            f"http://127.0.0.1:{self.rpc_port}/jsonrpc",
            data=payload,
            headers={"Content-Type": "application/json"},
        )
        with urlopen(request, timeout=timeout) as response:
            result = json.load(response)
        if "error" in result:
            error = result["error"]
            raise RuntimeError(f"seed aria2 error {error.get('code')}: {error.get('message')}")
        return result.get("result")

    def wait_ready(self, timeout=20):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"seed aria2 exited with {self.process.returncode}")
            try:
                self.call("getVersion")
                return
            except Exception:
                time.sleep(0.2)
        raise RuntimeError(f"seed aria2 did not become ready; see {self.log_path}")

    def add(self, torrent):
        gid = self.call(
            "addTorrent",
            [base64.b64encode(torrent).decode(), [], {}],
        )
        return gid

    def items(self):
        items = []
        for method, params in (
            ("tellActive", []),
            ("tellWaiting", [0, 1000]),
            ("tellStopped", [0, 1000]),
        ):
            try:
                items.extend(self.call(method, params))
            except Exception:
                pass
        return items

    def wait_seeding(self, gid, payload_size, timeout=30):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for item in self.items():
                if item.get("gid") != gid:
                    continue
                completed = int(item.get("completedLength", "0"))
                if completed >= payload_size and item.get("status") in {"active", "complete"}:
                    return item
                if item.get("status") == "error":
                    raise RuntimeError("seed aria2: " + item.get("errorMessage", "unknown error"))
            time.sleep(0.2)
        raise RuntimeError(f"seed did not hash and share the fixture; see {self.log_path}")

    def close(self):
        if self.process.poll() is None:
            try:
                self.call("shutdown", timeout=3)
            except Exception:
                self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
        self.log_file.close()


class Profile:
    def __init__(self, name, state, downloads):
        self.name = name
        self.state = state
        self.downloads = downloads
        self.evidence_dir = state / "evidence"
        self.evidence_dir.mkdir(parents=True)
        self.running = False
        self.logical_id = None
        self.engine_id = None
        self.record = {
            "source": None,
            "state": str(state),
            "downloads": str(downloads),
            "steps": [],
            "status_files": {},
        }
        evidence["profiles"][name] = self.record


def cli(profile, *command, timeout=100):
    result = subprocess.run(
        [str(wirectl), "download", "--data-dir", str(profile.state), *command],
        env=base_env,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if result.returncode:
        raise RuntimeError(
            f"{profile.name} {' '.join(command)} failed ({result.returncode}): "
            f"{result.stderr.strip() or result.stdout.strip()}"
        )
    return result.stdout


def cli_input(profile, input_text, *command, timeout=100):
    result = subprocess.run(
        [str(wirectl), "download", "--data-dir", str(profile.state), *command],
        env=base_env,
        input=input_text,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if result.returncode:
        raise RuntimeError(
            f"{profile.name} {' '.join(command)} failed ({result.returncode}): "
            f"{result.stderr.strip() or result.stdout.strip()}"
        )
    return result.stdout


def list_status(profile):
    raw = cli(profile, "list", "--json")
    snapshot = json.loads(raw)
    (profile.evidence_dir / "status-latest.json").write_text(json.dumps(snapshot, indent=2) + "\n")
    return snapshot


def native_call(profile, method, params=None, timeout=5):
    config = json.loads((profile.state / "config.json").read_text())
    request_params = ["token:" + config["secret"]]
    request_params.extend(params or [])
    payload = json.dumps(
        {
            "jsonrpc": "2.0",
            "id": "wirectl-bt-test",
            "method": "aria2." + method,
            "params": request_params,
        }
    ).encode()
    request = Request(
        f"http://127.0.0.1:{config['aria2_port']}/jsonrpc",
        data=payload,
        headers={"Content-Type": "application/json"},
    )
    with urlopen(request, timeout=timeout) as response:
        result = json.load(response)
    if "error" in result:
        error = result["error"]
        raise RuntimeError(f"aria2 error {error.get('code')}: {error.get('message')}")
    return result.get("result")


def assert_no_native_pending(profile, stage):
    active = native_call(profile, "tellActive")
    waiting = native_call(profile, "tellWaiting", [0, 1000])
    profile.record[f"native_{stage}_active"] = len(active)
    profile.record[f"native_{stage}_waiting"] = len(waiting)
    if active or waiting:
        raise RuntimeError(
            f"native aria2 task survived {stage}: active={active!r} waiting={waiting!r}"
        )


def job_in(snapshot, logical_id):
    return next((job for job in snapshot.get("jobs", []) if job.get("id") == logical_id), None)


def save_stage(profile, stage, snapshot):
    safe = re.sub(r"[^a-zA-Z0-9_.-]+", "-", stage)
    path = profile.evidence_dir / f"{safe}.json"
    path.write_text(json.dumps(snapshot, indent=2) + "\n")
    profile.record["status_files"][stage] = str(path)


def wait_for(profile, predicate, stage, timeout):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            snapshot = list_status(profile)
            result = predicate(snapshot)
            if result:
                profile.record["steps"].append(stage)
                save_stage(profile, stage, snapshot)
                print("PASS", profile.name, stage, flush=True)
                return snapshot
        except Exception as error:
            last_error = error
        time.sleep(0.25)
    suffix = f"; last error: {last_error}" if last_error else ""
    raise RuntimeError(f"timed out waiting for {profile.name}: {stage}{suffix}")


def start_daemon(profile):
    errors = []
    for attempt in range(1, 4):
        try:
            cli(profile, "daemon", "start", timeout=130)
            profile.running = True
            return
        except Exception as error:
            errors.append(str(error))
            if "bind: invalid argument" in str(error):
                raise
            try:
                list_status(profile)
                profile.running = True
                return
            except Exception:
                if attempt < 3:
                    time.sleep(1)
    raise RuntimeError("daemon start failed after three attempts: " + " | ".join(errors))


def stop_daemon(profile):
    if not profile.running:
        return
    cli(profile, "daemon", "stop", timeout=60)
    profile.running = False


def prepare_profile(profile, tracker_url, ports):
    cli(profile, "init", "--downloads", str(profile.downloads))
    config_path = profile.state / "config.json"
    config = json.loads(config_path.read_text())
    config.update(
        {
            "server_list_urls": [],
            "trackers": [tracker_url],
            "max_downloads": 1,
            "download_limit": "256K",
            "seed_ratio": 0,
            "aria2_port": ports["aria2"],
            "amule_ec_port": ports["amule"],
            "bt_port": ports["bt"],
            "ed2k_port": ports["ed2k"],
            "kad_port": ports["kad"],
            "auth_proxy_port": ports["auth"],
        }
    )
    config_path.write_text(json.dumps(config, indent=2) + "\n")


def run_profile(profile, source, payload, fixture_name, timeout):
    profile.record["source"] = source
    start_daemon(profile)
    try:
        if args.search:
            query_type = "magnet" if profile.name == "magnet" else "torrent"
            snapshot = json.loads(cli(profile, "search", "--type", query_type, "--json", "--timeout", "15s", "owned fixture"))
            (profile.evidence_dir / "search.json").write_text(json.dumps(snapshot, indent=2))
            if snapshot["status"] != "partial" or len(snapshot["results"]) != 1:
                raise RuntimeError(f"search did not merge partial results: {snapshot!r}")
            result = snapshot["results"][0]
            if result.get("info_hash") != info_hash or result.get("size") != len(payload):
                raise RuntimeError(f"search changed hash/size: {result!r}")
            if sorted(result["sources"]) != ["one", "two"] or result["kind"] != query_type:
                raise RuntimeError(f"wrong merged sources or kind: {result!r}")
            if list_status(profile)["jobs"]:
                raise RuntimeError("search automatically downloaded a result")
            output = cli(profile, "search", "download", snapshot["id"], result["id"])
            profile.record["steps"].append("multi-source search merges hashes and preserves partial results; explicit result download")
            print("PASS", profile.name, "search result selected for download", flush=True)
        else:
            output = cli(profile, "add", "--detach", source)
        match = re.search(r"(?m)^([0-9a-f]{12})\s+aria2\s+", output)
        if not match:
            raise RuntimeError(f"could not parse logical job ID from add output: {output!r}")
        profile.logical_id = match.group(1)
        profile.record["logical_job_id"] = profile.logical_id
        snapshot = wait_for(
            profile,
            lambda current: job_in(current, profile.logical_id) is not None,
            "job accepted",
            20,
        )
        job = job_in(snapshot, profile.logical_id)
        if job.get("engine") != "aria2":
            raise RuntimeError(f"expected aria2 job, got {job!r}")

        def partial(current):
            item = job_in(current, profile.logical_id)
            if not item or item.get("status") not in {"active", "waiting"}:
                return False
            return (
                0 < item.get("progress", 0) < 100
                or item.get("completed", 0) > 0
            )

        snapshot = wait_for(profile, partial, "partial download", timeout)
        job = job_in(snapshot, profile.logical_id)
        profile.engine_id = job.get("engine_id")
        if not profile.engine_id:
            raise RuntimeError(f"missing aria2 engine ID at partial state: {job!r}")
        parent_id = job.get("metadata_id") or hashlib.sha256(
            (profile.logical_id + source).encode()
        ).hexdigest()[:16]
        profile.record["engine_id_at_pause"] = profile.engine_id
        profile.record["metadata_id"] = job.get("metadata_id") or parent_id
        profile.record["metadata_parent_id_expected"] = parent_id
        profile.record["partial_job"] = job

        cli(profile, "pause", profile.logical_id)
        snapshot = wait_for(
            profile,
            lambda current: (
                (item := job_in(current, profile.logical_id)) is not None
                and item.get("status") == "paused"
            ),
            "paused",
            20,
        )
        paused = job_in(snapshot, profile.logical_id)
        if paused.get("engine_id") != profile.engine_id:
            raise RuntimeError(f"engine ID changed during pause: {paused!r}")

        stop_daemon(profile)
        jobs_path = profile.state / "jobs.json"
        saved_jobs = json.loads(jobs_path.read_text())
        saved = next((job for job in saved_jobs["jobs"] if job["id"] == profile.logical_id), None)
        if not saved:
            raise RuntimeError(f"logical job {profile.logical_id} missing from jobs.json")
        if saved.get("engine_id") != profile.engine_id:
            raise RuntimeError(f"engine ID changed while stopping: {saved!r}")
        saved_parent_id = saved.get("metadata_id") or parent_id
        session_path = profile.state / "aria2/session.txt"
        session = session_path.read_text(errors="replace")
        if not session.strip():
            raise RuntimeError(f"unfinished task was not saved to aria2 session: {session_path}")
        profile.record["jobs_after_stop"] = saved_jobs
        profile.record["session_path"] = str(session_path)
        profile.record["session_bytes"] = len(session.encode())
        profile.record["session_contains_engine_id"] = profile.engine_id in session
        profile.record["session_contains_metadata_parent_id"] = saved_parent_id in session
        if profile.engine_id not in session and saved_parent_id not in session:
            raise RuntimeError(
                f"aria2 session omitted engine ID {profile.engine_id} "
                f"and metadata parent ID {saved_parent_id}"
            )

        start_daemon(profile)
        snapshot = wait_for(
            profile,
            lambda current: (
                (item := job_in(current, profile.logical_id)) is not None
                and item.get("status") == "paused"
            ),
            "restart preserves paused state",
            30,
        )
        resumed_pause = job_in(snapshot, profile.logical_id)
        if resumed_pause.get("metadata_id") and resumed_pause.get("metadata_id") != saved_parent_id:
            raise RuntimeError(f"metadata parent ID changed after restart: {resumed_pause!r}")
        profile.record["engine_id_after_restart"] = resumed_pause.get("engine_id")
        profile.record["logical_id_after_restart"] = resumed_pause.get("id")

        cli(profile, "resume", profile.logical_id)

        def finished(current):
            item = job_in(current, profile.logical_id)
            return (
                item
                and item.get("status") == "seeding"
                and item.get("name") == fixture_name
                and item.get("total") == len(payload)
                and item.get("completed") == len(payload)
                and item.get("progress", 0) >= 100
            )

        snapshot = wait_for(profile, finished, "resumed download complete", timeout)
        final_job = job_in(snapshot, profile.logical_id)
        output_path = profile.downloads / fixture_name
        if not output_path.is_file():
            raise RuntimeError(f"completed job has no output file: {output_path}")
        actual = output_path.read_bytes()
        if actual != payload:
            raise RuntimeError(
                f"download bytes differ: expected {len(payload)} bytes, got {len(actual)} bytes"
            )
        digest = hashlib.sha256(actual).hexdigest()
        profile.record["final_job"] = final_job
        profile.record["output_path"] = str(output_path)
        profile.record["output_bytes"] = len(actual)
        profile.record["sha256"] = digest
        profile.record["steps"].append("output bytes and SHA-256 match fixture")
        print("PASS", profile.name, "output bytes and SHA-256 match fixture", flush=True)

        stop_daemon(profile)
        start_daemon(profile)
        snapshot = wait_for(profile, finished, "completed seeding survives daemon restart", 30)
        seeded_after_restart = job_in(snapshot, profile.logical_id)
        profile.record["engine_id_after_seed_restart"] = seeded_after_restart.get("engine_id")
        if not output_path.is_file() or output_path.read_bytes() != payload:
            raise RuntimeError("completed file changed after seeding restart")
        profile.record["steps"].append("completed seeding remains byte-identical after restart")
        print("PASS", profile.name, "completed seeding remains byte-identical after restart", flush=True)

        cli(profile, "remove", profile.logical_id)
        snapshot = wait_for(
            profile,
            lambda current: (
                (item := job_in(current, profile.logical_id)) is not None
                and item.get("status") == "removed"
            ),
            "remove marks completed task removed",
            20,
        )
        removed = job_in(snapshot, profile.logical_id)
        profile.record["engine_id_at_remove"] = removed.get("engine_id")
        if not output_path.is_file() or output_path.read_bytes() != payload:
            raise RuntimeError("remove deleted or changed the completed file")
        profile.record["steps"].append("remove preserves completed output")
        print("PASS", profile.name, "remove preserves completed output", flush=True)
        session_after_remove = session_path.read_text(errors="replace")
        profile.record["session_bytes_after_remove"] = len(session_after_remove.encode())
        if session_after_remove.strip():
            raise RuntimeError(
                f"aria2 session retained a removed task before stop: {session_path}"
            )
        assert_no_native_pending(profile, "remove-before-stop")
        profile.record["steps"].append("remove clears aria2 session and pending native tasks")
        print("PASS", profile.name, "remove clears aria2 session and pending native tasks", flush=True)

        stop_daemon(profile)
        start_daemon(profile)
        snapshot = wait_for(
            profile,
            lambda current: (
                (item := job_in(current, profile.logical_id)) is not None
                and item.get("status") == "removed"
            ),
            "removed task stays removed after daemon restart",
            30,
        )
        removed_after_restart = job_in(snapshot, profile.logical_id)
        profile.record["engine_id_after_remove_restart"] = removed_after_restart.get("engine_id")
        session_after_restart = session_path.read_text(errors="replace")
        profile.record["session_bytes_after_remove_restart"] = len(session_after_restart.encode())
        if session_after_restart.strip():
            raise RuntimeError(
                f"aria2 session resurrected a removed task after restart: {session_path}"
            )
        assert_no_native_pending(profile, "remove-after-restart")
        if not output_path.is_file() or output_path.read_bytes() != payload:
            raise RuntimeError("removed task restart changed the completed file")
        profile.record["steps"].append("removed task does not resurrect after restart")
        print("PASS", profile.name, "removed task does not resurrect after restart", flush=True)
    finally:
        stop_daemon(profile)


seed = None
tracker = None
file_server = None
profiles = []
failure = None
try:
    payload = hashlib.sha256(b"wirectl local BitTorrent recovery fixture").digest() * (4 * 1024 * 1024 // 32)
    fixture_name = "wirectl-bt-recovery-fixture.bin"
    piece_length = 16 * 1024
    pieces = b"".join(
        hashlib.sha1(payload[offset : offset + piece_length]).digest()
        for offset in range(0, len(payload), piece_length)
    )
    torrent_name = fixture_name.encode()
    info = {
        b"length": len(payload),
        b"name": torrent_name,
        b"piece length": piece_length,
        b"pieces": pieces,
    }
    info_hash = hashlib.sha1(bencode(info)).hexdigest()
    seed_bt_port = free_port()
    seed_rpc_port = free_port()
    tracker = Tracker(seed_bt_port)
    tracker_url = tracker.url
    torrent = bencode({b"announce": tracker_url.encode(), b"info": info})
    torrent_path = work / "fixture.torrent"
    torrent_path.write_bytes(torrent)
    file_server = FixtureServer(torrent)
    sources = {
        "torrent": str(torrent_path),
        "http-torrent": file_server.url,
        "http-torrent-auth": file_server.private_url,
        "magnet": (
            "magnet:?xt=urn:btih:"
            + info_hash
            + "&dn="
            + quote(fixture_name)
            + "&tr="
            + quote(tracker_url, safe="")
        ),
    }
    file_server.feed = ('<?xml version="1.0"?><rss version="2.0" xmlns:nyaa="https://nyaa.si/xmlns/nyaa"><channel>'
                        '<title>Owned fixture index</title><item><title>' + fixture_name + '</title><link>' + escape(file_server.url) + '</link>'
                        '<description><![CDATA[<a href="' + escape(sources["magnet"]) + '">Magnet</a>]]></description>'
                        '<enclosure type="application/x-bittorrent" url="' + escape(file_server.url) + '"/>'
                        '<nyaa:infoHash>' + info_hash + '</nyaa:infoHash><nyaa:seeders>1</nyaa:seeders>'
                        '<nyaa:size>4 MiB</nyaa:size></item></channel></rss>')
    evidence["torrent"] = {
        "name": fixture_name,
        "bytes": len(payload),
        "piece_length": piece_length,
        "info_hash": info_hash,
        "torrent_path": str(torrent_path),
        "tracker": tracker_url,
        "http_torrent": file_server.url,
        "http_torrent_auth": file_server.private_url,
        "sources": sources,
    }

    seed_dir = work / "seed"
    seed_dir.mkdir()
    (seed_dir / fixture_name).write_bytes(payload)
    seed = Seed(aria2c, seed_dir, seed_rpc_port, seed_bt_port, tracker_url)
    seed.wait_ready()
    seed_gid = seed.add(torrent)
    seeded = seed.wait_seeding(seed_gid, len(payload))
    evidence["seed"] = {
        "gid": seed_gid,
        "bt_port": seed_bt_port,
        "rpc_port": seed_rpc_port,
        "status": seeded.get("status"),
        "completed_length": seeded.get("completedLength"),
        "log": str(seed.log_path),
    }
    print("PASS local aria2 seed hashed fixture", flush=True)

    profile_dirs = {
        "torrent": "t",
        "http-torrent": "h",
        "magnet": "m",
        "http-torrent-auth": "a",
    }
    profile_names = args.only or ("torrent", "http-torrent", "magnet", "http-torrent-auth")
    for name in profile_names:
        ports = distinct_ports(
            ("aria2", "amule", "bt", "ed2k", "kad", "auth")
        )
        profile_dir = work / profile_dirs[name]
        profile = Profile(name, profile_dir, profile_dir / "downloads")
        profile.state.parent.mkdir(parents=True, exist_ok=True)
        profile.downloads.mkdir(parents=True)
        prepare_profile(profile, tracker_url, ports)
        if args.search:
            search_sources = [{"id": source_id, "name": "Owned " + source_id, "type": "rss",
                               "url": file_server.origin + "/rss-" + source_id + "?q={query}", "enabled": True}
                              for source_id in ("one", "two", "offline")]
            source_path = profile.state / "search-sources.json"
            source_path.write_text(json.dumps(search_sources))
            source_path.chmod(0o600)
        profile.record["ports"] = ports
        profiles.append(profile)
        if name == "http-torrent-auth":
            session = {
                "origin": file_server.origin,
                "user_agent": "wirectl owned browser fixture",
                "cookies": [
                    {
                        "name": "session",
                        "value": "owned-browser-session",
                        "domain": "127.0.0.1",
                        "path": "/",
                        "httpOnly": True,
                    }
                ],
            }
            cli_input(profile, json.dumps(session), "login", "--import")
            profile.record["auth_origin"] = file_server.origin
            profile.record["auth_cookie_imported"] = True
        run_profile(profile, sources[name], payload, fixture_name, args.timeout)

    evidence["tracker_requests"] = tracker.requests
    evidence["file_server_requests"] = file_server.requests
    if len(tracker.requests) < len(profile_names):
        raise RuntimeError(f"tracker saw too few announces: {len(tracker.requests)}")
    if any(name in profile_names for name in ("http-torrent", "http-torrent-auth")) and not file_server.requests:
        raise RuntimeError("HTTP torrent source was never requested")
    if "http-torrent-auth" in profile_names:
        private_requests = [request for request in file_server.requests if request["private"]]
        if not private_requests or not all(request["authorized"] for request in private_requests):
            raise RuntimeError(
                f"authenticated HTTP torrent requests were not cookie-authorized: {private_requests}"
            )
    evidence["passed"] = True
    print("PASS all local torrent, HTTP torrent, and magnet recovery profiles", flush=True)
except Exception as error:
    failure = error
    evidence["error"] = f"{type(error).__name__}: {error}"
    traceback.print_exc()
finally:
    for profile in reversed(profiles):
        try:
            stop_daemon(profile)
        except Exception as error:
            evidence.setdefault("cleanup_errors", []).append(f"{profile.name}: {error}")
    if seed is not None:
        try:
            seed.close()
        except Exception as error:
            evidence.setdefault("cleanup_errors", []).append(f"seed: {error}")
    if file_server is not None:
        file_server.close()
        evidence["file_server_requests"] = file_server.requests
    if tracker is not None:
        tracker.close()
        evidence["tracker_requests"] = tracker.requests
    (work / "result.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print("Test evidence:", work, flush=True)

if failure is not None:
    raise SystemExit(1)
