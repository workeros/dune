#!/usr/bin/env python3
"""Fetch pinned ripgrep executables and notices after SHA-256 verification."""

import argparse
import concurrent.futures
import hashlib
import json
import pathlib
import platform
import shutil
import sys
import tarfile
import urllib.request


ROOT = pathlib.Path(__file__).resolve().parent.parent
MANIFEST = json.loads((ROOT / "third_party/ripgrep/manifest.json").read_text())
VERSION = MANIFEST["version"]
CACHE = ROOT / ".tools/ripgrep" / VERSION
BASE_URL = f"{MANIFEST['repository']}/releases/download/{VERSION}"


def write_atomic(destination, content, mode):
    temporary = destination.with_suffix(".new")
    temporary.write_bytes(content)
    temporary.chmod(mode)
    temporary.replace(destination)


def fetch(target):
    upstream, digest = MANIFEST["archives"][target]
    name = f"ripgrep-{VERSION}-{upstream}"
    archive_path = CACHE / f"{name}.tar.gz"
    if not archive_path.exists() or hashlib.sha256(archive_path.read_bytes()).hexdigest() != digest:
        with urllib.request.urlopen(f"{BASE_URL}/{name}.tar.gz", timeout=120) as response:
            content = response.read()
        if hashlib.sha256(content).hexdigest() != digest:
            raise RuntimeError(f"ripgrep checksum mismatch: {target}")
        write_atomic(archive_path, content, 0o644)

    destination = CACHE / target
    notices = destination / "licenses"
    notices.mkdir(parents=True, exist_ok=True)
    pcre2_notice = (ROOT / "third_party/ripgrep/PCRE2-LICENCE.md").read_bytes()
    if hashlib.sha256(pcre2_notice).hexdigest() != MANIFEST["pcre2"]["license_sha256"]:
        raise RuntimeError("PCRE2 license checksum mismatch")
    write_atomic(notices / "PCRE2-LICENCE.md", pcre2_notice, 0o644)
    # Extract only named regular files. Never apply archive paths, symlinks or
    # permissions to the destination tree.
    with tarfile.open(archive_path, "r:gz") as archive:
        for filename in ["rg", *MANIFEST["license_files"]]:
            member = archive.getmember(f"{name}/{filename}")
            if not member.isfile():
                raise RuntimeError(f"ripgrep archive entry is not a regular file: {filename}")
            with archive.extractfile(member) as source:
                content = source.read()
            output = destination / "rg" if filename == "rg" else notices / filename
            write_atomic(output, content, 0o755 if filename == "rg" else 0o644)
    print(target, destination / "rg", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--all", action="store_true", help="fetch all four release targets")
    args = parser.parse_args()
    system = {"darwin": "darwin", "linux": "linux"}.get(sys.platform)
    architecture = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
    host = f"{system}-{architecture}"
    if not args.all and host not in MANIFEST["archives"]:
        parser.error(f"unsupported ripgrep host: {sys.platform}/{platform.machine()}")
    CACHE.mkdir(parents=True, exist_ok=True)
    targets = list(MANIFEST["archives"]) if args.all else [host]
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(fetch, targets))
    if host in targets:
        (ROOT / "bin").mkdir(exist_ok=True)
        temporary = ROOT / "bin/rg.new"
        shutil.copy2(CACHE / host / "rg", temporary)
        temporary.replace(ROOT / "bin/rg")


if __name__ == "__main__":
    main()
