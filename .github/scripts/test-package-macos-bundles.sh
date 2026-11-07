#!/usr/bin/env bash
set -euo pipefail

# Exercise the real Ruby transform without macOS packaging or signing tools.
script_directory=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
python3 - "$script_directory/package-macos-bundles.sh" <<'PY'
import copy
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

source = Path(sys.argv[1]).read_text()
blocks = re.findall(r"^ruby [^\n]*<<'RUBY'\n(.*?)^RUBY$", source, re.M | re.S)
assert len(blocks) == 1, "expected one actual package transformation block"
version = "0.47.1"
prefix = "https://github.com/jacobcxdev/cq/releases/download/"

def digest(data):
    return hashlib.sha256(data).hexdigest()

def run_case(root, name, quoted=True, interpolate=True, invalid=None):
    work = root / name
    dist = work / "distribution with spaces"
    cask = dist / "homebrew/Casks/cq.rb"
    cask.parent.mkdir(parents=True)
    native = b"verified native ZIP fixture"
    pin = dict(schema_version=2, version=version, source_commit="a"*40,
               asset_id=123, sha256=digest(native), minimum_macos="13.0")
    manifest = work / "native pin.json"
    manifest.write_text(json.dumps(pin))
    before_manifest = manifest.read_bytes()
    originals, bundles = {}, {}
    retained = []
    for architecture in ["arm64", "amd64"]:
        cli = f"cq_{version}_darwin_{architecture}.tar.gz"
        bundle = f"cq_{version}_darwin_{architecture}_bundle.tar.gz"
        originals[cli] = (architecture + " original CLI").encode()
        bundles[architecture] = (architecture + " combined payload").encode()
        (dist / cli).write_bytes(originals[cli])
        (dist / bundle).write_bytes(bundles[architecture])
        retained.append(f"{digest(originals[cli])}  {cli}\n")
    for architecture in ["arm64", "amd64"]:
        cli = f"cq_{version}_linux_{architecture}.tar.gz"
        originals[cli] = (architecture + " unchanged Linux CLI").encode()
        (dist / cli).write_bytes(originals[cli])
        retained.append(f"{digest(originals[cli])}  {cli}\n")
    checksums = dist / "checksums.txt"
    checksums.write_text("".join(retained))
    original_checksums = checksums.read_bytes()
    token = "#{version}" if interpolate else version
    cask_version = "0.47.2" if invalid == "wrong-version" else version
    old_pin = copy.deepcopy(pin)
    if invalid == "wrong-pin-version": old_pin["version"] = "0.47.2"
    if invalid == "wrong-pin-digest": old_pin["sha256"] = "b"*64
    fragments = [f'cask "cq" do\n  version "{cask_version}"\n  on_macos do\n']
    for architecture, condition in [("arm64", "on_arm"), ("amd64", "on_intel")]:
        if invalid == "missing-arch" and architecture == "amd64": continue
        checksum = digest(originals[f"cq_{version}_darwin_{architecture}.tar.gz"])
        url = prefix + f"v{token}/cq_{token}_darwin_{architecture}.tar.gz"
        if architecture == "arm64":
            if invalid == "foreign-url": url = url.replace("github.com", "example.invalid")
            if invalid == "wrong-url-version": url = url.replace(token, "0.47.2")
            if invalid == "wrong-digest": checksum = "0"*64
            if invalid == "cross-arch-digest": checksum = digest(originals[f"cq_{version}_darwin_amd64.tar.gz"])
            if invalid == "core-url-bundle-digest": checksum = digest(bundles[architecture])
            if invalid == "bundle-url-core-digest": url = url.replace(".tar.gz", "_bundle.tar.gz")
        fragment = f'  {condition} do\n    sha256 "{checksum}"\n    url "{url}"\n  end\n'
        if invalid == "missing-digest" and architecture == "arm64": fragment = re.sub(r'^    sha256 .*\n', '', fragment, flags=re.M)
        fragments.append(fragment)
        if invalid == "duplicate-arch" and architecture == "arm64": fragments.append(fragment)
    fragments.append("  end\n")
    linux_fragments = ["  on_linux do\n"]
    for architecture, condition in [("arm64", "on_arm"), ("amd64", "on_intel")]:
        checksum = digest(originals[f"cq_{version}_linux_{architecture}.tar.gz"])
        url = prefix + f"v{token}/cq_{token}_linux_{architecture}.tar.gz"
        if architecture == "arm64":
            if invalid == "foreign-linux-url": url = url.replace("github.com", "example.invalid")
            if invalid == "wrong-linux-version": url = url.replace(token, "0.47.2")
            if invalid == "wrong-linux-digest": checksum = "0"*64
        fragment = f'    {condition} do\n      sha256 "{checksum}"\n      url "{url}"\n    end\n'
        linux_fragments.append(fragment)
        if invalid == "duplicate-linux" and architecture == "arm64": linux_fragments.append(fragment)
    linux_fragments.append("  end\n")
    linux_block = "".join(linux_fragments)
    fragments.append(linux_block)
    if invalid == "extra-foreign-url": fragments.append('  url "https://example.invalid/foreign.tar.gz"\n')
    delimiter = "'JSON'" if quoted else "JSON"
    pin_text = f"  pin = JSON.parse(<<~{delimiter})\n    {json.dumps(old_pin)}\n  JSON\n"
    if invalid != "missing-pin": fragments.append(pin_text)
    if invalid == "duplicate-pin": fragments.append(pin_text)
    fragments.append("end\n")
    cask.write_text("".join(fragments))
    before_cask = cask.read_bytes()

    def transform():
        return subprocess.run(["ruby", "-", version, str(dist), str(manifest)], input=blocks[0], text=True, capture_output=True)

    result = transform()
    for filename, body in originals.items(): assert (dist / filename).read_bytes() == body, "original CLI archive changed"
    assert manifest.read_bytes() == before_manifest, "source native pin changed"
    if invalid:
        assert result.returncode != 0, f"accepted invalid {invalid}"
        assert cask.read_bytes() == before_cask, "rejected transform changed Cask"
        assert checksums.read_bytes() == original_checksums, "rejected transform changed checksums"
        return
    assert result.returncode == 0, result.stderr
    syntax = subprocess.run(["ruby", "-c", str(cask)], capture_output=True, text=True)
    assert syntax.returncode == 0, syntax.stderr
    text = cask.read_text()
    assert linux_block in text, "Linux Cask blocks changed"
    expected = {architecture: dict(url=prefix + f"v{version}/cq_{version}_darwin_{architecture}_bundle.tar.gz",
                                   sha256=digest(body)) for architecture, body in bundles.items()}
    assert json.loads((dist / "macos-bundles.json").read_text()) == expected
    bundled_pin = json.loads((dist / "bundled-native-network.json").read_text())
    assert bundled_pin == dict(pin, bundles=expected)
    matches = re.findall(r'pin = JSON\.parse\(<<~(?:\x27JSON\x27|JSON)\)\n(.*?)^\s*JSON$', text, re.M | re.S)
    assert len(matches) == 1 and json.loads(matches[0]) == bundled_pin
    pairs = [pair for pair in re.findall(r'sha256 "([0-9a-f]{64})"\s+url "([^"\n]+)"', text) if "_darwin_" in pair[1]]
    evaluated = {url.replace("#{version}", version): sha for sha, url in pairs}
    assert evaluated == {value["url"]: value["sha256"] for value in expected.values()}, "URL/SHA architecture pairing differs"
    assert all("#{version}" in url for _, url in pairs), "Ruby version interpolation lost"
    checksum_lines = checksums.read_text().splitlines(keepends=True)
    assert checksum_lines[:len(retained)] == retained, "old CLI checksums changed"
    for architecture, value in expected.items():
        filename = f"cq_{version}_darwin_{architecture}_bundle.tar.gz"
        assert checksum_lines.count(f"{value['sha256']}  {filename}\n") == 1
    assert checksum_lines.count(f"{digest((dist/'macos-bundles.json').read_bytes())}  macos-bundles.json\n") == 1
    assert checksum_lines.count(f"{pin['sha256']}  CQNetwork_{version}_universal.zip\n") == 1
    before = {path: path.read_bytes() for path in [cask, checksums, dist/"macos-bundles.json", dist/"bundled-native-network.json"]}
    repeated = transform()
    assert repeated.returncode == 0, repeated.stderr
    assert all(path.read_bytes() == body for path, body in before.items()), "transformation is not idempotent"

with tempfile.TemporaryDirectory(prefix="cq-bundle-casks.") as temporary:
    root = Path(temporary)
    for quoted in [True, False]:
        for interpolate in [True, False]:
            run_case(root, f"valid-{quoted}-{interpolate}", quoted, interpolate)
    failures = []
    for invalid in ["foreign-url", "extra-foreign-url", "missing-arch", "duplicate-arch", "wrong-version",
                    "wrong-url-version", "wrong-pin-version", "wrong-pin-digest", "wrong-digest",
                    "cross-arch-digest", "missing-digest", "core-url-bundle-digest", "bundle-url-core-digest",
                    "missing-pin", "duplicate-pin", "foreign-linux-url", "wrong-linux-version",
                    "wrong-linux-digest", "duplicate-linux"]:
        try: run_case(root, invalid, invalid=invalid)
        except AssertionError as error: failures.append(str(error))
    assert not failures, "; ".join(failures)
    print("macOS bundle Cask interpolation, architecture pairing, pin, checksums and rejection fixtures passed")
PY
