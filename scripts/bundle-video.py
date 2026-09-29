#!/usr/bin/env python3
"""Build and stage pinned website-video tools; all build/cache writes stay in repo."""
import hashlib
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile
import zipfile

root = Path(__file__).resolve().parent.parent
cache = root / '.cache/video'
cache.mkdir(parents=True, exist_ok=True)
destination = Path(sys.argv[1]).resolve()
bin_dir = destination / 'bin'
licenses = destination / 'licenses'
bin_dir.mkdir(parents=True, exist_ok=True)
licenses.mkdir(parents=True, exist_ok=True)
env = {**os.environ, 'TMPDIR': str(root / '.cache/tmp'), 'DENO_DIR': str(cache / 'deno-cache')}
Path(env['TMPDIR']).mkdir(parents=True, exist_ok=True)
target = (sys.platform, platform.machine())
yt_assets = {
    ('darwin', 'arm64'): ('yt-dlp_macos', '0f192b7ec147ab6288885d6351d9ab67367640029b4377576ef46dd79cf7b202'),
    ('linux', 'x86_64'): ('yt-dlp_linux', '58162f9bfdc27458ea47bfcb311cf47028f17d8154a8bf7d689861d46399230a'),
    ('linux', 'aarch64'): ('yt-dlp_linux_aarch64', 'b16e4dab368a816cd05d477d698a605a6ae87ccee1c8ffd38fa21d7254141fcc'),
}
deno_assets = {
    ('darwin', 'arm64'): ('deno-aarch64-apple-darwin.zip', '5cd46d6268f6f78f5d88bdc7159d20bd44cdaa4b3303474839f87ec6fe7ae25c'),
    ('linux', 'x86_64'): ('deno-x86_64-unknown-linux-gnu.zip', 'c6527f24f4b16031d3ae4fa9f658d5f11534c8d84ce7dc8502420280919c3490'),
    ('linux', 'aarch64'): ('deno-aarch64-unknown-linux-gnu.zip', 'c832298b1ad4422481334855f6003e0f54145762c5a134f20a489511d2f65bbf'),
}
if target not in yt_assets:
    raise SystemExit(f'Unsupported video engine target: {target}')

def fetch(url, name, sha):
    path = cache / name
    archived = root.parent / 'upstream' / name
    if not path.exists() and archived.is_file():
        shutil.copy2(archived, path)
    if not path.exists():
        partial = path.with_suffix(path.suffix + '.partial')
        subprocess.run(['curl', '-fL', '--retry', '3', '--connect-timeout', '20', '--max-time', '300', url, '-o', str(partial)], check=True)
        partial.rename(path)
    if hashlib.sha256(path.read_bytes()).hexdigest() != sha:
        raise SystemExit(f'Checksum mismatch: {path}')
    return path

asset, sha = yt_assets[target]
yt = fetch(f'https://github.com/yt-dlp/yt-dlp/releases/download/2026.08.19/{asset}', asset, sha)
(bin_dir / 'yt-dlp').unlink(missing_ok=True)
shutil.copy2(yt, bin_dir / 'yt-dlp')
(bin_dir / 'yt-dlp').chmod(0o755)
asset, sha = deno_assets[target]
deno = fetch(f'https://github.com/denoland/deno/releases/download/v2.9.7/{asset}', asset, sha)
(bin_dir / 'deno').unlink(missing_ok=True)
with zipfile.ZipFile(deno) as archive:
    (bin_dir / 'deno').write_bytes(archive.read('deno'))
(bin_dir / 'deno').chmod(0o755)

ffmpeg_version = '9.0.2'
source = fetch(f'https://ffmpeg.org/releases/ffmpeg-{ffmpeg_version}.tar.xz', f'ffmpeg-{ffmpeg_version}.tar.xz', '8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e')
src = cache / f'ffmpeg-{ffmpeg_version}'
if not src.exists():
    with tarfile.open(source) as archive:
        # Debian 12's Python 3.11.2 predates tarfile's filter argument.
        # This pinned source archive needs only ordinary files/directories;
        # validate every member before any extraction, including its root.
        for member in archive.getmembers():
            target_path = (cache / member.name).resolve()
            if not target_path.is_relative_to(src) or not (member.isdir() or member.isfile()):
                raise SystemExit(f'Unsafe FFmpeg archive member: {member.name}')
        archive.extractall(cache)
