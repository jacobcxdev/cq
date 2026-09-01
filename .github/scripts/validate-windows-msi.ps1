param(
    [Parameter(Mandatory = $true)]
    [string]$PreviousMSI,

    [Parameter(Mandatory = $true)]
    [string]$CurrentMSI,

    [Parameter(Mandatory = $true)]
    [string]$PreviousVersion,

    [Parameter(Mandatory = $true)]
    [string]$CurrentVersion,

    [ValidateRange(1024, 65535)]
    [int]$Port = 19280
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$taskPath = "\cq\"
$proxyTask = "\cq\Proxy"
$refreshTask = "\cq\Refresh"
$local = [Environment]::GetFolderPath("LocalApplicationData")
$roaming = [Environment]::GetFolderPath("ApplicationData")
$homePath = [Environment]::GetFolderPath("UserProfile")
$installedCQ = Join-Path $local "Programs\cq\cq.exe"
$installRoot = Split-Path -Parent $installedCQ
$codexRoot = Join-Path $homePath ".codex"
$roamingCQ = Join-Path $roaming "cq"
$localCQ = Join-Path $local "cq"
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("cq-msi-validation-" + [Guid]::NewGuid().ToString("N"))
$probeExecutable = Join-Path $temporaryRoot "native-transport-probe.exe"
$addressFile = Join-Path $temporaryRoot "upstream-address.txt"
$upstreamProcess = $null
$ownsState = $false
$ownsTasks = $false

function Invoke-MSI {
    param(
        [ValidateSet("install", "uninstall")]
        [string]$Action,
        [string]$Path,
        [string]$Log
    )
    $verb = if ($Action -eq "install") { "/i" } else { "/x" }
    $arguments = @($verb, ('"' + $Path + '"'), "/qn", "/norestart", "/l*v", ('"' + $Log + '"'))
    $process = Start-Process -FilePath "msiexec.exe" -ArgumentList $arguments -Wait -PassThru
    if ($process.ExitCode -ne 0 -and $process.ExitCode -ne 3010) {
        if (Test-Path -LiteralPath $Log -PathType Leaf) {
            Get-Content -LiteralPath $Log -Tail 160 | Write-Error
        }
        throw "msiexec $Action failed with exit code $($process.ExitCode)"
    }
}

function Set-PrivateACL {
    param([string]$Path)
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $sddl = "O:${sid}G:${sid}D:P(A;;FA;;;${sid})(A;;FA;;;SY)(A;;FA;;;BA)"
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer) {
        $security = [System.Security.AccessControl.DirectorySecurity]::new()
    }
    else {
        $security = [System.Security.AccessControl.FileSecurity]::new()
    }
    $security.SetSecurityDescriptorSddlForm($sddl)
    Set-Acl -LiteralPath $Path -AclObject $security
}

function Set-PrivateTree {
    param([string]$Root)
    Get-ChildItem -LiteralPath $Root -Force -Recurse | Sort-Object { $_.FullName.Length } -Descending | ForEach-Object {
        Set-PrivateACL -Path $_.FullName
    }
    Set-PrivateACL -Path $Root
}

function Wait-File {
    param([string]$Path)
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if (Test-Path -LiteralPath $Path -PathType Leaf) {
            return
        }
        Start-Sleep -Milliseconds 100
    }
    throw "timed out waiting for $Path"
}

function Wait-ServiceStatus {
    param([string]$Version)
    for ($attempt = 0; $attempt -lt 90; $attempt++) {
        try {
            $reportedVersion = (& $installedCQ --version).TrimStart("v")
            $status = (& $installedCQ service status --json | ConvertFrom-Json)
            if ($reportedVersion -eq $Version -and $status.owner -eq "winget" -and $status.proxy.running -and $status.proxy.healthy -and $status.refresh.healthy) {
                return $status
            }
        }
        catch {
        }
        Start-Sleep -Seconds 1
    }
    throw "CQ $Version services did not become healthy"
}

function Assert-Installed {
    param([string]$Version)
    $status = Wait-ServiceStatus -Version $Version
    if ($status.proxy.configured_executable -ne $installedCQ -or $status.proxy.live_executable -ne $installedCQ -or $status.proxy.listener -ne "127.0.0.1:$Port") {
        throw "installed CQ process identity differs"
    }
    foreach ($name in @("Proxy", "Refresh")) {
        $task = Get-ScheduledTask -TaskPath $taskPath -TaskName $name
        if ($task.Actions.Execute -ne $installedCQ) {
            throw "$name task executable differs"
        }
    }
    $entries = @(Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue | Where-Object {
        $_.DisplayName -eq "CQ" -and $_.Publisher -eq "jacobcxdev" -and $_.DisplayVersion -eq $Version
    })
    if ($entries.Count -ne 1) {
        throw "CQ MSI registration count is $($entries.Count)"
    }
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ";") -notcontains $installRoot) {
        throw "CQ MSI PATH entry is absent"
    }
    & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    if ($LASTEXITCODE -ne 0) {
        throw "installed HTTP/SSE/WebSocket probe failed"
    }
    return $status
}

