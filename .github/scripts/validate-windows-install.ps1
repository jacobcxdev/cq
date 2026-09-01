param(
    [Parameter(Mandatory = $true)]
    [string]$ManifestPath,

    [string]$PreviousManifestPath = "",

    [Parameter(Mandatory = $true)]
    [string]$ExpectedVersion,

    [string]$PreviousVersion = "",

    [string]$PreviousGoVersion = "",

    [ValidateRange(1024, 65535)]
    [int]$Port = 19280
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$taskPath = "\cq\"
$proxyTask = "\cq\Proxy"
$refreshTask = "\cq\Refresh"
$shellFoldersPath = "Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders"
$environmentPath = "Environment"
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("cq-native-install-" + [Guid]::NewGuid().ToString("N"))
$temporaryLocal = Join-Path $temporaryRoot "local"
$temporaryRoaming = Join-Path $temporaryRoot "roaming"
$temporaryHome = Join-Path $temporaryRoot "home"
$temporaryCodex = Join-Path $temporaryHome ".codex"
$temporaryGoBin = Join-Path $temporaryRoot "gobin"
$probeExecutable = Join-Path $temporaryRoot "native-transport-probe.exe"
$addressFile = Join-Path $temporaryRoot "upstream-address.txt"
$upstreamProcess = $null
$installedCQ = Join-Path $temporaryLocal "Programs\cq\cq.exe"
$installRoot = Split-Path -Parent $installedCQ
$goInstalledCQ = Join-Path $temporaryGoBin "cq.exe"
$wingetSettingsPath = Join-Path $env:LOCALAPPDATA "Packages\Microsoft.DesktopAppInstaller_8wekyb3d8bbwe\LocalState\settings.json"

$shellKey = $null
$environmentKey = $null
$shellSnapshot = @{}
$environmentSnapshot = @{}
$wingetSettingsExisted = Test-Path -LiteralPath $wingetSettingsPath -PathType Leaf
$wingetSettingsBytes = if ($wingetSettingsExisted) { [IO.File]::ReadAllBytes($wingetSettingsPath) } else { $null }
$ownsCQTasks = $false
$ownsWinGetPackage = $false
$processEnvironmentSnapshot = @{
    APPDATA = $env:APPDATA
    LOCALAPPDATA = $env:LOCALAPPDATA
    USERPROFILE = $env:USERPROFILE
    CODEX_HOME = $env:CODEX_HOME
    GOBIN = $env:GOBIN
    PATH = $env:PATH
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

function Invoke-WinGet {
    param([string[]]$Arguments)
    & winget.exe @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "winget failed with exit code $LASTEXITCODE`: $($Arguments -join ' ')"
    }
}

function Invoke-GoRunner {
    param(
        [string]$Version,
        [string]$Action = "install"
    )
    $arguments = @("run", "github.com/jacobcxdev/cq/cmd/cq-install@v$Version")
    if ($Action -eq "uninstall") {
        $arguments += "uninstall"
    }
    $arguments += "--silent"
    & go @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Go installer runner failed with exit code $LASTEXITCODE for v$Version $Action"
    }
}

function Get-CQARPEntries {
    return @(Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue | Where-Object {
        $_.DisplayName -eq "CQ" -and $_.Publisher -eq "jacobcxdev"
    })
}

function Set-PrivateACL {
    param([string]$Path)
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $sddl = "O:{0}G:{0}D:P(A;;FA;;;{0})(A;;FA;;;SY)(A;;FA;;;BA)" -f $sid
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
    param(
        [string]$Executable,
        [string]$Owner
    )
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $status = (& $Executable service status --json | ConvertFrom-Json)
            if ($status.owner -eq $Owner -and $status.proxy.healthy -and $status.proxy.running -and $status.refresh.healthy) {
                return $status
            }
        }
        catch {
        }
        Start-Sleep -Seconds 1
    }
    throw "CQ services did not become healthy"
}

function Wait-Removed {
    param([string]$Path)
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if (-not (Test-Path -LiteralPath $Path)) {
            return
        }
        Start-Sleep -Milliseconds 100
    }
    throw "timed out waiting for removal of $Path"
}

