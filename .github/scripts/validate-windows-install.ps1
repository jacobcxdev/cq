param(
    [Parameter(Mandatory = $true)]
    [string]$InstallerPath,

    [Parameter(Mandatory = $true)]
    [string]$ExpectedVersion,

    [string]$PreviousInstallerPath = "",

    [ValidateRange(1024, 65535)]
    [int]$Port = 19280
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$taskPath = "\cq\"
$proxyTask = "\cq\Proxy"
$refreshTask = "\cq\Refresh"
$uninstallRegistryPath = "Software\Microsoft\Windows\CurrentVersion\Uninstall\cq"
$shellFoldersPath = "Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders"
$environmentPath = "Environment"
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("cq-native-install-" + [Guid]::NewGuid().ToString("N"))
$temporaryLocal = Join-Path $temporaryRoot "local"
$temporaryRoaming = Join-Path $temporaryRoot "roaming"
$temporaryHome = Join-Path $temporaryRoot "home"
$temporaryCodex = Join-Path $temporaryRoot "codex"
$probeExecutable = Join-Path $temporaryRoot "native-transport-probe.exe"
$addressFile = Join-Path $temporaryRoot "upstream-address.txt"
$upstreamProcess = $null
$installedCQ = Join-Path $temporaryLocal "Programs\cq\cq.exe"
$installRoot = Split-Path -Parent $installedCQ

$shellKey = $null
$environmentKey = $null
$shellSnapshot = @{}
$environmentSnapshot = @{}
$ownsCQTasks = $false
$ownsUninstallRegistration = $false
$processEnvironmentSnapshot = @{
    APPDATA = $env:APPDATA
    LOCALAPPDATA = $env:LOCALAPPDATA
    USERPROFILE = $env:USERPROFILE
    CODEX_HOME = $env:CODEX_HOME
}

function Save-RegistryValue {
    param(
        [Microsoft.Win32.RegistryKey]$Key,
        [string]$Name
    )
    $value = $Key.GetValue($Name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if ($null -eq $value) {
        return @{ Exists = $false }
    }
    return @{
        Exists = $true
        Value = $value
        Kind = $Key.GetValueKind($Name)
    }
}

function Restore-RegistryValue {
    param(
        [Microsoft.Win32.RegistryKey]$Key,
        [string]$Name,
        [hashtable]$Snapshot
    )
    if ($Snapshot.Exists) {
        $Key.SetValue($Name, $Snapshot.Value, $Snapshot.Kind)
    }
    else {
        $Key.DeleteValue($Name, $false)
    }
}

function Invoke-Installer {
    param([string]$Path)
    & $Path install --owner=winget --silent
    if ($LASTEXITCODE -ne 0) {
        throw "installer failed with exit code $LASTEXITCODE"
    }
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
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $status = (& $installedCQ service status --json | ConvertFrom-Json)
            if ($status.proxy.healthy -and $status.proxy.running -and $status.refresh.healthy) {
                return $status
            }
        }
        catch {
        }
        Start-Sleep -Seconds 1
    }
    throw "CQ services did not become healthy"
}

function Assert-TaskDefinition {
    param(
        [string]$Name,
        [string]$Arguments,
        [string]$CurrentSID
    )
    [xml]$definition = Export-ScheduledTask -TaskPath $taskPath -TaskName $Name
    $principal = $definition.Task.Principals.Principal
    $action = $definition.Task.Actions.Exec
    if ($principal.UserId -ne $CurrentSID -or $principal.LogonType -ne "InteractiveToken" -or $principal.RunLevel -ne "LeastPrivilege") {
        throw "$Name principal differs from current-user authority"
    }
    if ($action.Command -ne $installedCQ -or $action.Arguments -ne $Arguments) {
        throw "$Name action differs from installed executable"
    }
}

function Remove-CQTask {
    param([string]$TaskName)
    & schtasks.exe /End /TN $TaskName 2>$null | Out-Null
    & schtasks.exe /Delete /TN $TaskName /F 2>$null | Out-Null
}

