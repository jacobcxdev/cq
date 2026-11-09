param(
    [Parameter(Mandatory = $true)][string]$ReleaseFile,
    [Parameter(Mandatory = $true)][string]$ChecksumsFile,
    [Parameter(Mandatory = $true)][string]$PackageFile,
    [Parameter(Mandatory = $true)][string]$Tag,
    [Parameter(Mandatory = $true)][string]$Source
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
if ($Tag -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$' -or $Source -notmatch '^[0-9a-f]{40}$') {
    throw "a stable tag and exact source commit are required"
}
$release = Get-Content -LiteralPath $ReleaseFile -Raw | ConvertFrom-Json
if ($release.tag_name -ne $Tag -or $release.target_commitish -ne $Source -or
    $release.draft -isnot [bool] -or $release.draft -or $release.prerelease -isnot [bool] -or $release.prerelease) {
    throw "release must be published, stable and bound to the exact tag source"
}
$version = $Tag.TrimStart("v")
$packageName = "cq_${version}_windows_arm64.msi"
if ([IO.Path]::GetFileName($PackageFile) -ne $packageName) {
    throw "public ARM64 package filename differs"
}
foreach ($file in @($PackageFile, $ChecksumsFile)) {
    $name = if ($file -eq $PackageFile) { $packageName } else { "checksums.txt" }
    $assets = @($release.assets | Where-Object { $_.name -eq $name })
    if ($assets.Count -ne 1) { throw "published asset is absent or ambiguous: $name" }
    $asset = $assets[0]
    $expectedURL = "https://github.com/jacobcxdev/cq/releases/download/$Tag/$name"
    if ($asset.id -le 0 -or $asset.state -ne "uploaded" -or $asset.browser_download_url -ne $expectedURL -or
        $asset.digest -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "published asset identity differs: $name"
    }
    $actual = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($asset.digest -ne "sha256:$actual") { throw "published asset bytes differ: $name" }
}
$lines = @(Get-Content -LiteralPath $ChecksumsFile | Where-Object { $_ -match ("^[0-9a-f]{64}  " + [regex]::Escape($packageName) + '$') })
if ($lines.Count -ne 1) { throw "package checksum is absent or ambiguous" }
$expected = ($lines[0] -split '\s+')[0]
$actual = (Get-FileHash -LiteralPath $PackageFile -Algorithm SHA256).Hash.ToLowerInvariant()
if ($expected -ne $actual) { throw "published package checksum differs" }
Write-Host "Verified public ARM64 MSI $Tag at $Source."
