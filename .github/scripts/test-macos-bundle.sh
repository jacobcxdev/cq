#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
  echo "macOS bundle fixtures require macOS" >&2
  exit 69
fi
script_directory=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
python3 - "$BASH" "$script_directory/build-macos-bundle.sh" <<'PY'
import hashlib
import io
import os
from pathlib import Path
import stat
import subprocess
import sys
import tarfile
import tempfile

shell, builder = sys.argv[1:]

def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()

def snapshot(root):
    result = {}
    for path in [root, *root.rglob("*")]:
        mode = path.lstat().st_mode
        kind = stat.S_IFMT(mode)
        value = os.readlink(path) if path.is_symlink() else digest(path) if path.is_file() else None
        result[str(path.relative_to(root))] = (kind, stat.S_IMODE(mode), value)
    return result

def write_archive(path, entries):
    with tarfile.open(path, "w:gz") as archive:
        for name, kind, data, mode in entries:
            entry = tarfile.TarInfo(name)
            entry.type, entry.mode, entry.mtime = kind, mode, 1_700_000_000
            if kind == tarfile.REGTYPE:
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
            else:
                entry.linkname = data.decode()
                archive.addfile(entry)

with tempfile.TemporaryDirectory(prefix="cq-macos-bundle-test.") as directory:
    root = Path(directory)
    app = root / "verified native" / "CQNetwork.app"
    files = {
        "Contents/Info.plist": (b"synthetic host metadata", 0o644),
        "Contents/MacOS/CQNetwork": (b"host\x00signed bytes", 0o755),
        "Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist": (b"synthetic provider metadata", 0o644),
        "Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy": (b"provider\x00signed bytes", 0o751),
        "Contents/_CodeSignature/CodeResources": (b"signature resources", 0o644),
        "Contents/CodeResources": (b"stapled ticket", 0o644),
        "Contents/Resources/colour with spaces": (b"resource bytes", 0o640),
    }
    for name, (body, mode) in files.items():
        path = app / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(body)
        path.chmod(mode)
    (app / "Contents/Resources/current").symlink_to("colour with spaces")
    cli = root / "core archive.tar.gz"
    executable = b"#!/bin/sh\nexit 0\n"
    regular = ("cq", tarfile.REGTYPE, executable, 0o751)
    write_archive(cli, [regular])
    original_cli, original_app = digest(cli), snapshot(app)
    output = root / "combined archive.tar.gz"

    def build(archive=cli, native=app, destination=output, succeeds=True):
        result = subprocess.run([shell, builder, str(archive), str(native), str(destination)], capture_output=True, text=True)
        if (result.returncode == 0) != succeeds:
            raise AssertionError(f"unexpected builder result {result.returncode}: {result.stderr}")
        assert digest(cli) == original_cli, "CLI input changed"
        assert snapshot(app) == original_app, "native input changed"
        assert not list(root.glob(".cq-macos-bundle.*")), "temporary build directory leaked"

    build()
    with tarfile.open(output, "r:gz") as archive:
        members = archive.getmembers()
        assert {member.name.split("/", 1)[0] for member in members} == {"cq", "CQNetwork.app"}
        cq = archive.getmember("cq")
        assert cq.mode == 0o751 and archive.extractfile(cq).read() == executable
    unpacked = root / "unpacked"
    unpacked.mkdir()
    subprocess.run(["/usr/bin/tar", "-xpf", str(output), "-C", str(unpacked)], check=True)
    assert snapshot(unpacked / "CQNetwork.app") == original_app
    build()  # Repeated assembly atomically replaces only the requested output.
    first_output = digest(output)

    invalid_archives = {
        "empty": [],
        "traversal": [("../cq", tarfile.REGTYPE, executable, 0o755)],
        "absolute": [("/cq", tarfile.REGTYPE, executable, 0o755)],
        "duplicate": [regular, regular],
        "extra": [regular, ("unexpected", tarfile.REGTYPE, b"extra", 0o644)],
        "symlink": [("cq", tarfile.SYMTYPE, b"/outside", 0o777)],
        "hardlink": [("cq", tarfile.LNKTYPE, b"other", 0o755)],
        "directory": [("cq", tarfile.DIRTYPE, b"", 0o755)],
        "not-executable": [("cq", tarfile.REGTYPE, executable, 0o644)],
        "setuid": [("cq", tarfile.REGTYPE, executable, 0o4755)],
    }
    for name, entries in invalid_archives.items():
        candidate = root / f"{name}.tar.gz"
        write_archive(candidate, entries)
        build(archive=candidate, succeeds=False)
        assert digest(output) == first_output, "failed build replaced existing output"
    corrupt = root / "corrupt.tar.gz"
    corrupt.write_bytes(b"not an archive")
    build(archive=corrupt, succeeds=False)
    build(archive=root / "missing.tar.gz", succeeds=False)
    build(native=root / "missing.app", succeeds=False)
    build(native=root, succeeds=False)
    build(destination=cli, succeeds=False)
    build(destination=app / "combined.tar.gz", succeeds=False)
    build(destination=root / "missing" / "bundle.tar.gz", succeeds=False)
    alias = root / "native-link.app"
    alias.symlink_to(app, target_is_directory=True)
    build(native=alias, succeeds=False)
    output_link = root / "output-link.tar.gz"
    output_link.symlink_to(output)
    build(destination=output_link, succeeds=False)
    assert digest(output) == first_output

    escaping = app / "Contents/Resources/escape"
    escaping.symlink_to("../../../outside")
    original_app = snapshot(app)
    build(succeeds=False)
    escaping.unlink()
    original_app = snapshot(app)
    special = app / "Contents/Resources/pipe"
    os.mkfifo(special)
    original_app = snapshot(app)
    build(succeeds=False)
    special.unlink()
    original_app = snapshot(app)
    print("macOS bundle assembly, preservation, atomic output and invalid-input fixtures passed")
PY
