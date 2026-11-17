#!/usr/bin/env bash
set -euo pipefail
script="$(cd "$(dirname "$0")/../../native/macos-network/Packaging" && pwd)/build-sign-ci.sh"
python3 - "$script" <<'PY'
import base64
import datetime
import hashlib
import json
import os
from pathlib import Path
import plistlib
import shutil
import subprocess
import sys
import tempfile
import uuid

script = sys.argv[1]
root = Path(tempfile.mkdtemp(prefix="cq-native-sign-test."))
try:
    (root / "bin").mkdir()
    (root / "scratch").mkdir()
    certificate = b"synthetic distribution certificate"
    fingerprint = hashlib.sha1(certificate).hexdigest().upper()
    profile_ids = [str(uuid.uuid4()), str(uuid.uuid4())]
    env = dict(os.environ, PATH=str(root / "bin") + ":" + os.environ["PATH"],
        RUNNER_TEMP=str(root / "scratch"), NATIVE_TEST_ROOT=str(root),
        NATIVE_TEST_FINGERPRINT=fingerprint, CQ_VERSION="1.2.3", CQ_BUILD_NUMBER="45",
        CQ_PREVIOUS_BUILD_NUMBER="44", CQ_SIGNING_P12_BASE64=base64.b64encode(b"synthetic p12").decode(),
        CQ_SIGNING_P12_PASSWORD="synthetic-password-never-log", CQ_NOTARY_KEY_P8="synthetic-private-key-never-log",
        CQ_NOTARY_KEY_ID="ABCDEFGHIJ", CQ_NOTARY_ISSUER_ID="11111111-2222-3333-4444-555555555555")
    # The shipped v0.49.9 Developer ID profiles authorise NE, but omit App Groups.
    profiles = []
    for identifier, profile_id in zip(["dev.jacobcx.cq.network", "dev.jacobcx.cq.network.proxy"], profile_ids):
        profile = dict(UUID=profile_id, Name="CI " + identifier, TeamIdentifier=["K2648T24P4"],
            ExpirationDate=datetime.datetime(2099, 1, 1), ProvisionsAllDevices=True,
            DeveloperCertificates=[certificate], Entitlements={"com.apple.application-identifier": "K2648T24P4." + identifier,
            "com.apple.developer.team-identifier": "K2648T24P4",
            "com.apple.developer.networking.networkextension": ["app-proxy-provider-systemextension"]})
        profiles.append(profile)
    def encode(profile):
        return base64.b64encode(plistlib.dumps(profile)).decode()
    env["CQ_HOST_PROFILE_BASE64"], env["CQ_PROVIDER_PROFILE_BASE64"] = map(encode, profiles)
    fake = root / "bin" / "native-tool"
    fake.write_text('''#!/usr/bin/env python3
import hashlib,json,os,plistlib,shutil,signal,sys,zipfile
from pathlib import Path
tool=Path(sys.argv[0]).name; a=sys.argv[1:]; root=Path(os.environ["NATIVE_TEST_ROOT"])
def value(flag): return a[a.index(flag)+1]
with (root/"calls").open("a") as f: f.write(tool+" "+" ".join(a)+"\\n")
if tool=="git":
    print("a"*40); sys.exit(0)
if tool=="security":
    if a[0]=="import" and os.environ.get("NATIVE_TEST_FAIL")=="import": raise SystemExit(19)
    if a[0]=="list-keychains":
        state=root/"keychains.json"
        if "-s" in a: state.write_text(json.dumps(a[a.index("-s")+1:])); sys.exit(0)
        for p in json.loads(state.read_text()) if state.exists() else ["/fixture/login.keychain-db"]: print(json.dumps(p))
    elif a[0]=="create-keychain": Path(a[-1]).touch()
    elif a[0]=="delete-keychain": Path(a[-1]).unlink()
    elif a[0]=="cms": sys.stdout.buffer.write(Path(value("-i")).read_bytes())
    elif a[0]=="find-identity": print('  1) '+os.environ["NATIVE_TEST_FINGERPRINT"]+' "Developer ID Application: Fixture (K2648T24P4)"')
    elif a[0] not in ["set-keychain-settings","unlock-keychain","import","set-key-partition-list"]: raise SystemExit("unexpected security call")
elif tool=="xcodebuild":
    if os.environ.get("NATIVE_TEST_FAIL")=="archive": raise SystemExit(19)
    if os.environ.get("NATIVE_TEST_FAIL")=="cancel": os.kill(os.getppid(),signal.SIGTERM); sys.exit(1)
    if "archive" in a:
        settings=dict(x.split("=",1) for x in a if "=" in x)
        assert settings["CODE_SIGN_STYLE"]=="Manual" and settings["CODE_SIGN_IDENTITY"]==os.environ["NATIVE_TEST_FINGERPRINT"]
        assert "-allowProvisioningUpdates" not in a
        assert settings["CQ_HOST_PROFILE"]!=settings["CQ_PROVIDER_PROFILE"]
        for name in ["HOST","PROVIDER"]:
            p=plistlib.loads(Path(settings["CQ_"+name+"_ENTITLEMENTS"]).read_bytes())
            assert p["com.apple.developer.networking.networkextension"]==["app-proxy-provider-systemextension"]
            assert p["com.apple.security.application-groups"]==["K2648T24P4.dev.jacobcx.cq.network"]
            if name=="PROVIDER": assert p["com.apple.security.temporary-exception.sbpl"] and p["com.apple.security.app-sandbox"]
            (root/(name.lower()+".entitlements")).write_bytes(plistlib.dumps(p))
        Path(value("-archivePath")).mkdir()
    else:
        p=plistlib.loads(Path(value("-exportOptionsPlist")).read_bytes())
        assert p["signingStyle"]=="manual" and p["method"]=="developer-id" and len(p["provisioningProfiles"])==2
        app=Path(value("-exportPath"))/"CQNetwork.app"
        provider=app/"Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension"
        for bundle,identifier,exe in [(app,"dev.jacobcx.cq.network","CQNetwork"),(provider,"dev.jacobcx.cq.network.proxy","CQNetworkProxy")]:
            (bundle/"Contents/MacOS").mkdir(parents=True)
            info=dict(CFBundleIdentifier=identifier,CFBundleVersion=os.environ["CQ_BUILD_NUMBER"],CFBundleShortVersionString=os.environ["CQ_VERSION"],LSMinimumSystemVersion="13.0",NSSystemExtensionUsageDescription="Fixture")
            if bundle==provider: info["NetworkExtension"]={"NEMachServiceName":"K2648T24P4.dev.jacobcx.cq.network.proxy."+os.environ["CQ_BUILD_NUMBER"]}
            (bundle/"Contents/Info.plist").write_bytes(plistlib.dumps(info)); (bundle/"Contents/MacOS"/exe).write_text("fixture")
            entitlements=plistlib.loads((root/(("provider" if bundle==provider else "host")+".entitlements")).read_bytes())
            failure=os.environ.get("NATIVE_TEST_FAIL")
            if failure=="signed-group" or (failure=="provider-signed-group" and bundle==provider): entitlements.pop("com.apple.security.application-groups")
            (bundle/"Contents/fake-entitlements.plist").write_bytes(plistlib.dumps(entitlements))
elif tool=="xcrun":
    if a[0]=="notarytool":
        assert "--key" in a and Path(value("--key")).read_text()==os.environ["CQ_NOTARY_KEY_P8"]
        if a[1]=="submit":
            if os.environ.get("NATIVE_TEST_FAIL")=="upload": raise SystemExit(75)
            print(json.dumps({"id":"bad" if os.environ.get("NATIVE_TEST_FAIL")=="missing-id" else "12345678-1234-1234-1234-123456789abc","message":"Successfully uploaded file"}))
        elif a[1]=="wait":
            assert a[2]=="12345678-1234-1234-1234-123456789abc"
            if os.environ.get("NATIVE_TEST_FAIL")=="wait": raise SystemExit(75)
            print(json.dumps({"id":a[2],"status":"Invalid" if os.environ.get("NATIVE_TEST_FAIL")=="invalid" else "Accepted"}))
        elif a[1]=="log": Path(a[3]).write_text('{"status":"Invalid"}')
        else: raise SystemExit("unexpected notary call")
    elif a[:2]==["stapler","staple"]: (Path(a[-1])/"ticket").write_text("accepted")
    elif a[:2]==["stapler","validate"]: assert (Path(a[-1])/"ticket").exists()
    else: raise SystemExit("unexpected xcrun call")
elif tool=="ditto":
    if "-c" in a:
        source=Path(a[-2])
        with zipfile.ZipFile(a[-1],"w") as z:
            for p in source.rglob("*"):
                if p.is_file(): z.write(p,p.relative_to(source.parent))
            z.writestr("__MACOSX/._CQNetwork.app",b"fixture resource metadata")
    else:
        with zipfile.ZipFile(a[-2]) as z: z.extractall(a[-1])
elif tool=="codesign":
    if "--display" in a:
        p=plistlib.loads((Path(a[-1])/"Contents/fake-entitlements.plist").read_bytes())
        if os.environ.get("NATIVE_TEST_FAIL")=="x86-signed-group" and "--arch" in a and value("--arch")=="x86_64": p.pop("com.apple.security.application-groups")
        sys.stdout.buffer.write(plistlib.dumps(p))
elif tool in ["lipo","spctl"]: pass
else: raise SystemExit("unexpected tool: "+tool)
''')
    fake.chmod(0o755)
    for name in ["security", "xcodebuild", "xcrun", "git", "ditto", "codesign", "lipo", "spctl"]:
        (root / "bin" / name).symlink_to(fake)
    def invoke(mode, name, changes=None, resume=None):
        settings = dict(env, **(changes or {}))
        args = ["/bin/bash", script, mode, str(root / name)]
        if resume: args.append(str(resume))
        result = subprocess.run(args, env=settings, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        assert "synthetic-password-never-log" not in result.stdout and "synthetic-private-key-never-log" not in result.stdout, result.stdout
        return result
    def clean():
        assert not list((root / "scratch").iterdir()), "ephemeral signing material survived"
        state = root / "keychains.json"
        if state.exists(): assert json.loads(state.read_text()) == ["/fixture/login.keychain-db"], "keychain search list not restored"
        for profile_id in profile_ids:
            for folder in ["Library/MobileDevice/Provisioning Profiles", "Library/Developer/Xcode/UserData/Provisioning Profiles"]:
                assert not (Path.home() / folder / (profile_id + ".provisionprofile")).exists(), "installed profile survived"
    # Removing the prepare/archive branch must stop producing a durable request.
    result = invoke("prepare", "good")
    assert result.returncode == 0, "prepare failed: " + result.stdout
    request = root / "good/native-request"
    receipt = json.loads((request / "submission.json").read_text())
    assert receipt["submission_id"] == "12345678-1234-1234-1234-123456789abc"
    assert receipt["source_commit"] == "a" * 40 and receipt["version"] == "1.2.3" and receipt["build_number"] == "45"
    assert receipt["sha256"] == hashlib.sha256((request / "payload.zip").read_bytes()).hexdigest()
    clean()
    # Recovery must consume the recorded ID and exact bytes without signing again.
    before = (root / "calls").read_text()
    result = invoke("finish", "finished", {"CQ_SIGNING_P12_BASE64":"", "CQ_SIGNING_P12_PASSWORD":"", "CQ_HOST_PROFILE_BASE64":"", "CQ_PROVIDER_PROFILE_BASE64":""}, request)
    assert result.returncode == 0, "finish failed: " + result.stdout
    import zipfile
    with zipfile.ZipFile(root / "finished/CQNetwork_1.2.3_universal.zip") as archive:
        assert "CQNetwork.app/ticket" in archive.namelist()
    calls = (root / "calls").read_text()[len(before):]
    assert "notarytool wait 12345678-1234-1234-1234-123456789abc" in calls and "notarytool submit" not in calls and "xcodebuild" not in calls
    clean()
    # Invalid boundary inputs must fail before a submission or final package.
    invalid_profile = dict(profiles[0], TeamIdentifier=["OTHERTEAM"])
    for name, changes in [
        ("old-build", {"CQ_BUILD_NUMBER":"44"}), ("bad-build", {"CQ_BUILD_NUMBER":"45.1"}),
        ("bad-version", {"CQ_VERSION":"vv1.2.3"}), ("missing-key", {"CQ_NOTARY_KEY_P8":""}),
        ("wrong-team", {"CQ_HOST_PROFILE_BASE64":encode(invalid_profile)}),
        ("expired", {"CQ_HOST_PROFILE_BASE64":encode(dict(profiles[0], ExpirationDate=datetime.datetime(2000,1,1)))}),
        ("wrong-cert", {"CQ_HOST_PROFILE_BASE64":encode(dict(profiles[0], DeveloperCertificates=[b"other certificate"]))}),
        ("wrong-id", {"CQ_HOST_PROFILE_BASE64":encode(dict(profiles[0], Entitlements=dict(profiles[0]["Entitlements"], **{"com.apple.application-identifier":"K2648T24P4.other"})))}),
        ("import-error", {"NATIVE_TEST_FAIL":"import"}), ("archive-error", {"NATIVE_TEST_FAIL":"archive"}),
        ("signed-group", {"NATIVE_TEST_FAIL":"signed-group"}), ("provider-signed-group", {"NATIVE_TEST_FAIL":"provider-signed-group"}),
        ("x86-signed-group", {"NATIVE_TEST_FAIL":"x86-signed-group"}), ("cancel", {"NATIVE_TEST_FAIL":"cancel"})]:
        before = (root / "calls").read_text()
        result = invoke("prepare", name, changes)
        assert result.returncode != 0, name + " succeeded"
        assert "notarytool submit" not in (root / "calls").read_text()[len(before):], name + " submitted invalid input"
        clean()
    for name, mutation in [("hash", lambda p: p.update(sha256="0"*64)), ("source", lambda p: p.update(source_commit="b"*40)), ("build", lambda p: p.update(build_number="46")), ("id", lambda p: p.update(submission_id="bad"))]:
        resume = root / ("bad-"+name); shutil.copytree(request, resume)
        invalid = dict(receipt); mutation(invalid); (resume/"submission.json").write_text(json.dumps(invalid))
        before = (root / "calls").read_text()
        assert invoke("finish", "reject-"+name, resume=resume).returncode != 0
        assert "notarytool wait" not in (root / "calls").read_text()[len(before):]
        clean()
    for failure in ["wait", "invalid"]:
        assert invoke("finish", failure, {"NATIVE_TEST_FAIL":failure}, request).returncode != 0
        assert not (root/failure/"CQNetwork_1.2.3_universal.zip").exists()
        assert json.loads((request/"submission.json").read_text()) == receipt
        clean()
    for failure in ["upload", "missing-id"]:
        assert invoke("prepare", failure, {"NATIVE_TEST_FAIL":failure}).returncode != 0
        uncertain = root/failure/"native-request"
        assert json.loads((uncertain/"submission.json").read_text())["state"] == "submission-uncertain"
        before = (root/"calls").read_text()
        assert invoke("prepare", failure).returncode != 0
        assert invoke("finish", "resume-"+failure, resume=uncertain).returncode != 0
        assert "notarytool submit" not in (root/"calls").read_text()[len(before):] and "notarytool wait" not in (root/"calls").read_text()[len(before):]
        clean()
    result = invoke("prepare", "tag-version", {"CQ_VERSION":"v1.2.3"})
    assert result.returncode == 0, "stable tag version was rejected: " + result.stdout
    assert json.loads((root/"tag-version/native-request/submission.json").read_text())["version"] == "1.2.3"
    clean()
    for name, entry, target in [("traversal", "CQNetwork.app/../../escape", None), ("symlink", "CQNetwork.app/link", "../../escape")]:
        resume = root/("unsafe-"+name); shutil.copytree(request, resume)
        with zipfile.ZipFile(resume/"payload.zip", "a") as archive:
            info = zipfile.ZipInfo(entry)
            if target: info.create_system = 3; info.external_attr = 0o120777 << 16
            archive.writestr(info, target or "escape")
        invalid = dict(receipt, sha256=hashlib.sha256((resume/"payload.zip").read_bytes()).hexdigest())
        (resume/"submission.json").write_text(json.dumps(invalid))
        before = (root/"calls").read_text()
        assert invoke("finish", "reject-unsafe-"+name, resume=resume).returncode != 0
        assert "notarytool wait" not in (root/"calls").read_text()[len(before):]
        clean()
    print("Native CI signing, recovery, input validation and cleanup checks passed.")
finally:
    shutil.rmtree(root)
PY
