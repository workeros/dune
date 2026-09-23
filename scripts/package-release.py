#!/usr/bin/env python3
"""Build complete release archives and the host's immutable upgrade catalog."""
import argparse
import gzip
import hashlib
import json
import pathlib
import tarfile
from urllib.parse import urlsplit


def canonical_json(value):
    # Matches Manifest.Digest's Go JSON encoding, including HTML and JS escapes.
    text = json.dumps(value, ensure_ascii=False, separators=(',', ':'))
    for char in '<>&\u2028\u2029':
        text = text.replace(char, f'\\u{ord(char):04x}')
    return text.encode('utf-8')


def package(root, output_dir, target, release_id, base_url, tmux, ripgrep):
    tmux_cache = root / '.tools/tmux' / tmux['version']
    rg_cache = root / '.tools/ripgrep' / ripgrep['version']
    files = {
        'dune': output_dir / f'dune-{target}',
        'tmux': tmux_cache / target / 'tmux',
        'rg': rg_cache / target / 'rg',
        'licenses/tmux-manifest.json': root / 'third_party/tmux/manifest.json',
        'licenses/ripgrep-manifest.json': root / 'third_party/ripgrep/manifest.json',
    }
    for directory, prefix in [(tmux_cache / 'licenses', 'licenses'),
                              (rg_cache / target / 'licenses', 'licenses/ripgrep')]:
        for source in sorted(directory.rglob('*')):
            if source.is_symlink():
                raise ValueError(f'symlink in release notices: {source}')
            if source.is_file():
                name = f'{prefix}/{source.relative_to(directory).as_posix()}'
                if name in files:
                    raise ValueError(f'duplicate release member: {name}')
                files[name] = source
    components = []
    output = output_dir / f'dune-{target}.tar.gz'
    with output.open('wb') as raw, gzip.GzipFile(fileobj=raw, mode='wb', filename='', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for name, source in sorted(files.items()):
                if source.is_symlink() or not source.is_file():
                    raise ValueError(f'ordinary release file required: {source}')
                body = source.read_bytes()
                mode = 0o600 if name.startswith('licenses/') else 0o700
                components.append({'path': name, 'sha256': hashlib.sha256(body).hexdigest(),
                                   'bytes': len(body), 'mode': mode})
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode = len(body), mode
                with source.open('rb') as content:
                    archive.addfile(entry, content)
    archive_sha = hashlib.sha256(output.read_bytes()).hexdigest()
    output.with_name(output.name + '.sha256').write_text(f'{archive_sha}  {output.name}\n')
    os_name, arch = target.split('-')
    manifest = {'id': release_id, 'platform': {'os': os_name, 'arch': arch},
                'archive_url': base_url.rstrip('/') + '/' + output.name,
                'archive_sha256': archive_sha,
                'state_contract': hashlib.sha256((root / 'internal/statecontract/contract.txt').read_bytes()).hexdigest(),
                'components': components}
    manifest_file = output_dir / f'dune-{target}.manifest.json'
    manifest_file.write_bytes(canonical_json(manifest) + b'\n')
    reference = {'id': release_id, 'manifest_sha256': hashlib.sha256(canonical_json(manifest)).hexdigest()}
    (output_dir / f'dune-{target}.ref.json').write_bytes(canonical_json(reference) + b'\n')
    print(output)
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--release-id', required=True, help='immutable release ID')
    parser.add_argument('--base-url', required=True, help='immutable HTTP(S) directory serving these archives')
    parser.add_argument('--platform', action='append', help='limit packaging to a built OS-ARCH target')
    parser.add_argument('--output-dir', type=pathlib.Path, help='directory containing dune-OS-ARCH binaries; defaults to bin')
    args = parser.parse_args()
    endpoint = urlsplit(args.base_url)
    if endpoint.scheme not in ('http', 'https') or not endpoint.netloc or endpoint.username or endpoint.query or endpoint.fragment:
        parser.error('--base-url must be an HTTP(S) directory without credentials, query or fragment')
    if not args.release_id or len(args.release_id) > 128 or any(c in args.release_id for c in '\r\n\0'):
        parser.error('invalid release ID')
    root = pathlib.Path(__file__).resolve().parent.parent
    output_dir = args.output_dir or root / 'bin'
    tmux = json.loads((root / 'third_party/tmux/manifest.json').read_text())
    ripgrep = json.loads((root / 'third_party/ripgrep/manifest.json').read_text())
    if set(tmux['archives']) != set(ripgrep['archives']):
        raise ValueError('tmux and ripgrep release targets differ')
    targets = args.platform or list(tmux['archives'])
    if not set(targets) <= set(tmux['archives']) or len(set(targets)) != len(targets):
        parser.error('unknown or duplicate platform')
    manifests = [package(root, output_dir, target, args.release_id, args.base_url, tmux, ripgrep) for target in targets]
    (output_dir / 'upgrade-catalog.json').write_bytes(canonical_json(manifests) + b'\n')


if __name__ == '__main__':
    main()
