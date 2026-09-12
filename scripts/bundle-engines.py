"""Stage native engines and verify their runtime dependency closure."""
import argparse
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys


def output(*args):
    return subprocess.check_output(args, text=True)


parser = argparse.ArgumentParser()
parser.add_argument('destination', type=Path)
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
destination = args.destination.resolve()
binary_dir = destination / 'bin'
binary_dir.mkdir(parents=True, exist_ok=True)
sources = [root / '.cache/engines/aria2-prefix/bin/aria2c']
sources += [root / '.cache/amule-build/prefix/bin' / name for name in ('amuled', 'amulecmd')]
for source in sources:
    if not source.is_file():
        raise SystemExit(f'Missing {source}; run sh scripts/build-engines.sh')
    target = binary_dir / source.name
    shutil.copy2(source, target)
    subprocess.run(['strip', str(target)], check=True)

if sys.platform == 'darwin':
    for target in binary_dir.iterdir():
        load_commands = output('otool', '-l', str(target))
        versions = re.findall(r'^\s*minos\s+([\d.]+)', load_commands, re.MULTILINE)
        if not versions:
            raise SystemExit(f'Missing macOS deployment target: {target.name}')
        for version in versions:
            if tuple(map(int, version.split('.'))) > (13, 0, 0):
                raise SystemExit(f'{target.name} requires macOS {version}; rebuild for macOS 13.0')
        for line in output('otool', '-L', str(target)).splitlines()[1:]:
            dependency = line.strip().split(' (')[0]
            if not dependency.startswith(('/usr/lib/', '/System/Library/')):
                raise SystemExit(f'Non-system dependency remains: {target.name}: {dependency}')
        subprocess.run(['codesign', '--force', '--sign', '-', str(target)], check=True)
elif sys.platform.startswith('linux'):
    if not shutil.which('patchelf'):
        raise SystemExit('Build tool patchelf is required to stage Linux runtime libraries')
    lib_dir = destination / 'lib'
    lib_dir.mkdir(exist_ok=True)
    system_names = ('libc.so.', 'libm.so.', 'libdl.so.', 'libpthread.so.', 'librt.so.', 'libresolv.so.', 'libutil.so.', 'ld-linux')
    queue = list(binary_dir.iterdir())
    seen = set()
    runtime_manifest = []
    runtime_licenses = destination / 'licenses'
    runtime_licenses.mkdir(exist_ok=True)
    # OpenSSL 3 loads this provider dynamically; ldd alone cannot discover it.
    modules = Path(output('openssl', 'version', '-m').split('"')[1])
    provider_dir = lib_dir / 'ossl-modules'
    provider_dir.mkdir()
    provider = provider_dir / 'legacy.so'
    shutil.copy2(modules / 'legacy.so', provider)
    queue.append(provider)
    cert_dir = destination / 'certs'
    cert_dir.mkdir()
    shutil.copy2('/etc/ssl/certs/ca-certificates.crt', cert_dir / 'ca-certificates.crt')
    shutil.copy2('/usr/share/doc/ca-certificates/copyright', runtime_licenses / 'ca-certificates.copyright')
    while queue:
        binary = queue.pop()
        dependencies = output('ldd', str(binary))
        if 'not found' in dependencies:
            raise SystemExit(dependencies)
        for line in dependencies.splitlines():
            if '=>' not in line:
                continue
            name, value = line.split('=>', 1)
            name = name.strip()
            if name.startswith(system_names) or name in seen:
                continue
            source = Path(value.strip().split()[0])
            if not source.is_absolute() or not source.is_file():
                raise SystemExit(f'Unresolved library: {line}')
            seen.add(name)
            target = lib_dir / name
            shutil.copy2(source.resolve(), target)
            # Preserve the actual runtime library provenance and notices in
            # the binary bundle, independently of the larger source archive.
            package = None
            candidates = [str(source), str(source.resolve())]
            candidates += [p.replace('/usr/lib/', '/lib/', 1) for p in candidates]
            for candidate in candidates:
                query = subprocess.run(['dpkg-query', '-S', candidate], text=True,
                                       capture_output=True)
                if query.returncode == 0:
                    package = query.stdout.splitlines()[0].rsplit(': ', 1)[0]
                    break
            if package is None:
                raise SystemExit(f'Cannot identify runtime library package: {source}')
            notice = Path('/usr/share/doc') / package.split(':')[0] / 'copyright'
            if not notice.is_file():
                raise SystemExit(f'Missing runtime license notice: {notice}')
            shutil.copy2(notice, runtime_licenses / (name + '.copyright'))
            version = output('dpkg-query', '-W', '-f=${Version}', package).strip()
            runtime_manifest.append({'library': name, 'package': package, 'version': version})
            queue.append(target)
    for target in lib_dir.iterdir():
        if target.is_file():
            subprocess.run(['patchelf', '--set-rpath', '$ORIGIN', str(target)], check=True)
    subprocess.run(['patchelf', '--set-rpath', '$ORIGIN/..', str(provider)], check=True)
    for target in binary_dir.iterdir():
        subprocess.run(['patchelf', '--set-rpath', '$ORIGIN/../lib', str(target)], check=True)
    (destination / 'runtime-libraries.json').write_text(json.dumps(runtime_manifest, indent=2) + '\n')
else:
    raise SystemExit('Only macOS and Linux are supported')

for target in binary_dir.iterdir():
    engine_env = {**os.environ, 'PATH': '/usr/bin:/bin'}
    if sys.platform.startswith('linux'):
        engine_env['OPENSSL_MODULES'] = str(provider_dir)
    result = subprocess.run([str(target), '--version'], text=True, capture_output=True,
                            env=engine_env, timeout=10)
    # wxApp exits with -1 after its version-only OnInit path, including on a
    # successful version print. Check both the known exit code and identity.
    expected = {'aria2c': ('aria2 version 1.37.0', (0,)),
                'amuled': ('aMuleD 2.3.3', (0, 255)),
                'amulecmd': ('amulecmd 2.3.3', (0, 255))}
    version, codes = expected[target.name]
    if result.returncode not in codes or not result.stdout.startswith(version):
        raise SystemExit(f'{target.name} failed version check: {result.returncode}: '
                         f'{result.stdout}{result.stderr}')
print('Verified engine bundle:', destination)
