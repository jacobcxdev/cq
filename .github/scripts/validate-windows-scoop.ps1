param(
    [Parameter(Mandatory)][ValidateSet('amd64', 'arm64')][string]$Architecture,
    [Parameter(Mandatory)][ValidatePattern('^v[0-9]+\.[0-9]+\.[0-9]+$')][string]$Version,
    [switch]$Preflight
)

$ErrorActionPreference = 'Stop'
if (!$IsWindows -or $env:GITHUB_ACTIONS -ne 'true' -or !$env:RUNNER_TEMP) {
    throw 'Scoop modifies the current-user PATH. Run this check on a disposable Windows GitHub runner.'
}
$architectureKey = if ($Architecture -eq 'arm64') { 'arm64' } else { '64bit' }
$osArchitecture = if ($Architecture -eq 'arm64') { 'Arm64' } else { 'X64' }
$machine = if ($Architecture -eq 'arm64') { 0xaa64 } else { 0x8664 }
if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() -ne $osArchitecture) {
    throw "Expected a native $osArchitecture Windows host."
}

function Get-CQArchives([string]$Tag) {
    $release = gh release view $Tag --repo jacobcxdev/cq --json tagName,isDraft,isPrerelease,assets
    if ($LASTEXITCODE -ne 0) { throw "Could not read release $Tag." }
    $release = $release | ConvertFrom-Json
    if ($release.tagName -ne $Tag -or $release.isDraft -or $release.isPrerelease) {
        throw "Expected a public stable release for $Tag."
    }
    $archives = [ordered]@{}
    foreach ($arch in @('amd64', 'arm64')) {
        $name = "cq_$($Tag.TrimStart('v'))_windows_$arch.zip"
        $url = "https://github.com/jacobcxdev/cq/releases/download/$Tag/$name"
        $asset = @($release.assets | Where-Object name -CEQ $name)
        if ($asset.Count -ne 1 -or $asset[0].url -cne $url -or $asset[0].digest -cnotmatch '^sha256:[a-f0-9]{64}$') {
            throw "Invalid release asset $name."
        }
        $key = if ($arch -eq 'amd64') { '64bit' } else { 'arm64' }
        $archives[$key] = @{ url = $url; hash = $asset[0].digest.Substring(7) }
    }
    return $archives
}

function Invoke-Scoop {
    & $scoop @args
    if ($LASTEXITCODE -ne 0) { throw "Scoop failed: $args." }
}

function Assert-ScoopCQ([string]$ExpectedVersion) {
    $installed = Join-Path $env:SCOOP "apps/cq/current"
    $info = Get-Content -LiteralPath (Join-Path $installed 'scoop-install.json') -Raw | ConvertFrom-Json
    if ($info.bucket -ne 'jacobcxdev' -or $info.architecture -ne $architectureKey) {
        throw "Scoop installed the wrong bucket or architecture for $ExpectedVersion."
    }
    $binary = Join-Path $installed 'cq.exe'
    $reader = [IO.BinaryReader]::new([IO.File]::OpenRead($binary))
    try {
        $reader.BaseStream.Position = 0x3c
        $reader.BaseStream.Position = $reader.ReadInt32()
        if ($reader.ReadUInt32() -ne 0x4550 -or $reader.ReadUInt16() -ne $machine) {
            throw "The selected cq.exe is not native $osArchitecture."
        }
    } finally {
        $reader.Dispose()
    }
    $reported = & $shim --version
    if ($LASTEXITCODE -ne 0 -or $reported.TrimStart('v') -ne $ExpectedVersion.TrimStart('v')) {
        throw "Scoop shim did not run CQ $ExpectedVersion."
    }
    Write-Host "Scoop ran native $osArchitecture CQ $ExpectedVersion through its shim."
}