function Assert-SecurityDescriptor {
    param(
        [string]$Value,
        [string]$CurrentSID,
        [bool]$RequireProtected
    )
    $descriptor = [System.Security.AccessControl.RawSecurityDescriptor]::new($Value)
    $protected = ($descriptor.ControlFlags -band [System.Security.AccessControl.ControlFlags]::DiscretionaryAclProtected) -ne 0
    if ($RequireProtected -and -not $protected) {
        throw "scheduler folder DACL is not protected"
    }
    $systemSID = "S-1-5-18"
    $systemFull = $false
    $userFull = $false
    foreach ($ace in $descriptor.DiscretionaryAcl) {
        if ($ace.AceType -ne [System.Security.AccessControl.AceType]::AccessAllowed) {
            throw "scheduler DACL contains non-allow ACE"
        }
        $trustee = $ace.SecurityIdentifier.Value
        if ($trustee -ne $systemSID -and $trustee -ne $CurrentSID) {
            throw "scheduler DACL contains foreign trustee $trustee"
        }
        if ($ace.AccessMask -eq 0x1F01FF) {
            if ($trustee -eq $systemSID) {
                $systemFull = $true
            }
            if ($trustee -eq $CurrentSID) {
                $userFull = $true
            }
        }
    }
    if (-not $systemFull -or -not $userFull) {
        throw "scheduler DACL omits required full-control ACE"
    }
}

function Assert-TaskDefinition {
    param(
        [string]$Name,
        [string]$Arguments,
        [string]$CurrentSID,
        [string]$Executable
    )
    [xml]$definition = Export-ScheduledTask -TaskPath $taskPath -TaskName $Name
    $principal = $definition.Task.Principals.Principal
    $action = $definition.Task.Actions.Exec
    if ($principal.UserId -ne $CurrentSID -or $principal.LogonType -ne "InteractiveToken" -or ($principal.RunLevel -and $principal.RunLevel -ne "LeastPrivilege")) {
        throw "$Name principal differs from current-user authority"
    }
    if ($action.Command -ne $Executable -or $action.Arguments -ne $Arguments) {
        throw "$Name action differs from installed executable"
    }
    $service = New-Object -ComObject "Schedule.Service"
    $service.Connect()
    $registered = $service.GetFolder("\cq").GetTask($Name)
    Assert-SecurityDescriptor -Value ([string]$registered.GetSecurityDescriptor(4)) -CurrentSID $CurrentSID -RequireProtected $false
    if ($Name -eq "Proxy") {
        $instances = $registered.GetInstances(0)
        if ($instances.Count -ne 1 -or [uint32]$instances.Item(1).EnginePID -eq 0) {
            throw "Proxy Task Scheduler instance PID is ambiguous"
        }
        return [uint32]$instances.Item(1).EnginePID
    }
}

function Assert-Installed {
    param(
        [string]$Executable,
        [string]$Version,
        [string]$Owner,
        [bool]$ExpectWindowsMetadata
    )
    if ((& $Executable --version).TrimStart("v") -ne $Version.TrimStart("v")) {
        throw "installed CQ version differs"
    }
    $status = Wait-ServiceStatus -Executable $Executable -Owner $Owner
    $currentSID = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $service = New-Object -ComObject "Schedule.Service"
    $service.Connect()
    Assert-SecurityDescriptor -Value ([string]$service.GetFolder("\cq").GetSecurityDescriptor(4)) -CurrentSID $currentSID -RequireProtected $true
    $managerPID = Assert-TaskDefinition -Name "Proxy" -Arguments "proxy start" -CurrentSID $currentSID -Executable $Executable
    $null = Assert-TaskDefinition -Name "Refresh" -Arguments "refresh" -CurrentSID $currentSID -Executable $Executable
    if ($managerPID -ne $status.proxy.pid) {
        throw "service status PID differs from Task Scheduler EnginePID"
    }
    if ($status.proxy.configured_executable -ne $Executable -or $status.proxy.live_executable -ne $Executable -or $status.proxy.listener -ne "127.0.0.1:$Port") {
        throw "installed process/listener identity differs"
    }
    $ownedProcess = Get-CimInstance Win32_Process -Filter "ProcessId=$($status.proxy.pid)"
    if ($ownedProcess.ExecutablePath -ne $Executable) {
        throw "listener process is not installed CQ"
    }
    if ($ExpectWindowsMetadata) {
        $root = Split-Path -Parent $Executable
        $entries = @(Get-CQARPEntries | Where-Object { $_.DisplayVersion -eq $Version.TrimStart("v") })
        if ($entries.Count -ne 1) {
            throw "CQ MSI registration count is $($entries.Count)"
        }
        $userPath = $environmentKey.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if (($userPath -split ";") -notcontains $root) {
            throw "installer PATH entry is absent"
        }
    }
    return $status
}