build = cache / ('ffmpeg-build-' + '-'.join(target))
build.mkdir(exist_ok=True)
if not (build / 'ffmpeg').exists():
    flags = ['--disable-autodetect', '--disable-shared', '--enable-static', '--disable-doc', '--disable-debug', '--disable-ffplay', '--disable-x86asm', '--disable-network']
    if sys.platform == 'darwin':
        env['MACOSX_DEPLOYMENT_TARGET'] = '13.0'
        flags += ['--extra-cflags=-mmacosx-version-min=13.0', '--extra-ldflags=-mmacosx-version-min=13.0']
    with (cache / 'ffmpeg-build.log').open('w') as log:
        subprocess.run([str(src / 'configure'), *flags], cwd=build, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
        subprocess.run(['make', '-j' + os.environ.get('JOBS', '4')], cwd=build, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
for name in ('ffmpeg', 'ffprobe'):
    (bin_dir / name).unlink(missing_ok=True)
    shutil.copy2(build / name, bin_dir / name)
    subprocess.run(['strip', str(bin_dir / name)], check=True)
    if sys.platform == 'darwin':
        subprocess.run(['codesign', '--force', '--sign', '-', str(bin_dir / name)], check=True)
    deps = subprocess.check_output(['otool', '-L', str(bin_dir / name)] if sys.platform == 'darwin' else ['ldd', str(bin_dir / name)], text=True)
    if sys.platform == 'darwin':
        if any(not line.strip().startswith(('/usr/lib/', '/System/Library/')) for line in deps.splitlines()[1:]):
            raise SystemExit(f'Non-system FFmpeg dependency: {deps}')
    elif 'not found' in deps:
        raise SystemExit(deps)
shutil.copy2(src / 'COPYING.LGPLv2.1', licenses / 'ffmpeg-LGPL-2.1.txt')
shutil.copy2(src / 'LICENSE.md', licenses / 'ffmpeg-build-license.md')
ytsrc = fetch('https://github.com/yt-dlp/yt-dlp/releases/download/2026.08.19/yt-dlp.tar.gz', 'yt-dlp-2026.08.19.tar.gz', '072aad4f2a7604e92155f61a275a4752dc64046c8f6d90df3710525d94cd37c1')
with tarfile.open(ytsrc) as archive:
    for name in ('LICENSE',):
        member = next(m for m in archive.getmembers() if m.name.removeprefix('./').split('/')[-1] == name and len(m.name.removeprefix('./').split('/')) <= 2)
        (licenses / ('yt-dlp-' + name)).write_bytes(archive.extractfile(member).read())
third = fetch('https://raw.githubusercontent.com/yt-dlp/yt-dlp/2026.08.19/THIRD_PARTY_LICENSES.txt', 'yt-dlp-THIRD_PARTY_LICENSES.txt', '472aefe951c7db35e1657c1d13fd337140511ed6f2b329205105ad441c5a02b7')
shutil.copy2(third, licenses / third.name)
if sys.platform.startswith('linux'):
    # Deno can use the C++ runtime already shipped for aMule. Keep its
    # loader rooted in the installation, independent of package-manager PATH.
    subprocess.run(['patchelf', '--set-rpath', '$ORIGIN/../lib', str(bin_dir / 'deno')], check=True)
deno_license = fetch('https://raw.githubusercontent.com/denoland/deno/v2.9.7/LICENSE.md', 'deno-LICENSE.md', 'f62497fffecc0852960c8d3e6934b9db86d16396e9b604072e923892cae3a588')
shutil.copy2(deno_license, licenses / deno_license.name)
for name, args, expected in [('yt-dlp', ['--version'], '2026.08.19'), ('deno', ['--version'], 'deno 2.9.7'), ('ffmpeg', ['-version'], 'ffmpeg version 9.0.2'), ('ffprobe', ['-version'], 'ffprobe version 9.0.2')]:
    result = subprocess.run([str(bin_dir / name), *args], text=True, capture_output=True, env={**env, 'PATH': '/usr/bin:/bin'}, timeout=120, check=True)
    if not result.stdout.startswith(expected):
        raise SystemExit(f'Unexpected {name} version: {result.stdout}')
print('Verified video engine bundle:', destination)