$releases = gh release list --repo jacobcxdev/cq --exclude-drafts --exclude-pre-releases --limit 100 --json tagName
if ($LASTEXITCODE -ne 0) { throw 'Could not list stable releases.' }
$current = [version]$Version.TrimStart('v')
$previous = $releases | ConvertFrom-Json | Where-Object {
    $_.tagName -cmatch '^v[0-9]+\.[0-9]+\.[0-9]+$' -and [version]$_.tagName.TrimStart('v') -lt $current
} | Sort-Object { [version]$_.tagName.TrimStart('v') } -Descending | Select-Object -First 1
if (!$previous) { throw 'No preceding stable release found.' }
$previousVersion = $previous.tagName
$previousArchives = Get-CQArchives $previousVersion
$currentArchives = Get-CQArchives $Version
$root = Join-Path (Resolve-Path -LiteralPath $env:RUNNER_TEMP).Path ('cq-scoop-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
try {
    $env:SCOOP = Join-Path $root 'scoop'
    $env:SCOOP_GLOBAL = Join-Path $root 'scoop-global'
    $env:SCOOP_CACHE = Join-Path $root 'scoop-cache'
    $env:XDG_CONFIG_HOME = Join-Path $root 'config'
    $core = Join-Path $env:SCOOP 'apps/scoop/current'
    git clone --quiet --depth 1 --branch v0.6.0 https://github.com/ScoopInstaller/Scoop.git $core
    if ($LASTEXITCODE -ne 0) { throw 'Could not clone Scoop v0.6.0.' }
    if ((git -C $core rev-parse HEAD) -ne 'e6aa3b366bdee8ed138c1e0f7b85192ebdd35d0f') {
        throw 'Scoop source did not match the v0.6.0 pin.'
    }
    $scoop = Join-Path $core 'bin/scoop.ps1'
    $shim = Join-Path $env:SCOOP 'shims/cq.exe'
    New-Item -ItemType Directory -Path (Join-Path $env:SCOOP 'buckets') | Out-Null
    # Keep Scoop's automatic self-update from replacing the pinned core.
    Invoke-Scoop config last_update ([DateTime]::Now.ToString('o'))
    Invoke-Scoop bucket add jacobcxdev https://github.com/jacobcxdev/homebrew-tap
    $bucket = Join-Path $env:SCOOP 'buckets/jacobcxdev'
    $manifestPath = Join-Path $bucket 'bucket/cq.json'
    if ($Preflight) {
        New-Item -ItemType Directory -Path (Split-Path -Parent $manifestPath) -Force | Out-Null
        $candidate = @{
            version = $Version.TrimStart('v')
            description = 'CLI tool to check AI provider quota usage'
            homepage = 'https://github.com/jacobcxdev/cq'
            license = 'MIT'
            architecture = $currentArchives
            bin = 'cq.exe'
        } | ConvertTo-Json -Depth 8
    } else {
        $candidate = Get-Content -LiteralPath $manifestPath -Raw
    }
    $manifest = $candidate | ConvertFrom-Json -AsHashtable
    if ($manifest.version -ne $Version.TrimStart('v') -or $manifest.bin -ne 'cq.exe') {
        throw "The published Scoop manifest does not provide CQ $Version."
    }
    foreach ($key in $currentArchives.Keys) {
        if ($manifest.architecture[$key].url -cne $currentArchives[$key].url -or
            $manifest.architecture[$key].hash -cne $currentArchives[$key].hash) {
            throw "Published Scoop asset differs from release metadata for $key."
        }
    }
    # Seed the predecessor even on the first Scoop release.
    $manifest.version = $previousVersion.TrimStart('v')
    $manifest.architecture = $previousArchives
    $manifest | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $manifestPath
    Invoke-Scoop install jacobcxdev/cq
    Assert-ScoopCQ $previousVersion
    Set-Content -LiteralPath $manifestPath -Value $candidate -NoNewline
    Invoke-Scoop update cq
    Assert-ScoopCQ $Version
    Invoke-Scoop uninstall cq
    if ((Test-Path -LiteralPath $shim) -or (Test-Path -LiteralPath (Join-Path $env:SCOOP 'apps/cq'))) {
        throw 'Scoop left a CQ shim or application directory after uninstall.'
    }
    Write-Host "Scoop lifecycle passed on Windows $osArchitecture."
} finally {
    if ((Resolve-Path -LiteralPath $root).Path -ne $root) { throw "Unsafe cleanup path $root." }
    Remove-Item -LiteralPath $root -Recurse -Force
}