function Remove-CQTask {
    param([string]$TaskName)
    $ErrorActionPreference = "Continue"
    & schtasks.exe /End /TN $TaskName 2>$null | Out-Null
    & schtasks.exe /Delete /TN $TaskName /F 2>$null | Out-Null
}

function Remove-CQTaskFolder {
    $ErrorActionPreference = "Continue"
    $service = New-Object -ComObject "Schedule.Service"
    $service.Connect()
    $root = $service.GetFolder("\")
    try {
        $folder = $service.GetFolder("\cq")
        if ($folder.GetTasks(0).Count -eq 0 -and $folder.GetFolders(0).Count -eq 0) {
            $root.DeleteFolder("cq", 0)
        }
    }
    catch {
    }
}

try {
    $ManifestPath = (Resolve-Path -LiteralPath $ManifestPath).Path
    $hasPreviousWinGet = $PreviousManifestPath -ne "" -or $PreviousVersion -ne ""
    $hasPreviousGo = $PreviousGoVersion -ne ""
    if ($hasPreviousWinGet -and ($PreviousManifestPath -eq "" -or $PreviousVersion -eq "")) {
        throw "previous manifest path and version must be supplied together"
    }
    if ($hasPreviousWinGet) {
        $PreviousManifestPath = (Resolve-Path -LiteralPath $PreviousManifestPath).Path
        if ([version]$PreviousVersion -ge [version]$ExpectedVersion) {
            throw "previous version must be older than expected version"
        }
    }
    if ($hasPreviousGo -and [version]$PreviousGoVersion -ge [version]$ExpectedVersion) {
        throw "previous Go runner version must be older than expected version"
    }
    $scheduler = New-Object -ComObject "Schedule.Service"
    $scheduler.Connect()
    try {
        $null = $scheduler.GetFolder("\cq")
        throw "refusing to replace existing \cq Task Scheduler folder"
    }
    catch [System.IO.FileNotFoundException] {
    }
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "refusing to replace existing $taskPath$name task"
        }
    }
    if ((Get-CQARPEntries).Count -ne 0) {
        throw "refusing to replace existing CQ MSI registration"
    }
    $ownsCQTasks = $true
    $ownsWinGetPackage = $true

    New-Item -ItemType Directory -Path $temporaryLocal, $temporaryRoaming, $temporaryHome, $temporaryCodex, $temporaryGoBin -Force | Out-Null
    $shellKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($shellFoldersPath, $true)
    $environmentKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($environmentPath, $true)
    if ($null -eq $shellKey -or $null -eq $environmentKey) {
        throw "current-user registry is unavailable"
    }
    foreach ($name in @("AppData", "Local AppData")) {
        $shellSnapshot[$name] = Save-RegistryValue -Key $shellKey -Name $name
    }
    foreach ($name in @("Path", "CODEX_HOME", "USERPROFILE")) {
        $environmentSnapshot[$name] = Save-RegistryValue -Key $environmentKey -Name $name
    }
    $shellKey.SetValue("AppData", $temporaryRoaming, [Microsoft.Win32.RegistryValueKind]::String)
    $shellKey.SetValue("Local AppData", $temporaryLocal, [Microsoft.Win32.RegistryValueKind]::String)
    $environmentKey.SetValue("CODEX_HOME", $temporaryCodex, [Microsoft.Win32.RegistryValueKind]::String)
    $environmentKey.SetValue("USERPROFILE", $temporaryHome, [Microsoft.Win32.RegistryValueKind]::String)
    $env:APPDATA = $temporaryRoaming
    $env:LOCALAPPDATA = $temporaryLocal
    $env:USERPROFILE = $temporaryHome
    $env:CODEX_HOME = $temporaryCodex
    $env:GOBIN = $temporaryGoBin
    $env:PATH = "$temporaryGoBin;$($env:PATH)"

    & winget.exe settings --enable LocalManifestFiles
    if ($LASTEXITCODE -ne 0) {
        throw "failed to enable WinGet local manifests"
    }
    if ($hasPreviousWinGet) {
        Invoke-WinGet -Arguments @("validate", "--manifest", $PreviousManifestPath)
    }
    Invoke-WinGet -Arguments @("validate", "--manifest", $ManifestPath)

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
    New-Item -ItemType Directory -Path $proxyState -Force | Out-Null
    Set-PrivateTree -Root $temporaryLocal
    & $probeExecutable fixtures --config $proxyConfig --auth $codexAuth --state-root $proxyState --upstream $upstream --port $Port
    if ($LASTEXITCODE -ne 0) {
        throw "failed to write synthetic acceptance fixtures"
    }
    Set-PrivateTree -Root $temporaryRoot

    if ($hasPreviousWinGet) {
        Invoke-WinGet -Arguments @("install", "--manifest", $PreviousManifestPath, "--scope", "user", "--silent", "--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity")
        $null = Assert-Installed -Executable $installedCQ -Version $PreviousVersion -Owner "winget" -ExpectWindowsMetadata $true
        Invoke-WinGet -Arguments @("upgrade", "--manifest", $ManifestPath, "--scope", "user", "--silent", "--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity")
    }
    else {
        Invoke-WinGet -Arguments @("install", "--manifest", $ManifestPath, "--scope", "user", "--silent", "--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity")
    }
    $status = Assert-Installed -Executable $installedCQ -Version $ExpectedVersion -Owner "winget" -ExpectWindowsMetadata $true

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

    & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    if ($LASTEXITCODE -ne 0) {
        throw "installed HTTP/SSE/WebSocket probe failed"
    }

    Invoke-WinGet -Arguments @("uninstall", "--id", "jacobcxdev.cq", "--exact", "--scope", "user", "--silent", "--accept-source-agreements", "--disable-interactivity")
    Wait-Removed -Path $installedCQ
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "scheduled task remains after uninstall"
        }
    }
    if ((Get-CQARPEntries).Count -ne 0) {
        throw "CQ MSI registration remains"
    }
    $userPath = $environmentKey.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if (($userPath -split ";") -contains $installRoot) {
        throw "installer PATH entry remains"
    }

    if ($hasPreviousGo) {
        Invoke-GoRunner -Version $PreviousGoVersion
        $null = Assert-Installed -Executable $goInstalledCQ -Version $PreviousGoVersion -Owner "go" -ExpectWindowsMetadata $false
        Invoke-GoRunner -Version $ExpectedVersion
    }
    else {
        Invoke-GoRunner -Version $ExpectedVersion
    }
    $status = Assert-Installed -Executable $goInstalledCQ -Version $ExpectedVersion -Owner "go" -ExpectWindowsMetadata $false
    & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    if ($LASTEXITCODE -ne 0) {
        throw "Go-runner installed HTTP/SSE/WebSocket probe failed"
    }
    Invoke-GoRunner -Version $ExpectedVersion -Action "uninstall"
    Wait-Removed -Path $goInstalledCQ
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "scheduled task remains after Go-runner uninstall"
        }
    }
}
finally {
    if ($upstreamProcess -and -not $upstreamProcess.HasExited) {
        Stop-Process -Id $upstreamProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if ((Test-Path -LiteralPath $installedCQ) -or (Test-Path -LiteralPath $goInstalledCQ)) {
        Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $installedCQ -or $_.ExecutablePath -eq $goInstalledCQ } | ForEach-Object {
            Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        }
    }
    if ($ownsCQTasks) {
        Remove-CQTask -TaskName $proxyTask
        Remove-CQTask -TaskName $refreshTask
        Remove-CQTaskFolder
    }
    if ($ownsWinGetPackage) {
        foreach ($entry in @(Get-CQARPEntries)) {
            $productCode = [string]$entry.PSChildName
            if ($productCode -match '^\{[0-9A-Fa-f-]{36}\}$') {
                Start-Process -FilePath "msiexec.exe" -ArgumentList @("/x", $productCode, "/qn", "/norestart") -Wait -ErrorAction SilentlyContinue | Out-Null
            }
        }
    }
    if ($environmentKey) {
        foreach ($name in @("Path", "CODEX_HOME", "USERPROFILE")) {
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
    $env:GOBIN = $processEnvironmentSnapshot.GOBIN
    $env:PATH = $processEnvironmentSnapshot.PATH
    if ($wingetSettingsExisted) {
        [IO.File]::WriteAllBytes($wingetSettingsPath, $wingetSettingsBytes)
    }
    else {
        Remove-Item -LiteralPath $wingetSettingsPath -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
