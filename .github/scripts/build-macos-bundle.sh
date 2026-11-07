#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 || "$(uname -s)" != Darwin ]]; then
  echo "usage (macOS): build-macos-bundle.sh CLI_ARCHIVE VERIFIED_NATIVE_APP_DIRECTORY OUTPUT_TAR_GZ" >&2
  exit 64
fi

# The caller verifies native source identity, signatures, notarisation and version.
# This step only combines those approved inputs; existing CLI archives stay intact.
python3 - "$@" <<'PY'
import os
from pathlib import Path
import posixpath
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile

archive, app, output = map(Path, sys.argv[1:])

def require(condition, message):
    if not condition:
        raise ValueError(message)

try:
    require(archive.is_file() and not archive.is_symlink(), "CLI archive must be a regular file")
    require(archive.stat().st_size <= 512 << 20, "CLI archive exceeds size limit")
    require(app.is_dir() and not app.is_symlink(), "native app must be a real directory")
    required = [
        "Contents/Info.plist",
        "Contents/MacOS/CQNetwork",
        "Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist",
        "Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy",
    ]
    for name in required:
        path = app / name
        require(path.is_file() and not path.is_symlink(), "native app is missing a required regular file")
        if "/MacOS/" in name:
            require(path.stat().st_mode & stat.S_IXUSR, "native executable lacks owner execute permission")
    for directory, directories, files in os.walk(app, followlinks=False):
        for name in directories + files:
            path = Path(directory) / name
            mode = path.lstat().st_mode
            require(stat.S_ISREG(mode) or stat.S_ISDIR(mode) or stat.S_ISLNK(mode), "native app contains a special file")
            if stat.S_ISLNK(mode):
                target = os.readlink(path)
                relative = path.relative_to(app)
                resolved = posixpath.normpath(posixpath.join(str(relative.parent), target))
                require(not posixpath.isabs(target) and resolved != ".." and not resolved.startswith("../"), "native app contains an escaping symlink")
    require(output.parent.is_dir(), "output directory does not exist")
    require(not output.is_symlink() and (not output.exists() or output.is_file()), "output must be a regular file path")
    output = output.parent.resolve() / output.name
    require(output != archive.resolve(), "output must not replace the CLI archive")
    require(not output.is_relative_to(app.resolve()), "output must not be inside the native app")

    # The temporary directory shares the output filesystem, so replacement is atomic.
    with tempfile.TemporaryDirectory(prefix=".cq-macos-bundle.", dir=output.parent) as temporary:
        temporary = Path(temporary)
        payload = temporary / "payload"
        payload.mkdir(mode=0o700)
        found = False
        with tarfile.open(archive, mode="r|gz") as source:
            for entry in source:
                require(not found and entry.name == "cq" and entry.isreg() and not entry.issparse(), "CLI archive must contain only the regular cq executable")
                require(0 < entry.size <= 512 << 20, "CLI executable size is invalid")
                require(entry.mode & stat.S_IXUSR and not entry.mode & 0o7000, "CLI executable permissions are invalid")
                body = source.extractfile(entry)
                require(body is not None, "CLI executable is unreadable")
                with body, (payload / "cq").open("xb") as destination:
                    shutil.copyfileobj(body, destination, 1 << 20)
                require((payload / "cq").stat().st_size == entry.size, "CLI executable is truncated")
                (payload / "cq").chmod(entry.mode & 0o777)
                os.utime(payload / "cq", (entry.mtime, entry.mtime))
                found = True
        require(found, "CLI archive is empty")
        subprocess.run(["/usr/bin/ditto", str(app), str(payload / "CQNetwork.app")], check=True)
        bundle = temporary / "bundle.tar.gz"
        # Signature and stapled-ticket files remain intact. Do not distribute the
        # build host's quarantine/provenance xattrs as AppleDouble archive entries.
        subprocess.run(["/usr/bin/tar", "-czf", str(bundle), "-C", str(payload), "cq", "CQNetwork.app"],
                       check=True, env=dict(os.environ, COPYFILE_DISABLE="1"))
        bundle.chmod(0o644)
        os.replace(bundle, output)
except (OSError, ValueError, tarfile.TarError, subprocess.CalledProcessError) as error:
    print(f"macOS bundle: {error}", file=sys.stderr)
    sys.exit(1)
PY
