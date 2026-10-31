#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "Usage: CQ_VERSION=1.2.3 CQ_BUILD_NUMBER=123 $0 OUTPUT_DIRECTORY" >&2
    exit 2
fi
: "${CQ_VERSION:?Set CQ_VERSION to the release version without a leading v}"
: "${CQ_BUILD_NUMBER:?Set CQ_BUILD_NUMBER to the increasing release build number}"
for value in "$CQ_VERSION" "$CQ_BUILD_NUMBER"; do
    if ! printf '%s\n' "$value" | /usr/bin/grep -Eq '^[0-9]+(\.[0-9]+){0,2}$'; then
        echo "Release versions must contain one to three numeric components." >&2
        exit 2
    fi
done

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
mkdir -p -- "$1"
output=$(CDPATH= cd -- "$1" && pwd -P)

# Bound each Xcode operation and terminate its child processes on timeout.
run_bounded() {
    /usr/bin/perl -MPOSIX -e '
        my $pid = fork();
        die "Could not start build\n" unless defined $pid;
        if (!$pid) { setpgrp(0, 0); exec @ARGV; exit 127; }
        my $stop = sub {
            alarm 0;
            kill "TERM", -$pid;
            sleep 2;
            kill "KILL", -$pid;
            waitpid($pid, 0);
            die "Build interrupted or timed out\n";
        };
        $SIG{ALRM} = $stop; $SIG{INT} = $stop; $SIG{TERM} = $stop;
        alarm 600;
        waitpid($pid, 0);
        my $status = $?;
        alarm 0;
        exit(($status & 127) ? 128 + ($status & 127) : $status >> 8);
    ' "$@"
}

run_bounded /usr/bin/xcodebuild \
    -project "$root/CQNetwork.xcodeproj" -scheme CQNetworkApp \
    -configuration Release -allowProvisioningUpdates \
    -derivedDataPath "$output/DerivedData" \
    -archivePath "$output/CQNetwork.xcarchive" \
    "MARKETING_VERSION=$CQ_VERSION" "CURRENT_PROJECT_VERSION=$CQ_BUILD_NUMBER" \
    archive

run_bounded /usr/bin/xcodebuild -exportArchive \
    -archivePath "$output/CQNetwork.xcarchive" \
    -exportPath "$output/export" \
    -exportOptionsPlist "$root/Packaging/ExportOptions.plist" \
    -allowProvisioningUpdates

app="$output/export/CQNetwork.app"
provider="$app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension"
provider_identifier=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$provider/Contents/Info.plist")
if [ "$(/usr/bin/basename "$provider" .systemextension)" != "$provider_identifier" ]; then
    echo "System extension filename must match its bundle identifier." >&2
    exit 1
fi
/usr/bin/codesign --verify --deep --strict "$app"
for architecture in arm64 x86_64; do
    /usr/bin/lipo "$app/Contents/MacOS/CQNetwork" -verify_arch "$architecture"
    /usr/bin/lipo \
        "$provider/Contents/MacOS/CQNetworkProxy" \
        -verify_arch "$architecture"
done
printf 'Signed app: %s\n' "$app"
