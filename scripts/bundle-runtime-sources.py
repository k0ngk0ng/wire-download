"""Download exact Debian corresponding sources for bundled runtime libraries."""
import json
from pathlib import Path
import subprocess
import sys

manifest = json.loads(Path(sys.argv[1]).read_text())
destination = Path(sys.argv[2]).resolve()
destination.mkdir(parents=True, exist_ok=True)
packages = {entry['package'] for entry in manifest} | {'ca-certificates', 'libssl3'}
sources = set()
for package in sorted(packages):
    source = subprocess.check_output(
        ['dpkg-query', '-W', '-f=${source:Package}=${source:Version}', package],
        text=True).strip()
    if '=' not in source or source.endswith('='):
        raise SystemExit('Cannot determine corresponding source for ' + package)
    sources.add(source)
for source in sorted(sources):
    subprocess.run(['apt-get', 'source', '--download-only', source],
                   cwd=destination, check=True)
(destination / 'SOURCES.txt').write_text('\n'.join(sorted(sources)) + '\n')
