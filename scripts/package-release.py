#!/usr/bin/env python3
"""Package Dune, pinned tmux/ripgrep executables and their upstream notices."""
import hashlib
import json
import pathlib
import tarfile

root = pathlib.Path(__file__).resolve().parent.parent
manifest = json.loads((root / 'third_party/tmux/manifest.json').read_text())
cache = root / '.tools/tmux' / manifest['version']
rg_manifest = json.loads((root / 'third_party/ripgrep/manifest.json').read_text())
rg_cache = root / '.tools/ripgrep' / rg_manifest['version']
if set(manifest['archives']) != set(rg_manifest['archives']):
    raise RuntimeError('tmux and ripgrep release targets differ')
for target in manifest['archives']:
    output = root / 'bin' / f'dune-{target}.tar.gz'
    with tarfile.open(output, 'w:gz') as package:
        package.add(root / 'bin' / f'dune-{target}', arcname='dune')
        package.add(cache / target / 'tmux', arcname='tmux')
        package.add(rg_cache / target / 'rg', arcname='rg')
        package.add(cache / 'licenses', arcname='licenses')
        package.add(root / 'third_party/tmux/manifest.json', arcname='licenses/tmux-manifest.json')
        package.add(rg_cache / target / 'licenses', arcname='licenses/ripgrep')
        package.add(root / 'third_party/ripgrep/manifest.json', arcname='licenses/ripgrep-manifest.json')
    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    output.with_name(output.name + '.sha256').write_text(f'{digest}  {output.name}\n')
    print(output)
