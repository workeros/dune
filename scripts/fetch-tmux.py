#!/usr/bin/env python3
"""Fetch pinned upstream tmux distributions; verify before extracting executable assets."""
import concurrent.futures, hashlib, io, json, os, pathlib, platform, sys, tarfile, urllib.request
ROOT = pathlib.Path(__file__).resolve().parent.parent
manifest = json.loads((ROOT/'third_party/tmux/manifest.json').read_text())
version = manifest['version']
base = f'https://github.com/tmux/tmux-builds/releases/download/v{version}/'
cache = ROOT/'.tools/tmux'/version
cache.mkdir(parents=True, exist_ok=True)

def archive(name, digest):
    path = cache/name
    if not path.exists() or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
        with urllib.request.urlopen(base+name, timeout=120) as r: data = r.read()
        if hashlib.sha256(data).hexdigest() != digest: raise RuntimeError('checksum mismatch: '+name)
        temp = path.with_suffix('.download'); temp.write_bytes(data); temp.replace(path)
    return tarfile.open(path, 'r:gz')

def fetch(target):
    upstream, digest = manifest['archives'][target]
    dest = cache/target/'tmux'; dest.parent.mkdir(exist_ok=True)
    with archive(f'tmux-{version}-{upstream}.tar.gz', digest) as tar:
        members = [m for m in tar.getmembers() if m.isfile() and pathlib.PurePosixPath(m.name).name == 'tmux']
        if len(members) != 1: raise RuntimeError('expected one tmux executable')
        temp = dest.with_suffix(".new"); temp.write_bytes(tar.extractfile(members[0]).read()); temp.chmod(0o755); temp.replace(dest)
    print(target, dest, flush=True)

host = ('darwin' if sys.platform == 'darwin' else 'linux') + '-' + ('arm64' if platform.machine() in ('arm64','aarch64') else 'amd64')
targets = list(manifest['archives']) if '--all' in sys.argv else [host]
with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool: list(pool.map(fetch,targets))
licenses = cache/'licenses'; licenses.mkdir(exist_ok=True)
with archive('LICENSES.tar.gz',manifest['licenses_sha256']) as tar:
    for m in tar.getmembers():
        if not m.isfile(): continue
        parts=pathlib.PurePosixPath(m.name)
        if parts.is_absolute() or '..' in parts.parts: raise RuntimeError('unsafe license archive')
        dest=licenses/parts; dest.parent.mkdir(parents=True,exist_ok=True);dest.write_bytes(tar.extractfile(m).read())
if host in targets:
    import shutil
    (ROOT/'bin').mkdir(exist_ok=True);shutil.copy2(cache/host/'tmux',ROOT/'bin/tmux.new');(ROOT/'bin/tmux.new').replace(ROOT/'bin/tmux')
