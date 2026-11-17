#!/usr/bin/env bash
# Signing material stays in disposable runner storage, never in the output.
set +x
set -euo pipefail
umask 077

if [[ $# -lt 2 || $# -gt 3 || ( "$1" != prepare && "$1" != finish ) || ( "$1" == prepare && $# != 2 ) ]]; then
  echo 'usage: build-sign-ci.sh prepare OUTPUT | finish OUTPUT [RESUME_DIRECTORY]' >&2
  exit 64
fi
mode=$1
root=$(cd "$(dirname "$0")/.." && pwd -P)
source_commit=$(git -C "$root" rev-parse --verify 'HEAD^{commit}')
export CQ_VERSION=${CQ_VERSION:?} CQ_BUILD_NUMBER=${CQ_BUILD_NUMBER:?} CQ_PREVIOUS_BUILD_NUMBER=${CQ_PREVIOUS_BUILD_NUMBER:?}
CQ_VERSION=${CQ_VERSION#v}
python3 - "$source_commit" <<'PY'
import os, re, sys
version, build, previous = (os.environ[k] for k in ["CQ_VERSION", "CQ_BUILD_NUMBER", "CQ_PREVIOUS_BUILD_NUMBER"])
if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version) or not re.fullmatch(r"[1-9][0-9]*", build) or not re.fullmatch(r"0|[1-9][0-9]*", previous) or int(build) <= int(previous) or not re.fullmatch(r"[0-9a-f]{40}", sys.argv[1]):
    sys.exit("Invalid release source, version or increasing integer build number")
PY
for name in CQ_NOTARY_KEY_P8 CQ_NOTARY_KEY_ID CQ_NOTARY_ISSUER_ID; do
  if [[ -z ${!name:-} ]]; then echo "Missing required input: $name" >&2; exit 64; fi
done
mkdir -p "$2"
output=$(cd "$2" && pwd -P)
temporary=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/cq-native-sign.XXXXXX")
keychain="$temporary/signing.keychain-db"
keychains=()
installed_profiles=()
search_list_changed=false
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ $search_list_changed == true ]]; then
    security list-keychains -d user -s ${keychains[@]+"${keychains[@]}"} >/dev/null 2>&1 || status=1
  fi
  if [[ -f $keychain ]]; then security delete-keychain "$keychain" >/dev/null 2>&1 || status=1; fi
  for profile in ${installed_profiles[@]+"${installed_profiles[@]}"}; do rm -f "$profile" || status=1; done
  rm -rf "$temporary" || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf '%s' "$CQ_NOTARY_KEY_P8" >"$temporary/notary.p8"
notary_auth=(--key "$temporary/notary.p8" --key-id "$CQ_NOTARY_KEY_ID" --issuer "$CQ_NOTARY_ISSUER_ID")

# Bound archive, export and upload operations, including their child processes.
run_bounded() {
  perl -MPOSIX -e '
    my $pid = fork(); die "Could not start operation\n" unless defined $pid;
    if (!$pid) { setpgrp(0, 0); exec @ARGV; exit 127; }
    my $stop = sub { alarm 0; kill "TERM", -$pid; sleep 2; kill "KILL", -$pid;
      waitpid($pid, 0); die "Operation interrupted or timed out\n"; };
    $SIG{ALRM} = $stop; $SIG{INT} = $stop; $SIG{TERM} = $stop;
    alarm 600; waitpid($pid, 0); my $status = $?; alarm 0;
    exit(($status & 127) ? 128 + ($status & 127) : $status >> 8);
  ' "$@"
}

