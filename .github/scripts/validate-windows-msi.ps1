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
    [int]$Port = 19280,

    [switch]$StandardUser,

    [switch]$StandardUserChild,

    [string]$EvidenceRoot
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if ($StandardUserChild) {
    # The launcher supplies explicit OS settings only, never CI credentials. Restore
    # this new user's profile variables from Windows after loading its profile.
    $env:USERPROFILE = [Environment]::GetFolderPath("UserProfile")
    $env:APPDATA = [Environment]::GetFolderPath("ApplicationData")
    $env:LOCALAPPDATA = [Environment]::GetFolderPath("LocalApplicationData")
    $env:USERNAME = [Environment]::UserName
    $env:USERDOMAIN = [Environment]::UserDomainName
    $env:TEMP = Join-Path $env:LOCALAPPDATA "Temp"
    $env:TMP = $env:TEMP
    New-Item -ItemType Directory -Path $env:TEMP -Force | Out-Null
}

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
        [ValidateSet("install", "uninstall")]
        [string]$Action,
        [string]$Path,
        [string]$Log
    )
    $verb = if ($Action -eq "install") { "/i" } else { "/x" }
    $arguments = @($verb, ('"' + $Path + '"'), "/qn", "/norestart", "/l*v", ('"' + $Log + '"'))
    $process = Start-Process -FilePath "msiexec.exe" -ArgumentList $arguments -Wait -PassThru
    if ($EvidenceRoot) {
        New-Item -ItemType Directory -Path $EvidenceRoot -Force | Out-Null
        @{ action = $Action; exit_code = $process.ExitCode; log = [IO.Path]::GetFileName($Log) } |
            ConvertTo-Json -Compress | Add-Content -LiteralPath (Join-Path $EvidenceRoot "msi-results.jsonl")
    }
    if ($process.ExitCode -ne 0 -and $process.ExitCode -ne 3010) {
        if (Test-Path -LiteralPath $Log -PathType Leaf) {
            Get-Content -LiteralPath $Log -Tail 160 | Write-Error -ErrorAction Continue
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

function Invoke-StandardUserMSIValidation {
    if ($env:GITHUB_ACTIONS -ne "true") {
        throw "standard-user qualification is restricted to a disposable GitHub Actions VM"
    }
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "standard-user qualification launcher requires an administrator on a disposable CI VM"
    }
    if (-not $EvidenceRoot) {
        throw "standard-user qualification requires an evidence directory"
    }
    if (@(Get-ScheduledTask -TaskPath "\cq\" -ErrorAction SilentlyContinue).Count -ne 0) {
        throw "refusing standard-user qualification while CQ tasks already exist"
    }
    New-Item -ItemType Directory -Path $EvidenceRoot -Force | Out-Null
    $qualificationRoot = Join-Path $env:PUBLIC ("cq-msi-standard-" + [Guid]::NewGuid().ToString("N"))
    $qualificationName = "cqmsi_" + [Guid]::NewGuid().ToString("N").Substring(0, 12)
    $passwordBytes = [byte[]]::new(24)
    [Security.Cryptography.RandomNumberGenerator]::Fill($passwordBytes)
    $password = ConvertTo-SecureString ("Aa9!" + [Convert]::ToBase64String($passwordBytes)) -AsPlainText -Force
    [Array]::Clear($passwordBytes, 0, $passwordBytes.Length)
    $qualificationUser = $null
    $process = $null
    $outputTask = $null
    $errorTask = $null
    # Hosted Windows Server images block unmanaged per-user MSI installs. Allow
    # that installation type only for this disposable VM test, without elevating
    # the child, and preserve the original policy for restoration and evidence.
    $policyPath = "SOFTWARE\Policies\Microsoft\Windows\Installer"
    $policyKey = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey($policyPath, $true)
    $policyKeyExisted = $null -ne $policyKey
    $policyValueExisted = $policyKeyExisted -and $policyKey.GetValueNames() -contains "DisableMSI"
    $previousPolicyValue = $null
    if ($policyValueExisted) {
        if ($policyKey.GetValueKind("DisableMSI") -ne [Microsoft.Win32.RegistryValueKind]::DWord) {
            $policyKey.Dispose()
            $password.Dispose()
            throw "unexpected DisableMSI registry type; refusing qualification policy override"
        }
        $previousPolicyValue = $policyKey.GetValue("DisableMSI")
    }
    $policyChanged = $false
    $policyEvidence = [ordered]@{
        registry_key_existed = $policyKeyExisted
        value_existed = $policyValueExisted
        original_disable_msi = $previousPolicyValue
        qualification_disable_msi = 0
        restored = $false
    }
    try {
        if (-not $policyKey) { $policyKey = [Microsoft.Win32.Registry]::LocalMachine.CreateSubKey($policyPath) }
        $policyKey.SetValue("DisableMSI", 0, [Microsoft.Win32.RegistryValueKind]::DWord)
        $policyChanged = $true
        $policyEvidence | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $EvidenceRoot "installer-policy.json")
        $qualificationUser = New-LocalUser -Name $qualificationName -Password $password -PasswordNeverExpires
        $users = Get-LocalGroup -SID "S-1-5-32-545"
        Add-LocalGroupMember -Group $users -Member $qualificationUser
        $administrators = Get-LocalGroup -SID "S-1-5-32-544"
        if (@(Get-LocalGroupMember -Group $administrators | Where-Object { $_.SID -eq $qualificationUser.SID }).Count -ne 0) {
            throw "qualification account unexpectedly belongs to Administrators"
        }
        New-Item -ItemType Directory -Path $qualificationRoot -Force | Out-Null
        $security = [Security.AccessControl.DirectorySecurity]::new()
        $security.SetSecurityDescriptorSddlForm("O:$($identity.User.Value)G:$($identity.User.Value)D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;$($identity.User.Value))(A;OICI;FA;;;$($qualificationUser.SID.Value))")
        Set-Acl -LiteralPath $qualificationRoot -AclObject $security
        $childScript = Join-Path $qualificationRoot "validate-windows-msi.ps1"
        $childMSI = Join-Path $qualificationRoot "current.msi"
        $childEvidence = Join-Path $qualificationRoot "evidence"
        Copy-Item -LiteralPath $PSCommandPath -Destination $childScript
        Copy-Item -LiteralPath $CurrentMSI -Destination $childMSI
        # Retain the exact package executable: a failed MSI rolls back its installed copy.
        $extractionRoot = Join-Path $qualificationRoot "extracted"
        $extractionLog = Join-Path $EvidenceRoot "extract-current.log"
        $extraction = Start-Process -FilePath "msiexec.exe" -ArgumentList @("/a", ('"' + $childMSI + '"'), "/qn", "/norestart",
            ('TARGETDIR="' + $extractionRoot + '"'), "/l*v", ('"' + $extractionLog + '"')) -PassThru
        try {
            if (-not $extraction.WaitForExit(60000)) {
                $extraction.Kill($true)
                $extraction.WaitForExit()
                throw "MSI diagnostic extraction exceeded one minute"
            }
            if ($extraction.ExitCode -ne 0) { throw "MSI diagnostic extraction failed with exit code $($extraction.ExitCode)" }
        }
        finally { $extraction.Dispose() }
        $extractedExecutables = @(Get-ChildItem -LiteralPath $extractionRoot -Filter "cq.exe" -File -Recurse)
        if ($extractedExecutables.Count -ne 1) { throw "MSI diagnostic extraction must contain exactly one CQ executable" }
        Copy-Item -LiteralPath $extractedExecutables[0].FullName -Destination (Join-Path $qualificationRoot "cq.exe")
        $start = [Diagnostics.ProcessStartInfo]::new()
        $start.FileName = (Get-Process -Id $PID).Path
        $start.UseShellExecute = $false
        $start.LoadUserProfile = $true
        $start.UserName = $qualificationName
        $start.Domain = $env:COMPUTERNAME
        $start.Password = $password
        $start.CreateNoWindow = $true
        $start.RedirectStandardOutput = $true
        $start.RedirectStandardError = $true
        $start.WorkingDirectory = $qualificationRoot
        $start.Environment.Clear()
        foreach ($name in @("SystemRoot", "WINDIR", "COMSPEC", "PATH", "PATHEXT", "COMPUTERNAME", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "CommonProgramFiles", "CommonProgramFiles(x86)")) {
            $value = [Environment]::GetEnvironmentVariable($name, "Machine")
            if (-not $value) { $value = [Environment]::GetEnvironmentVariable($name, "Process") }
            if ($value) { $start.Environment[$name] = $value }
        }
        foreach ($argument in @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", $childScript,
            "-PreviousMSI", $childMSI, "-CurrentMSI", $childMSI, "-PreviousVersion", $CurrentVersion,
            "-CurrentVersion", $CurrentVersion, "-StandardUserChild", "-EvidenceRoot", $childEvidence)) {
            $start.ArgumentList.Add($argument)
        }
        $process = [Diagnostics.Process]::Start($start)
        $outputTask = $process.StandardOutput.ReadToEndAsync()
        $errorTask = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit(600000)) {
            $process.Kill($true)
            $process.WaitForExit()
            throw "standard-user MSI qualification exceeded ten minutes"
        }
        if ($process.ExitCode -ne 0) {
            throw "standard-user MSI qualification failed with exit code $($process.ExitCode); inspect retained evidence"
        }
    }
    finally {
        try {
            if ($process -and -not $process.HasExited) {
                $process.Kill($true)
                $process.WaitForExit()
            }
            if ($outputTask) { [IO.File]::WriteAllText((Join-Path $EvidenceRoot "stdout.log"), $outputTask.GetAwaiter().GetResult()) }
            if ($errorTask) { [IO.File]::WriteAllText((Join-Path $EvidenceRoot "stderr.log"), $errorTask.GetAwaiter().GetResult()) }
            if (Test-Path -LiteralPath (Join-Path $qualificationRoot "evidence")) {
                Copy-Item -Path (Join-Path $qualificationRoot "evidence\*") -Destination $EvidenceRoot -Force
            }
            if ($qualificationUser) {
                foreach ($name in @("Proxy", "Refresh")) {
                    $task = Get-ScheduledTask -TaskPath "\cq\" -TaskName $name -ErrorAction SilentlyContinue
                    if ($task -and $task.Principal.UserId -in @($qualificationUser.SID.Value, "$env:COMPUTERNAME\$qualificationName")) {
                        Stop-ScheduledTask -InputObject $task -ErrorAction SilentlyContinue
                        Unregister-ScheduledTask -InputObject $task -Confirm:$false
                    }
                }
                if (@(Get-ScheduledTask -TaskPath "\cq\" -ErrorAction SilentlyContinue).Count -eq 0) {
                    Remove-CQTasks
                }
                Get-CimInstance Win32_Process -Filter "Name = 'cq.exe'" | ForEach-Object {
                    if ((Invoke-CimMethod -InputObject $_ -MethodName GetOwnerSid).Sid -eq $qualificationUser.SID.Value) {
                        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
                    }
                }
                Get-CimInstance Win32_UserProfile | Where-Object { $_.SID -eq $qualificationUser.SID.Value -and -not $_.Loaded } | Remove-CimInstance
                Remove-LocalUser -SID $qualificationUser.SID
            }
            $password.Dispose()
            if ($process) { $process.Dispose() }
            if (Test-Path -LiteralPath $qualificationRoot) { Remove-Item -LiteralPath $qualificationRoot -Recurse -Force }
        }
        finally {
            if ($policyKey) {
                try {
                    if ($policyChanged) {
                        if ($policyValueExisted) {
                            $policyKey.SetValue("DisableMSI", $previousPolicyValue, [Microsoft.Win32.RegistryValueKind]::DWord)
                        }
                        else {
                            $policyKey.DeleteValue("DisableMSI", $false)
                        }
                        $restoredValueExisted = $policyKey.GetValueNames() -contains "DisableMSI"
                        $policyEvidence.restored = $restoredValueExisted -eq $policyValueExisted -and
                            (-not $policyValueExisted -or $policyKey.GetValue("DisableMSI") -eq $previousPolicyValue)
                        if (-not $policyEvidence.restored) { throw "DisableMSI policy restoration differs" }
                        $policyEvidence | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $EvidenceRoot "installer-policy.json")
                    }
                }
                finally {
                    $policyKey.Dispose()
                }
                if (-not $policyKeyExisted) {
                    # Do not remove a key which acquired unrelated values during qualification.
                    $restoredKey = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey($policyPath)
                    $emptyKey = $restoredKey.ValueCount -eq 0 -and $restoredKey.SubKeyCount -eq 0
                    $restoredKey.Dispose()
                    if ($emptyKey) { [Microsoft.Win32.Registry]::LocalMachine.DeleteSubKey($policyPath) }
                }
            }
        }
    }
}

