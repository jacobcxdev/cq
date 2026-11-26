param(
    [Parameter(Mandatory = $true)]
    [string]$PreviousMSI,

    [Parameter(Mandatory = $true)]
    [string]$CurrentMSI,

    [Parameter(Mandatory = $true)]
    [string]$PreviousVersion,

    [Parameter(Mandatory = $true)]
    [string]$CurrentVersion,

    [string]$LegacyPatch = "",

    [switch]$VerifyLegacyRollback,

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
$serviceProbe = Join-Path $temporaryRoot "cq-service-probe.exe"
$addressFile = Join-Path $temporaryRoot "upstream-address.txt"
$upstreamProcess = $null
$foregroundProxy = $null
$ownsState = $false
$ownsTasks = $false

function Invoke-MSI {
    param(
        [ValidateSet("install", "uninstall", "patch")]
        [string]$Action,
        [string]$Path,
        [string]$Log,
        [int[]]$ExpectedExitCodes = @(0, 3010)
    )
    $verb = switch ($Action) {
        "install" { "/i" }
        "uninstall" { "/x" }
        "patch" { "/update" }
    }
    $arguments = @($verb, ('"' + $Path + '"'), "/qn", "/norestart", "/l*v", ('"' + $Log + '"'))
    if ($Action -eq "patch") {
        # Rewrite the per-user registry marker without reinstalling a present executable.
        $arguments += @("REINSTALL=ALL", "REINSTALLMODE=pmus")
    }
    $process = Start-Process -FilePath "msiexec.exe" -ArgumentList $arguments -Wait -PassThru
    if ($process.ExitCode -notin $ExpectedExitCodes) {
        if (Test-Path -LiteralPath $Log -PathType Leaf) {
            Get-Content -LiteralPath $Log -Tail 160 | Write-Error -ErrorAction Continue
        }
        throw "msiexec $Action failed with exit code $($process.ExitCode)"
    }
}

function New-RollbackFixture {
    param([string]$Source, [string]$Destination)
    Copy-Item -LiteralPath $Source -Destination $Destination
    $installer = New-Object -ComObject "WindowsInstaller.Installer"
    try {
        $database = $installer.OpenDatabase($Destination, 1)
        try {
            # RemoveExistingProducts has run by this point. Fail before the
            # upgrade can commit so the predecessor's rollback action must run.
            foreach ($statement in @(
                'INSERT INTO `CustomAction` (`Action`, `Type`, `Target`) VALUES (''CQValidationForceRollback'', 19, ''CQ validation requested installer rollback'')',
                'INSERT INTO `InstallExecuteSequence` (`Action`, `Condition`, `Sequence`) VALUES (''CQValidationForceRollback'', ''NOT Installed'', 6599)'
            )) {
                $view = $database.OpenView($statement)
                try { [void]$view.Execute() }
                finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view) }
            }
            $summary = $database.SummaryInformation(1)
            try {
                $summary.Property(9) = [guid]::NewGuid().ToString("B").ToUpperInvariant()
                [void]$summary.Persist()
            }
            finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($summary) }
            [void]$database.Commit()
        }
        finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database) }
    }
    finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer) }
}

function Assert-PatchedLegacyUninstall {
    param([string]$Log)
    if (-not (Select-String -LiteralPath $Log -Pattern 'CustomActionSchedule\(Action=UninstallCQServices,ActionType=1026,.*Target=service uninstall --owner=winget --legacy-msi-executable=' -Quiet)) {
        Select-String -LiteralPath $Log -Pattern 'UninstallCQServices|RollbackLegacyCQServices' | Select-Object -Last 30 | ForEach-Object { Write-Host $_.Line }
        throw "upgrade did not execute the patched legacy uninstall action"
    }
}