verify_app() {
  local app=$1
  local provider="$app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension"
  local requirement='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "K2648T24P4"'
  codesign --verify --deep --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network\"" "$app"
  codesign --verify --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network.proxy\"" "$provider"
  for architecture in arm64 x86_64; do
    lipo "$app/Contents/MacOS/CQNetwork" -verify_arch "$architecture"
    lipo "$provider/Contents/MacOS/CQNetworkProxy" -verify_arch "$architecture"
    codesign --display --arch "$architecture" --entitlements - --xml "$app" >"$temporary/host-$architecture.entitlements" 2>/dev/null
    codesign --display --arch "$architecture" --entitlements - --xml "$provider" >"$temporary/provider-$architecture.entitlements" 2>/dev/null
  done
  python3 - "$app" "$provider" "$root" "$temporary" <<'PY'
import os, pathlib, plistlib, sys
for kind, path, identifier in zip(["host", "provider"], sys.argv[1:3], ["dev.jacobcx.cq.network", "dev.jacobcx.cq.network.proxy"]):
    p = plistlib.loads((pathlib.Path(path)/"Contents/Info.plist").read_bytes())
    expected = {"CFBundleIdentifier": identifier, "CFBundleVersion": os.environ["CQ_BUILD_NUMBER"], "CFBundleShortVersionString": os.environ["CQ_VERSION"], "LSMinimumSystemVersion": "13.0"}
    if any(p.get(k) != v for k, v in expected.items()) or not p.get("NSSystemExtensionUsageDescription", "").strip():
        sys.exit("Signed bundle metadata differs from requested release")
    if identifier.endswith(".proxy") and p.get("NetworkExtension", {}).get("NEMachServiceName") != "K2648T24P4.dev.jacobcx.cq.network.proxy." + os.environ["CQ_BUILD_NUMBER"]:
        sys.exit("Provider Mach service differs from requested build")
    expected = plistlib.loads((pathlib.Path(sys.argv[3])/"Packaging"/(kind.title()+".entitlements")).read_bytes())
    expected["com.apple.developer.networking.networkextension"] = ["app-proxy-provider-systemextension"]
    for architecture in ["arm64", "x86_64"]:
        signed = plistlib.loads((pathlib.Path(sys.argv[4])/(kind+"-"+architecture+".entitlements")).read_bytes())
        if any(signed.get(key) != value for key, value in expected.items()):
            sys.exit("Signed bundle entitlements differ from the requested distribution entitlements")
PY
}

if [[ $mode == prepare ]]; then
  request="$output/native-request"
  if [[ -e $request ]]; then echo 'A native request already exists; resume or reconcile it before preparing another.' >&2; exit 1; fi
  for name in CQ_SIGNING_P12_BASE64 CQ_SIGNING_P12_PASSWORD CQ_HOST_PROFILE_BASE64 CQ_PROVIDER_PROFILE_BASE64; do
    if [[ -z ${!name:-} ]]; then echo "Missing required input: $name" >&2; exit 64; fi
  done
  python3 - "$temporary" <<'PY'
import base64, os, pathlib, sys
try:
    for name, file in [("CQ_SIGNING_P12_BASE64", "signing.p12"), ("CQ_HOST_PROFILE_BASE64", "host.provisionprofile"), ("CQ_PROVIDER_PROFILE_BASE64", "provider.provisionprofile")]:
        (pathlib.Path(sys.argv[1])/file).write_bytes(base64.b64decode("".join(os.environ[name].split()), validate=True))
except ValueError:
    sys.exit("Signing inputs must contain valid base64")
PY
  security list-keychains -d user >"$temporary/keychains.txt"
  python3 - "$temporary/keychains.txt" >"$temporary/keychain-paths.txt" <<'PY'
import pathlib, shlex, sys
for value in shlex.split(pathlib.Path(sys.argv[1]).read_text()): print(value)
PY
  while IFS= read -r path; do keychains+=("$path"); done <"$temporary/keychain-paths.txt"
  password=$(openssl rand -hex 32)
  security create-keychain -p "$password" "$keychain" >/dev/null 2>&1
  security set-keychain-settings -lut 21600 "$keychain" >/dev/null 2>&1
  security unlock-keychain -p "$password" "$keychain" >/dev/null 2>&1
  search_list_changed=true
  security list-keychains -d user -s "$keychain" ${keychains[@]+"${keychains[@]}"} >/dev/null 2>&1
  security import "$temporary/signing.p12" -k "$keychain" -P "$CQ_SIGNING_P12_PASSWORD" -T /usr/bin/codesign -T /usr/bin/security >/dev/null 2>&1
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$password" "$keychain" >/dev/null 2>&1
  security find-identity -v -p codesigning "$keychain" >"$temporary/identities.txt"
  identity=$(python3 - "$temporary/identities.txt" <<'PY'
import pathlib, re, sys
matches = re.findall(r'\b([0-9A-F]{40}) "Developer ID Application: [^"\n]+ \(K2648T24P4\)"', pathlib.Path(sys.argv[1]).read_text())
if len(matches) != 1: sys.exit("Expected one valid company Developer ID Application identity")
print(matches[0])
PY
)
  for kind in host provider; do
    security cms -D -i "$temporary/$kind.provisionprofile" >"$temporary/$kind.plist" 2>/dev/null
  done
  python3 - "$root" "$temporary" "$identity" <<'PY'