if ($StandardUser) {
    Invoke-StandardUserMSIValidation
    return
}

if ($StandardUserChild) {
    New-Item -ItemType Directory -Path $EvidenceRoot, $temporaryRoot -Force | Out-Null
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $administrator = [Security.Principal.WindowsPrincipal]::new($identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    @{ sid = $identity.User.Value; administrator = $administrator; interactive = [Environment]::UserInteractive;
        session_id = (Get-Process -Id $PID).SessionId; authentication_type = $identity.AuthenticationType; version = $CurrentVersion } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $EvidenceRoot "identity.json")
    if ($administrator) { throw "qualification child is elevated; refusing installer test" }
    if (-not [Environment]::UserInteractive) { throw "qualification child has no interactive window station" }
    try {
        Invoke-MSI -Action install -Path $CurrentMSI -Log (Join-Path $temporaryRoot "install-current.log")
        $status = Wait-ServiceStatus -Version $CurrentVersion
        @{ owner = $status.owner; proxy_running = $status.proxy.running; proxy_healthy = $status.proxy.healthy; refresh_healthy = $status.refresh.healthy } |
            ConvertTo-Json | Set-Content -LiteralPath (Join-Path $EvidenceRoot "service.json")
    }
    catch {
        $installationFailure = $_
        # Diagnose the same package as the same standard user after MSI rollback.
        # Record only command output and owned task state; never read credentials.
        try {
            $diagnosticCQ = Join-Path $PSScriptRoot "cq.exe"
            $diagnosticVersion = (& $diagnosticCQ --version).TrimStart("v")
            if ($diagnosticVersion -ne $CurrentVersion) { throw "MSI diagnostic executable version differs" }
            foreach ($probe in @(
                @{ name = "interactive-session"; executable = (Join-Path $env:SystemRoot "System32\query.exe"); arguments = @("user", $env:USERNAME); seconds = 10 },
                @{ name = "service-install"; arguments = @("service", "install", "--owner=winget"); seconds = 120 },
                @{ name = "service-status"; arguments = @("service", "status", "--json"); seconds = 30 },
                @{ name = "foreground-proxy"; arguments = @("proxy", "start"); seconds = 15 }
            )) {
                $probeExecutablePath = if ($probe.ContainsKey("executable")) { $probe.executable } else { $diagnosticCQ }
                $diagnostic = Start-Process -FilePath $probeExecutablePath -ArgumentList $probe.arguments -PassThru `
                    -RedirectStandardOutput (Join-Path $EvidenceRoot ($probe.name + ".stdout.log")) `
                    -RedirectStandardError (Join-Path $EvidenceRoot ($probe.name + ".stderr.log"))
                $deadline = [DateTime]::UtcNow.AddSeconds($probe.seconds)
                try {
                    while (-not $diagnostic.WaitForExit(2000) -and [DateTime]::UtcNow -lt $deadline) {
                        foreach ($name in @("Proxy", "Refresh")) {
                            $task = Get-ScheduledTask -TaskPath $taskPath -TaskName $name -ErrorAction SilentlyContinue
                            if ($task) {
                                $info = Get-ScheduledTaskInfo -InputObject $task
                                @{ probe = $probe.name; task = $name; state = [string]$task.State; principal = $task.Principal.UserId;
                                    logon_type = [string]$task.Principal.LogonType; last_result = $info.LastTaskResult } |
                                    ConvertTo-Json -Compress | Add-Content -LiteralPath (Join-Path $EvidenceRoot "diagnostic-tasks.jsonl")
                            }
                        }
                    }
                    $timedOut = -not $diagnostic.HasExited
                    if ($timedOut) { $diagnostic.Kill($true); $diagnostic.WaitForExit() }
                    @{ probe = $probe.name; exit_code = $diagnostic.ExitCode; timed_out = $timedOut } |
                        ConvertTo-Json -Compress | Add-Content -LiteralPath (Join-Path $EvidenceRoot "diagnostic-results.jsonl")
                }
                finally {
                    if (-not $diagnostic.HasExited) { $diagnostic.Kill($true); $diagnostic.WaitForExit() }
                    $diagnostic.Dispose()
                }
            }
        }
        catch {
            $_.Exception.Message | Set-Content -LiteralPath (Join-Path $EvidenceRoot "diagnostic-error.txt")
        }
        throw $installationFailure
    }
    finally {
        if (Test-Path -LiteralPath $installedCQ) {
            try { Invoke-MSI -Action uninstall -Path $CurrentMSI -Log (Join-Path $temporaryRoot "uninstall-current.log") }
            catch { Write-Warning "standard-user uninstall failed; outer disposable-user cleanup will run" }
        }
        Get-ChildItem -LiteralPath $temporaryRoot -Filter "*.log" | Copy-Item -Destination $EvidenceRoot -Force
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
    return
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
    & go build -ldflags "-X main.version=$PreviousVersion" -o $serviceProbe ./cmd/cq
    if ($LASTEXITCODE -ne 0) {
        throw "failed to build direct service lifecycle probe"
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
