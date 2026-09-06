#!/usr/bin/env python3
"""Package Dune, its pinned tmux executable and upstream notices together."""
import json
import pathlib
import tarfile

root = pathlib.Path(__file__).resolve().parent.parent
manifest = json.loads((root / 'third_party/tmux/manifest.json').read_text())
cache = root / '.tools/tmux' / manifest['version']
for target in manifest['archives']:
    output = root / 'bin' / f'dune-{target}.tar.gz'
    with tarfile.open(output, 'w:gz') as package:
        package.add(root / 'bin' / f'dune-{target}', arcname='dune')
        package.add(cache / target / 'tmux', arcname='tmux')
        package.add(cache / 'licenses', arcname='licenses')
        package.add(root / 'third_party/tmux/manifest.json', arcname='licenses/tmux-manifest.json')
    print(output)