import datetime, hashlib, pathlib, plistlib, re, sys
root, tmp, identity = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
mapping = {}
for kind, identifier in [("host", "dev.jacobcx.cq.network"), ("provider", "dev.jacobcx.cq.network.proxy")]:
    p = plistlib.loads((tmp/(kind+".plist")).read_bytes()); e = p.get("Entitlements", {})
    expiry = p.get("ExpirationDate")
    app_id = e.get("com.apple.application-identifier", e.get("application-identifier"))
    valid = p.get("TeamIdentifier") == ["K2648T24P4"] and e.get("com.apple.developer.team-identifier") == "K2648T24P4" and app_id == "K2648T24P4."+identifier and p.get("ProvisionsAllDevices") is True
    valid = valid and isinstance(expiry, datetime.datetime) and expiry.replace(tzinfo=datetime.timezone.utc) > datetime.datetime.now(datetime.timezone.utc)
    valid = valid and identity in [hashlib.sha1(c).hexdigest().upper() for c in p.get("DeveloperCertificates", [])]
    # App Groups is unrestricted on macOS; validate the claimed signed value below.
    valid = valid and "app-proxy-provider-systemextension" in e.get("com.apple.developer.networking.networkextension", [])
    if not valid or not re.fullmatch(r"[0-9a-fA-F-]{36}", p.get("UUID", "")) or not p.get("Name") or "\n" in p["Name"]:
        sys.exit("Distribution profile has an invalid identity, entitlement, certificate or expiry")
    (tmp/(kind+"-uuid")).write_text(p["UUID"]); (tmp/(kind+"-name")).write_text(p["Name"])
    entitlements = plistlib.loads((root/"Packaging"/(kind.title()+".entitlements")).read_bytes())
    if entitlements["com.apple.developer.networking.networkextension"] != ["app-proxy-provider"]:
        sys.exit("Unexpected source Network Extension entitlement")
    entitlements["com.apple.developer.networking.networkextension"] = ["app-proxy-provider-systemextension"]
    (tmp/(kind+".entitlements")).write_bytes(plistlib.dumps(entitlements))
    mapping[identifier] = p["UUID"]
(tmp/"ExportOptions.plist").write_bytes(plistlib.dumps({"method":"developer-id", "teamID":"K2648T24P4", "signingStyle":"manual", "signingCertificate":identity, "provisioningProfiles":mapping, "stripSwiftSymbols":True}))
PY
  for kind in host provider; do
    uuid=$(cat "$temporary/$kind-uuid")
    for directory in "$HOME/Library/MobileDevice/Provisioning Profiles" "$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"; do
      mkdir -p "$directory"
      profile="$directory/$uuid.provisionprofile"
      if [[ -e $profile ]]; then
        cmp -s "$temporary/$kind.provisionprofile" "$profile" || { echo 'An existing profile has a conflicting UUID.' >&2; exit 1; }
      else
        installed_profiles+=("$profile")
        cp "$temporary/$kind.provisionprofile" "$profile"
      fi
    done
  done
  run_bounded xcodebuild -project "$root/CQNetwork.xcodeproj" -scheme CQNetworkApp -configuration Release \
    -derivedDataPath "$temporary/DerivedData" -archivePath "$temporary/CQNetwork.xcarchive" \
    "MARKETING_VERSION=$CQ_VERSION" "CURRENT_PROJECT_VERSION=$CQ_BUILD_NUMBER" \
    CODE_SIGN_STYLE=Manual "CODE_SIGN_IDENTITY=$identity" 'ARCHS=arm64 x86_64' ONLY_ACTIVE_ARCH=NO \
    "CQ_HOST_PROFILE=$(cat "$temporary/host-name")" "CQ_PROVIDER_PROFILE=$(cat "$temporary/provider-name")" \
    "CQ_HOST_ENTITLEMENTS=$temporary/host.entitlements" "CQ_PROVIDER_ENTITLEMENTS=$temporary/provider.entitlements" archive
  run_bounded xcodebuild -exportArchive -archivePath "$temporary/CQNetwork.xcarchive" \
    -exportPath "$temporary/export" -exportOptionsPlist "$temporary/ExportOptions.plist"
  verify_app "$temporary/export/CQNetwork.app"
  mkdir "$request"
  ditto -c -k --sequesterRsrc --keepParent "$temporary/export/CQNetwork.app" "$request/payload.zip"
  # Persist uncertainty before the upload; interruption must never trigger resubmission.
  python3 - "$request" "$source_commit" <<'PY'
import hashlib, json, os, pathlib, sys
path = pathlib.Path(sys.argv[1])
receipt = {"state":"submission-uncertain", "source_commit":sys.argv[2], "version":os.environ["CQ_VERSION"], "build_number":os.environ["CQ_BUILD_NUMBER"], "sha256":hashlib.sha256((path/"payload.zip").read_bytes()).hexdigest()}
(path/"submission.json").write_text(json.dumps(receipt)+"\n")
PY
  if ! run_bounded xcrun notarytool submit "$request/payload.zip" "${notary_auth[@]}" --output-format json >"$temporary/submitted.json" 2>"$temporary/submit-error"; then
    echo 'Notary upload did not return a confirmed submission; preserve the request and reconcile it before retrying.' >&2; exit 1
  fi
  python3 - "$request" "$temporary/submitted.json" <<'PY'