try {
    $InstallerPath = (Resolve-Path -LiteralPath $InstallerPath).Path
    if ($PreviousInstallerPath) {
        $PreviousInstallerPath = (Resolve-Path -LiteralPath $PreviousInstallerPath).Path
    }
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "refusing to replace existing $taskPath$name task"
        }
    }
    if (Test-Path -LiteralPath "HKCU:\$uninstallRegistryPath") {
        throw "refusing to replace existing CQ uninstall registration"
    }
    $ownsCQTasks = $true
    $ownsUninstallRegistration = $true

    New-Item -ItemType Directory -Path $temporaryLocal, $temporaryRoaming, $temporaryHome, $temporaryCodex -Force | Out-Null
    $shellKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($shellFoldersPath, $true)
    $environmentKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($environmentPath, $true)
    if ($null -eq $shellKey -or $null -eq $environmentKey) {
        throw "current-user registry is unavailable"
    }
    foreach ($name in @("AppData", "Local AppData")) {
        $shellSnapshot[$name] = Save-RegistryValue -Key $shellKey -Name $name
    }
    foreach ($name in @("Path", "CODEX_HOME", "CQPathAdded")) {
        $environmentSnapshot[$name] = Save-RegistryValue -Key $environmentKey -Name $name
    }
    $shellKey.SetValue("AppData", $temporaryRoaming, [Microsoft.Win32.RegistryValueKind]::String)
    $shellKey.SetValue("Local AppData", $temporaryLocal, [Microsoft.Win32.RegistryValueKind]::String)
    $environmentKey.SetValue("CODEX_HOME", $temporaryCodex, [Microsoft.Win32.RegistryValueKind]::String)
    $env:APPDATA = $temporaryRoaming
    $env:LOCALAPPDATA = $temporaryLocal
    $env:USERPROFILE = $temporaryHome
    $env:CODEX_HOME = $temporaryCodex

    $repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
    $probeSource = Join-Path $repositoryRoot ".github\scripts\native-transport-probe.go"
    & go build -o $probeExecutable $probeSource
    if ($LASTEXITCODE -ne 0) {
        throw "failed to build native transport probe"
    }
    $upstreamProcess = Start-Process -FilePath $probeExecutable -ArgumentList @("serve", "--address-file", $addressFile) -PassThru -NoNewWindow
    Wait-File -Path $addressFile
    $upstream = (Get-Content -LiteralPath $addressFile -Raw).Trim()
    $proxyConfig = Join-Path $temporaryRoaming "cq\proxy.json"
    $proxyState = Join-Path $temporaryLocal "cq\state\proxy-resilience"
    $codexAuth = Join-Path $temporaryCodex "auth.json"
    & $probeExecutable fixtures --config $proxyConfig --auth $codexAuth --state-root $proxyState --upstream $upstream --port $Port
    if ($LASTEXITCODE -ne 0) {
        throw "failed to write synthetic acceptance fixtures"
    }

    if ($PreviousInstallerPath) {
        Invoke-Installer -Path $PreviousInstallerPath
    }
    else {
        Invoke-Installer -Path $InstallerPath
    }
    Invoke-Installer -Path $InstallerPath

    if ((& $installedCQ --version).TrimStart("v") -ne $ExpectedVersion.TrimStart("v")) {
        throw "installed CQ version differs"
    }
    $status = Wait-ServiceStatus
    $currentSID = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    Assert-TaskDefinition -Name "Proxy" -Arguments "proxy start" -CurrentSID $currentSID
    Assert-TaskDefinition -Name "Refresh" -Arguments "refresh" -CurrentSID $currentSID

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

    $arp = Get-ItemProperty -LiteralPath "HKCU:\$uninstallRegistryPath"
    if ($arp.DisplayVersion -ne $ExpectedVersion.TrimStart("v") -or $arp.InstallLocation -ne $installRoot -or $arp.CQPathAdded -ne 1) {
        throw "Add/Remove Programs metadata differs"
    }
    $userPath = $environmentKey.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if (($userPath -split ";") -notcontains $installRoot) {
        throw "installer PATH entry is absent"
    }
    if ($status.proxy.configured_executable -ne $installedCQ -or $status.proxy.live_executable -ne $installedCQ -or $status.proxy.listener -ne "127.0.0.1:$Port") {
        throw "installed process/listener identity differs"
    }
    $ownedProcess = Get-CimInstance Win32_Process -Filter "ProcessId=$($status.proxy.pid)"
    if ($ownedProcess.ExecutablePath -ne $installedCQ) {
        throw "listener process is not installed CQ"
    }
    & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    if ($LASTEXITCODE -ne 0) {
        throw "installed HTTP/SSE/WebSocket probe failed"
    }

    & cmd.exe /d /c (Join-Path $installRoot "uninstall.cmd")
    if ($LASTEXITCODE -ne 0) {
        throw "durable uninstaller failed"
    }
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "scheduled task remains after uninstall"
        }
    }
    if (Test-Path -LiteralPath "HKCU:\$uninstallRegistryPath") {
        throw "uninstall registration remains"
    }
    $userPath = $environmentKey.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if (($userPath -split ";") -contains $installRoot) {
        throw "installer PATH entry remains"
    }
}
finally {
    if ($upstreamProcess -and -not $upstreamProcess.HasExited) {
        Stop-Process -Id $upstreamProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $installedCQ) {
        Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $installedCQ } | ForEach-Object {
            Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        }
    }
    if ($ownsCQTasks) {
        Remove-CQTask -TaskName $proxyTask
        Remove-CQTask -TaskName $refreshTask
    }
    if ($ownsUninstallRegistration) {
        Remove-Item -LiteralPath "HKCU:\$uninstallRegistryPath" -Recurse -Force -ErrorAction SilentlyContinue
    }
    if ($environmentKey) {
        foreach ($name in @("Path", "CODEX_HOME", "CQPathAdded")) {
            if ($environmentSnapshot.ContainsKey($name)) {
                Restore-RegistryValue -Key $environmentKey -Name $name -Snapshot $environmentSnapshot[$name]
            }
        }
        $environmentKey.Close()
    }
    if ($shellKey) {
        foreach ($name in @("AppData", "Local AppData")) {
            if ($shellSnapshot.ContainsKey($name)) {
                Restore-RegistryValue -Key $shellKey -Name $name -Snapshot $shellSnapshot[$name]
            }
        }
        $shellKey.Close()
    }
    $env:APPDATA = $processEnvironmentSnapshot.APPDATA
    $env:LOCALAPPDATA = $processEnvironmentSnapshot.LOCALAPPDATA
    $env:USERPROFILE = $processEnvironmentSnapshot.USERPROFILE
    $env:CODEX_HOME = $processEnvironmentSnapshot.CODEX_HOME
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
