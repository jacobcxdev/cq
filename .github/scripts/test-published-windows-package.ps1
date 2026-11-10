$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$root = Join-Path ([IO.Path]::GetTempPath()) ("cq-public-package-test-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $root | Out-Null
try {
    $package = Join-Path $root "cq_0.48.0_windows_arm64.msi"
    $checksums = Join-Path $root "checksums.txt"
    $releasePath = Join-Path $root "release.json"
    [IO.File]::WriteAllText($package, "synthetic MSI fixture")
    $digest = (Get-FileHash -LiteralPath $package -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText($checksums, "$digest  cq_0.48.0_windows_arm64.msi`n")
    $checksumDigest = (Get-FileHash -LiteralPath $checksums -Algorithm SHA256).Hash.ToLowerInvariant()
    $release = @{
        tag_name = "v0.48.0"; target_commitish = ("a" * 40); draft = $false; prerelease = $false
        assets = @(
            @{ id = 1; name = "cq_0.48.0_windows_arm64.msi"; state = "uploaded"; digest = "sha256:$digest"; browser_download_url = "https://github.com/jacobcxdev/cq/releases/download/v0.48.0/cq_0.48.0_windows_arm64.msi" },
            @{ id = 2; name = "checksums.txt"; state = "uploaded"; digest = "sha256:$checksumDigest"; browser_download_url = "https://github.com/jacobcxdev/cq/releases/download/v0.48.0/checksums.txt" }
        )
    }
    function Invoke-Guard {
        $release | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $releasePath -Encoding utf8
        & (Join-Path $PSScriptRoot "verify-published-windows-package.ps1") -ReleaseFile $releasePath -ChecksumsFile $checksums -PackageFile $package -Tag "v0.48.0" -Source ("a" * 40)
    }
    Invoke-Guard
    $valid = $release | ConvertTo-Json -Depth 10
    foreach ($mutation in @(
        { $release.draft = $true }, { $release.prerelease = $true }, { $release.target_commitish = ("b" * 40) },
        { $release.assets[0].digest = "sha256:" + ("c" * 64) }, { $release.assets[1].digest = "sha256:" + ("c" * 64) },
        { $release.assets[0].state = "new" }, { $release.assets[0].browser_download_url = "https://example.invalid/package.msi" },
        { $release.assets += $release.assets[0] }, { $release.draft = 0 }
    )) {
        $release = $valid | ConvertFrom-Json -AsHashtable
        & $mutation
        $rejected = $false
        try { Invoke-Guard } catch { $rejected = $true }
        if (-not $rejected) { throw "invalid public package metadata was accepted" }
    }
    foreach ($text in @(
        "$digest  different-package.msi`n",
        "$digest  cq_0.48.0_windows_arm64.msi`n$digest  cq_0.48.0_windows_arm64.msi`n",
        (("c" * 64) + "  cq_0.48.0_windows_arm64.msi`n")
    )) {
        $release = $valid | ConvertFrom-Json -AsHashtable
        [IO.File]::WriteAllText($checksums, $text)
        $release.assets[1].digest = "sha256:" + (Get-FileHash -LiteralPath $checksums -Algorithm SHA256).Hash.ToLowerInvariant()
        $rejected = $false
        try { Invoke-Guard } catch { $rejected = $true }
        if (-not $rejected) { throw "invalid published checksum entries were accepted" }
    }
    [IO.File]::WriteAllText($checksums, "$digest  cq_0.48.0_windows_arm64.msi`n")
    $release = $valid | ConvertFrom-Json -AsHashtable
    [IO.File]::AppendAllText($package, "changed")
    $rejected = $false
    try { Invoke-Guard } catch { $rejected = $true }
    if (-not $rejected) { throw "changed public package bytes were accepted" }
    Write-Host "Published Windows package regression checks passed."
}
finally {
    Remove-Item -LiteralPath $root -Recurse -Force
}