import json, pathlib, re, sys
path = pathlib.Path(sys.argv[1]); result = json.loads(pathlib.Path(sys.argv[2]).read_text()); identifier = result.get("id", "")
if not re.fullmatch(r"[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", identifier): sys.exit("Notary upload returned no valid submission ID; reconcile the uncertain request")
receipt = json.loads((path/"submission.json").read_text()); receipt.update(state="submitted", submission_id=identifier)
(path/"submission.json.tmp").write_text(json.dumps(receipt)+"\n"); (path/"submission.json.tmp").replace(path/"submission.json")
PY
  echo 'Signed native payload and notarisation submission receipt prepared.'
else
  request=${3:-"$output/native-request"}
  identifier=$(python3 - "$request" "$source_commit" <<'PY'
import hashlib, json, os, pathlib, posixpath, re, stat, sys, zipfile
path = pathlib.Path(sys.argv[1]); receipt = json.loads((path/"submission.json").read_text()); identifier = receipt.get("submission_id", "")
expected = {"state":"submitted", "source_commit":sys.argv[2], "version":os.environ["CQ_VERSION"], "build_number":os.environ["CQ_BUILD_NUMBER"], "sha256":hashlib.sha256((path/"payload.zip").read_bytes()).hexdigest()}
if any(receipt.get(k) != v for k, v in expected.items()) or not re.fullmatch(r"[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", identifier): sys.exit("Native request is uncertain or differs from source, version, build or payload")
with zipfile.ZipFile(path/"payload.zip") as archive:
    entries = archive.infolist(); names = [p.filename.rstrip("/") for p in entries]
    links = {p.filename.rstrip("/") for p in entries if stat.S_ISLNK(p.external_attr >> 16)}
    if len(names) != len(set(names)) or len(names) > 10000 or sum(p.file_size for p in entries) > 512*1024**2 or any(p.file_size > 256*1024**2 for p in entries): sys.exit("Unsafe native archive size or duplicate paths")
    for p in entries:
        name = pathlib.PurePosixPath(p.filename)
        if not name.parts or name.parts[0] not in ["CQNetwork.app", "__MACOSX"] or str(name) != p.filename.rstrip("/") or ".." in name.parts or "\\" in p.filename or any(str(parent) in links for parent in name.parents): sys.exit("Unsafe native archive path")
        if stat.S_ISLNK(p.external_attr >> 16):
            if p.file_size > 1024: sys.exit("Unsafe native archive link size")
            target = archive.read(p).decode(); resolved = posixpath.normpath(posixpath.join(str(name.parent), target))
            if target.startswith("/") or "\\" in target or not resolved.startswith("CQNetwork.app/"): sys.exit("Unsafe native archive link")
        elif stat.S_IFMT(p.external_attr >> 16) not in [0, stat.S_IFREG, stat.S_IFDIR]: sys.exit("Unsupported native archive file type")
print(identifier)
PY
)
  if ! xcrun notarytool wait "$identifier" "${notary_auth[@]}" --timeout 45m --output-format json >"$temporary/result.json" 2>"$temporary/wait-error"; then
    echo 'Notarisation wait failed or timed out; resume the preserved submission ID.' >&2; exit 1
  fi
  python3 - "$temporary/result.json" "$identifier" "$output/notary-result.json" <<'PY'
import json, pathlib, sys
result = json.loads(pathlib.Path(sys.argv[1]).read_text())
safe = {k:result.get(k) for k in ["id", "status"]}; pathlib.Path(sys.argv[3]).write_text(json.dumps(safe)+"\n")
if safe != {"id":sys.argv[2], "status":"Accepted"}: sys.exit("Notarisation was not accepted; preserve the submission for investigation")
PY
  ditto -x -k "$request/payload.zip" "$temporary/unpacked"
  app="$temporary/unpacked/CQNetwork.app"
  verify_app "$app"
  xcrun stapler staple "$app"
  xcrun stapler validate "$app"
  verify_app "$app"
  spctl --assess --type execute "$app"
  final="$output/CQNetwork_${CQ_VERSION}_universal.zip"
  if [[ -e $final ]]; then echo 'The final native ZIP already exists; refusing to replace it.' >&2; exit 1; fi
  ditto -c -k --sequesterRsrc --keepParent "$app" "$temporary/final.zip"
  mv "$temporary/final.zip" "$final"
  echo 'Accepted, stapled and verified universal native ZIP prepared.'
fi
