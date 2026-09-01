param(
    [Parameter(Mandatory = $true)]
    [string]$Executable,

    [Parameter(Mandatory = $true)]
    [string]$Version,

    [Parameter(Mandatory = $true)]
    [ValidateSet("amd64", "arm64")]
    [string]$Architecture,

    [Parameter(Mandatory = $true)]
    [string]$Output,

    [Parameter(Mandatory = $true)]
    [string]$Wix
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') {
    throw "MSI version must be stable semantic version"
}
$Executable = (Resolve-Path -LiteralPath $Executable).Path
$Wix = (Resolve-Path -LiteralPath $Wix).Path
$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$source = Join-Path $repositoryRoot "packaging\windows\cq.wxs"
$outputParent = Split-Path -Parent $Output
if (-not (Test-Path -LiteralPath $outputParent -PathType Container)) {
    New-Item -ItemType Directory -Path $outputParent -Force | Out-Null
}
$Output = [IO.Path]::GetFullPath($Output)

if ($Architecture -eq "amd64") {
    $wixArchitecture = "x64"
    $componentGuid = "{B40777D1-C8BB-45A5-99BF-135B47E89D28}"
}
else {
    $wixArchitecture = "arm64"
    $componentGuid = "{47757F03-FE29-43EC-BADB-567FB0D93125}"
}

& $Wix build $source -arch $wixArchitecture `
    -d "CQVersion=$Version" `
    -d "CQExecutable=$Executable" `
    -d "CQComponentGuid=$componentGuid" `
    -out $Output
if ($LASTEXITCODE -ne 0) {
    throw "WiX build failed with exit code $LASTEXITCODE"
}
& $Wix msi validate $Output
if ($LASTEXITCODE -ne 0) {
    throw "WiX validation failed with exit code $LASTEXITCODE"
}
if (-not (Test-Path -LiteralPath $Output -PathType Leaf)) {
    throw "WiX did not create MSI output"
}