function Assert-LegacyPatchRegistered {
    param([string]$ProductCode, [string]$PatchCode)
    $marker = Get-ItemPropertyValue -LiteralPath "HKCU:\Software\jacobcxdev\CQ\Installer" -Name "LegacyQuietUpgrade"
    if ($marker -ne $PatchCode) {
        throw "migration patch registration marker differs"
    }
    $installer = New-Object -ComObject "WindowsInstaller.Installer"
    try {
        $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        $patch = $installer.Patch($PatchCode, $ProductCode, $sid, 2)
        try {
            # Use IDispatch directly because PowerShell cannot adapt the Patch object's properties.
            $state = $patch.GetType().InvokeMember("State", [Reflection.BindingFlags]::GetProperty, $null, $patch, $null)
        }
        finally {
            [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($patch)
        }
    }
    finally {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
    }
    if ($state -ne 1) {
        throw "the current-user CQ legacy patch is not registered as applied"
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

function Get-CQMSIRegistrationCount {
    param([string]$Version)
    $installer = New-Object -ComObject "WindowsInstaller.Installer"
    $count = 0
    try {
        foreach ($product in @($installer.ProductsEx("", "", 2))) {
            try {
                if ($product.InstallProperty("InstalledProductName") -eq "CQ" -and
                    $product.InstallProperty("Publisher") -eq "jacobcxdev" -and
                    $product.InstallProperty("VersionString") -eq $Version) {
                    $count++
                }
            }
            finally {
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($product)
            }
        }
    }
    finally {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
    }
    return $count
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
    $registrationCount = Get-CQMSIRegistrationCount -Version $Version
    if ($registrationCount -ne 1) {
        throw "CQ MSI registration count is $registrationCount"
    }
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $normalisedInstallRoot = $installRoot -replace '[\\/]+$', ''
    $normalisedUserPath = @($userPath -split ";" | ForEach-Object { $_ -replace '[\\/]+$', '' })
    if ($normalisedUserPath -notcontains $normalisedInstallRoot) {
        throw "CQ MSI PATH entry is absent"
    }
    $probeOutput = & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    $probeExitCode = $LASTEXITCODE
    if ($probeExitCode -ne 0) {
        throw "installed HTTP/SSE/WebSocket probe failed"
    }
    $probeOutput | Write-Host
    return $status
}

function Remove-CQTasks {
    $ErrorActionPreference = "Continue"
    foreach ($name in @("Proxy", "Refresh")) {
        Stop-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskPath $taskPath -TaskName $name -Confirm:$false -ErrorAction SilentlyContinue
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
    $legacyMigration = $PreviousVersion -eq "0.53.5"
    if ($legacyMigration -ne ($LegacyPatch -ne "")) {
        throw "a legacy migration patch is required only for the published 0.53.5 predecessor"
    }
    if ($VerifyLegacyRollback -and -not $legacyMigration) {
        throw "forced rollback verification requires the published 0.53.5 predecessor"
    }
    if ($legacyMigration) {
        $LegacyPatch = (Resolve-Path -LiteralPath $LegacyPatch).Path
        $architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
        $legacy = switch ($architecture) {
            "x64" {
                @{
                    Filename = "cq_0.53.5_windows_amd64.msi"
                    SHA256 = "4ae8ac26ee0211a13f57e8796e6158183516ead5fe34efb1061cb6426e80ff37"
                    ProductCode = "{35BC2A8A-8B84-4E6D-9AF2-78B5485354EC}"
                    PatchCode = "{67038060-1859-4ABF-B9B7-7AD86C6290E7}"
                }
            }
            "arm64" {
                @{
                    Filename = "cq_0.53.5_windows_arm64.msi"
                    SHA256 = "5ee1e0855905c6bdb2cf8ed59b81cd4671b8d278a790215152a4b85df34bba12"
                    ProductCode = "{845594B7-7E46-4694-8671-8D688B9F747B}"
                    PatchCode = "{D268364B-6589-4C12-A47A-3059DBE180A0}"
                }
            }
            default { throw "unsupported Windows architecture $architecture" }
        }
        if ([IO.Path]::GetFileName($PreviousMSI) -ne $legacy.Filename -or
            (Get-FileHash -LiteralPath $PreviousMSI -Algorithm SHA256).Hash -ne $legacy.SHA256) {
            throw "previous MSI differs from the published 0.53.5 $architecture package"
        }
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
    & go build -ldflags "-X main.version=$PreviousVersion" -o $serviceProbe ./cmd/cq
    if ($LASTEXITCODE -ne 0) {
        throw "failed to build direct service lifecycle probe"
    }

    Invoke-MSI -Action install -Path $CurrentMSI -Log (Join-Path $temporaryRoot "install-unconfigured.log")
    $null = Wait-ServiceStatus -Version $CurrentVersion
    Invoke-MSI -Action uninstall -Path $CurrentMSI -Log (Join-Path $temporaryRoot "uninstall-unconfigured.log")
    if (Test-Path -LiteralPath $installedCQ) {
        throw "CQ executable remains after unconfigured MSI uninstall"
    }
    foreach ($name in @("Proxy", "Refresh")) {
        if (Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue) {
            throw "$name task remains after unconfigured MSI uninstall"
        }
    }
    foreach ($path in @($codexRoot, $roamingCQ, $localCQ)) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -LiteralPath $path -Recurse -Force
        }
    }

    $upstreamProcess = Start-Process -FilePath $probeExecutable -ArgumentList @("serve", "--address-file", $addressFile) -PassThru -NoNewWindow
    Wait-File -Path $addressFile
    $upstream = (Get-Content -LiteralPath $addressFile -Raw).Trim()
    $proxyConfig = Join-Path $roamingCQ "proxy.json"
    $proxyState = Join-Path $localCQ "state\proxy-resilience"
    $codexAuth = Join-Path $codexRoot "auth.json"
    New-Item -ItemType Directory -Path $proxyState -Force | Out-Null
    Set-PrivateTree -Root $localCQ
    & $probeExecutable fixtures --config $proxyConfig --auth $codexAuth --state-root $proxyState --upstream $upstream --port $Port
    if ($LASTEXITCODE -ne 0) {
        throw "failed to write synthetic acceptance fixtures"
    }
    foreach ($root in @($codexRoot, $roamingCQ, $localCQ)) {
        Set-PrivateTree -Root $root
    }

    $foregroundStdout = Join-Path $temporaryRoot "foreground-proxy.stdout.log"
    $foregroundStderr = Join-Path $temporaryRoot "foreground-proxy.stderr.log"
    $foregroundProxy = Start-Process -FilePath $serviceProbe -ArgumentList @("proxy", "start") -RedirectStandardOutput $foregroundStdout -RedirectStandardError $foregroundStderr -PassThru -NoNewWindow
    $foregroundReady = $false
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if ($foregroundProxy.HasExited) {
            break
        }
        if (Get-NetTCPConnection -LocalAddress "127.0.0.1" -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
            $foregroundReady = $true
            break
        }
        Start-Sleep -Milliseconds 100
    }
    if (-not $foregroundReady) {
        foreach ($log in @($foregroundStdout, $foregroundStderr)) {
            if (Test-Path -LiteralPath $log -PathType Leaf) {
                Get-Content -LiteralPath $log | ForEach-Object { Write-Host "foreground CQ: $_" }
            }
        }
        throw "foreground CQ proxy failed to start"
    }
    & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    if ($LASTEXITCODE -ne 0) {
        throw "foreground CQ proxy transport probe failed"
    }
    Stop-Process -Id $foregroundProxy.Id -Force
    $foregroundProxy.WaitForExit()
    $foregroundProxy = $null
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if (-not (Get-NetTCPConnection -LocalAddress "127.0.0.1" -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)) {
            break
        }
        Start-Sleep -Milliseconds 100
    }

    & $serviceProbe service install --owner=winget
    if ($LASTEXITCODE -ne 0) {
        throw "direct CQ service install failed"
    }
    $directStatus = (& $serviceProbe service status --json | ConvertFrom-Json)
    if ($LASTEXITCODE -ne 0 -or $directStatus.owner -ne "winget" -or -not $directStatus.proxy.running -or -not $directStatus.proxy.healthy -or -not $directStatus.refresh.healthy) {
        throw "direct CQ service lifecycle is unhealthy"
    }
    if ($directStatus.proxy.configured_executable -ne $serviceProbe -or $directStatus.proxy.live_executable -ne $serviceProbe -or $directStatus.proxy.listener -ne "127.0.0.1:$Port") {
        throw "direct CQ service process identity differs"
    }
    & $probeExecutable probe --address "http://127.0.0.1:$Port" --token "cq-native-local"
    if ($LASTEXITCODE -ne 0) {
        throw "direct CQ service transport probe failed"
    }
    & $serviceProbe service uninstall --owner=winget
    if ($LASTEXITCODE -ne 0) {
        throw "direct CQ service uninstall failed"
    }

    Invoke-MSI -Action install -Path $PreviousMSI -Log (Join-Path $temporaryRoot "install-previous.log")
    $previous = Assert-Installed -Version $PreviousVersion
    if ($legacyMigration) {
        $oldDigest = (Get-FileHash -LiteralPath $installedCQ -Algorithm SHA256).Hash
        $guardLog = Join-Path $temporaryRoot "unpatched-upgrade.log"
        Invoke-MSI -Action install -Path $CurrentMSI -Log $guardLog -ExpectedExitCodes 1603
        if (-not (Select-String -LiteralPath $guardLog -Pattern "Apply the CQ v0.53.5 quiet upgrade patch before installing this version." -SimpleMatch -Quiet)) {
            throw "unpatched legacy upgrade failed without the migration guard"
        }
        $unpatched = Assert-Installed -Version $PreviousVersion
        if ((Get-FileHash -LiteralPath $installedCQ -Algorithm SHA256).Hash -ne $oldDigest -or $unpatched.proxy.pid -ne $previous.proxy.pid -or
            (Get-CQMSIRegistrationCount -Version $CurrentVersion) -ne 0) {
            throw "unpatched legacy upgrade changed the previous executable or its services"
        }

        Invoke-MSI -Action patch -Path $LegacyPatch -Log (Join-Path $temporaryRoot "patch-previous.log") -ExpectedExitCodes 0
        Assert-LegacyPatchRegistered -ProductCode $legacy.ProductCode -PatchCode $legacy.PatchCode
        $patched = Assert-Installed -Version $PreviousVersion
        if ((Get-FileHash -LiteralPath $installedCQ -Algorithm SHA256).Hash -ne $oldDigest -or $patched.proxy.pid -ne $previous.proxy.pid) {
            throw "legacy migration patch replaced the installed executable or restarted the proxy"
        }
        Write-Host "Published CQ $PreviousVersion migration patch applied without replacing the executable or restarting the proxy."
        if ($VerifyLegacyRollback) {
            $rollbackMSI = Join-Path $temporaryRoot "rollback-fixture.msi"
            $rollbackLog = Join-Path $temporaryRoot "forced-rollback.log"
            New-RollbackFixture -Source $CurrentMSI -Destination $rollbackMSI
            Invoke-MSI -Action install -Path $rollbackMSI -Log $rollbackLog -ExpectedExitCodes 1603
            Assert-PatchedLegacyUninstall -Log $rollbackLog
            if (-not (Select-String -LiteralPath $rollbackLog -Pattern 'CQ validation requested installer rollback' -Quiet) -or
                -not (Select-String -LiteralPath $rollbackLog -Pattern 'CustomActionSchedule\(Action=RollbackLegacyCQServices,ActionType=1282,' -Quiet)) {
                Select-String -LiteralPath $rollbackLog -Pattern 'CQValidationForceRollback|RollbackLegacyCQServices' | Select-Object -Last 30 | ForEach-Object { Write-Host $_.Line }
                throw "legacy migration did not reach the expected rollback action"
            }
            Assert-LegacyPatchRegistered -ProductCode $legacy.ProductCode -PatchCode $legacy.PatchCode
            $restored = Assert-Installed -Version $PreviousVersion
            if ((Get-FileHash -LiteralPath $installedCQ -Algorithm SHA256).Hash -ne $oldDigest -or $restored.proxy.pid -eq $patched.proxy.pid -or
                (Get-CQMSIRegistrationCount -Version $CurrentVersion) -ne 0) {
                throw "legacy migration rollback did not restore the previous executable and its services"
            }
            Write-Host "Legacy migration rollback restored CQ $PreviousVersion and a healthy replacement listener."
        }
    }

    $upgradeLog = Join-Path $temporaryRoot "upgrade-current.log"
    Invoke-MSI -Action install -Path $CurrentMSI -Log $upgradeLog
    if ($legacyMigration) {
        Assert-PatchedLegacyUninstall -Log $upgradeLog
        $installerKey = Get-Item -LiteralPath "HKCU:\Software\jacobcxdev\CQ\Installer"
        if ($installerKey.GetValueNames() -contains "LegacyQuietUpgrade") {
            throw "legacy migration patch marker remains after upgrade"
        }
    }
    if ((Get-CQMSIRegistrationCount -Version $PreviousVersion) -ne 0) {
        throw "previous CQ MSI registration remains after upgrade"
    }
    $status = Assert-Installed -Version $CurrentVersion
    if ($status.proxy.pid -eq 0) {
        throw "upgraded proxy has no manager PID"
    }

    $previousProxyPID = $status.proxy.pid
    $restart = [Diagnostics.Stopwatch]::StartNew()
    & $installedCQ proxy restart
    if ($LASTEXITCODE -ne 0) {
        throw "installed CQ proxy restart failed"
    }
    $restart.Stop()
    $listener = Get-NetTCPConnection -LocalAddress "127.0.0.1" -LocalPort $Port -State Listen
    if ($listener.OwningProcess -eq $previousProxyPID -or (Invoke-WebRequest -Uri "http://127.0.0.1:$Port/health" -TimeoutSec 5).StatusCode -ne 200) {
        throw "installed CQ proxy restart returned before the replacement listener was healthy"
    }
    $status = Assert-Installed -Version $CurrentVersion
    if ($status.proxy.pid -eq 0 -or $status.proxy.pid -eq $previousProxyPID) {
        throw "installed CQ proxy restart did not replace the listener process"
    }
    Write-Host ("CQ proxy restart verified in {0:N2} seconds (PID {1} to {2})" -f $restart.Elapsed.TotalSeconds, $previousProxyPID, $status.proxy.pid)

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
    if ($foregroundProxy -and -not $foregroundProxy.HasExited) {
        Stop-Process -Id $foregroundProxy.Id -Force -ErrorAction SilentlyContinue
    }
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
        Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $serviceProbe } | ForEach-Object {
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