function Remove-CQTasks {
    $ErrorActionPreference = "Continue"
    foreach ($task in @($proxyTask, $refreshTask)) {
        & schtasks.exe /End /TN $task 2>$null | Out-Null
        & schtasks.exe /Delete /TN $task /F 2>$null | Out-Null
    }
    try {
        $scheduler = New-Object -ComObject "Schedule.Service"
        $scheduler.Connect()
        $folder = $scheduler.GetFolder("\cq")
        if ($folder.GetTasks(0).Count -eq 0 -and $folder.GetFolders(0).Count -eq 0) {
            $scheduler.GetFolder("\").DeleteFolder("cq", 0)
        }
    }
    catch {
    }
}

try {
    $PreviousMSI = (Resolve-Path -LiteralPath $PreviousMSI).Path
    $CurrentMSI = (Resolve-Path -LiteralPath $CurrentMSI).Path
    if ([version]$PreviousVersion -ge [version]$CurrentVersion) {
        throw "previous MSI version must be older"
    }
    foreach ($path in @($installedCQ, $codexRoot, $roamingCQ, $localCQ)) {
        if (Test-Path -LiteralPath $path) {
            throw "refusing to replace existing validation path $path"
        }
    }
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "refusing to replace existing $taskPath$name task"
        }
    }
    if (Get-NetTCPConnection -LocalAddress "127.0.0.1" -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
        throw "refusing to replace existing listener on port $Port"
    }
    $ownsState = $true
    $ownsTasks = $true
    New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null

    $repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
    $probeSource = Join-Path $repositoryRoot ".github\scripts\native-transport-probe.go"
    & go build -o $probeExecutable $probeSource
    if ($LASTEXITCODE -ne 0) {
        throw "failed to build native transport probe"
    }
    $upstreamProcess = Start-Process -FilePath $probeExecutable -ArgumentList @("serve", "--address-file", $addressFile) -PassThru -NoNewWindow
    Wait-File -Path $addressFile
    $upstream = (Get-Content -LiteralPath $addressFile -Raw).Trim()
    $proxyConfig = Join-Path $roamingCQ "proxy.json"
    $proxyState = Join-Path $localCQ "state\proxy-resilience"
    $codexAuth = Join-Path $codexRoot "auth.json"
    & $probeExecutable fixtures --config $proxyConfig --auth $codexAuth --state-root $proxyState --upstream $upstream --port $Port
    if ($LASTEXITCODE -ne 0) {
        throw "failed to write synthetic acceptance fixtures"
    }
    foreach ($root in @($codexRoot, $roamingCQ, $localCQ)) {
        Set-PrivateTree -Root $root
    }

    Invoke-MSI -Action install -Path $PreviousMSI -Log (Join-Path $temporaryRoot "install-previous.log")
    $null = Assert-Installed -Version $PreviousVersion

    Invoke-MSI -Action install -Path $CurrentMSI -Log (Join-Path $temporaryRoot "upgrade-current.log")
    $status = Assert-Installed -Version $CurrentVersion
    if ($status.proxy.pid -eq 0) {
        throw "upgraded proxy has no manager PID"
    }

    Start-ScheduledTask -TaskPath $taskPath -TaskName "Refresh"
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        $refreshInfo = Get-ScheduledTask -TaskPath $taskPath -TaskName "Refresh" | Get-ScheduledTaskInfo
        if ($refreshInfo.LastRunTime.Year -gt 2000 -and [uint32]$refreshInfo.LastTaskResult -eq 0) {
            break
        }
        Start-Sleep -Milliseconds 500
    }
    if ([uint32]$refreshInfo.LastTaskResult -ne 0) {
        throw "refresh task did not complete successfully"
    }

    Invoke-MSI -Action uninstall -Path $CurrentMSI -Log (Join-Path $temporaryRoot "uninstall-current.log")
    for ($attempt = 0; $attempt -lt 100 -and (Test-Path -LiteralPath $installedCQ); $attempt++) {
        Start-Sleep -Milliseconds 100
    }
    if (Test-Path -LiteralPath $installedCQ) {
        throw "CQ executable remains after MSI uninstall"
    }
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "$name task remains after MSI uninstall"
        }
    }
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ";") -contains $installRoot) {
        throw "CQ MSI PATH entry remains after uninstall"
    }
}
finally {
    if ($upstreamProcess -and -not $upstreamProcess.HasExited) {
        Stop-Process -Id $upstreamProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if ($ownsTasks) {
        foreach ($msi in @($CurrentMSI, $PreviousMSI)) {
            if ($msi -and (Test-Path -LiteralPath $msi -PathType Leaf)) {
                $process = Start-Process -FilePath "msiexec.exe" -ArgumentList @("/x", ('"' + $msi + '"'), "/qn", "/norestart") -Wait -PassThru
            }
        }
        Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $installedCQ } | ForEach-Object {
            Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        }
        Remove-CQTasks
    }
    if ($ownsState) {
        foreach ($path in @($codexRoot, $roamingCQ, $localCQ)) {
            if (Test-Path -LiteralPath $path) {
                Remove-Item -LiteralPath $path -Recurse -Force
            }
        }
    }
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
