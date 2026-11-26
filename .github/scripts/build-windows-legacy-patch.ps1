param(
    [Parameter(Mandatory = $true)]
    [string]$PreviousMSI,

    [Parameter(Mandatory = $true)]
    [string]$Executable,

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

function Get-MsiRow {
    param($Database, [string]$Statement, [int]$Columns)

    $view = $Database.OpenView($Statement)
    try {
        [void]$view.Execute()
        $record = $view.Fetch()
        if ($null -ne $record) {
            try {
                for ($index = 1; $index -le $Columns; $index++) {
                    $record.StringData($index)
                }
            }
            finally {
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($record)
            }
        }
    }
    finally {
        [void]$view.Close()
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
    }
}

function Invoke-MsiStatement {
    param($Database, [string]$Statement, $Record = $null)

    $view = $Database.OpenView($Statement)
    try {
        if ($null -eq $Record) {
            [void]$view.Execute()
        }
        else {
            [void]$view.Execute($Record)
        }
    }
    finally {
        [void]$view.Close()
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
    }
}

$PreviousMSI = (Resolve-Path -LiteralPath $PreviousMSI).Path
$Executable = (Resolve-Path -LiteralPath $Executable).Path
$Wix = (Resolve-Path -LiteralPath $Wix).Path
$Output = [IO.Path]::GetFullPath($Output)
if ([IO.Path]::GetExtension($Output) -ine ".msp" -or (Test-Path -LiteralPath $Output)) {
    throw "The requested output must be a new MSP file"
}

$legacy = @{
    amd64 = @{
        SHA256 = "4ae8ac26ee0211a13f57e8796e6158183516ead5fe34efb1061cb6426e80ff37"
        PatchCode = "{67038060-1859-4ABF-B9B7-7AD86C6290E7}"
        WixArchitecture = "x64"
        Machine = 0x8664
    }
    arm64 = @{
        SHA256 = "5ee1e0855905c6bdb2cf8ed59b81cd4671b8d278a790215152a4b85df34bba12"
        PatchCode = "{D268364B-6589-4C12-A47A-3059DBE180A0}"
        WixArchitecture = "arm64"
        Machine = 0xaa64
    }
}[$Architecture]
if ((Get-FileHash -LiteralPath $PreviousMSI -Algorithm SHA256).Hash -ine $legacy.SHA256) {
    throw "Expected the published CQ 0.53.5 $Architecture MSI"
}
$reader = [IO.BinaryReader]::new([IO.File]::OpenRead($Executable))
try {
    if ($reader.BaseStream.Length -lt 64 -or $reader.ReadUInt16() -ne 0x5a4d) {
        throw "The quiet service helper must be a Windows executable"
    }
    $reader.BaseStream.Position = 0x3c
    $peOffset = $reader.ReadUInt32()
    if ($peOffset -gt ($reader.BaseStream.Length - 94)) {
        throw "The quiet service helper has an invalid PE header"
    }
    $reader.BaseStream.Position = $peOffset
    if ($reader.ReadUInt32() -ne 0x00004550 -or $reader.ReadUInt16() -ne $legacy.Machine) {
        throw "The quiet service helper does not match $Architecture"
    }
    $reader.BaseStream.Position = $peOffset + 24
    if ($reader.ReadUInt16() -ne 0x20b) {
        throw "The quiet service helper must use PE32+"
    }
    $reader.BaseStream.Position = $peOffset + 92
    if ($reader.ReadUInt16() -ne 2) {
        throw "The quiet service helper must use the Windows GUI subsystem"
    }
}
finally {
    $reader.Dispose()
}

$scratch = [IO.Directory]::CreateTempSubdirectory("cq-msi-legacy-patch-").FullName
try {
    $updated = Join-Path $scratch "updated.msi"
    Copy-Item -LiteralPath $PreviousMSI -Destination $updated
    $installer = New-Object -ComObject WindowsInstaller.Installer
    try {
        $database = $installer.OpenDatabase($updated, 1)
        try {
            $productCode = Get-MsiRow $database 'SELECT `Value` FROM `Property` WHERE `Property` = ''ProductCode''' 1
            if ($productCode -cnotmatch '^\{[A-F0-9]{8}-(?:[A-F0-9]{4}-){3}[A-F0-9]{12}\}$') {
                throw "The legacy MSI has an invalid ProductCode"
            }
            $version = Get-MsiRow $database 'SELECT `Value` FROM `Property` WHERE `Property` = ''ProductVersion''' 1
            $action = @(Get-MsiRow $database 'SELECT `Type`, `Source`, `Target` FROM `CustomAction` WHERE `Action` = ''UninstallCQServices''' 3)
            $sequence = @(Get-MsiRow $database 'SELECT `Condition`, `Sequence` FROM `InstallExecuteSequence` WHERE `Action` = ''UninstallCQServices''' 2)
            $removeFiles = Get-MsiRow $database 'SELECT `Sequence` FROM `InstallExecuteSequence` WHERE `Action` = ''RemoveFiles''' 1
            $rollbackAt = Get-MsiRow $database 'SELECT `Action` FROM `InstallExecuteSequence` WHERE `Sequence` = 3498' 1
            if ($version -cne "0.53.5" -or ($action -join "|") -cne '1042|CQExe|service uninstall --owner=winget' -or
                ($sequence -join "|") -cne 'Installed AND REMOVE~="ALL"|3499' -or $removeFiles -cne "3500" -or $null -ne $rollbackAt) {
                throw "The published Windows Installer service action did not match the expected contract"
            }

            Invoke-MsiStatement $database 'CREATE TABLE `Binary` (`Name` CHAR(72) NOT NULL, `Data` OBJECT NOT NULL PRIMARY KEY `Name`)'
            $binary = $installer.CreateRecord(2)
            try {
                $binary.StringData(1) = "CQLegacyQuietService"
                [void]$binary.SetStream(2, $Executable)
                Invoke-MsiStatement $database 'INSERT INTO `Binary` (`Name`, `Data`) VALUES (?, ?)' $binary
            }
            finally {
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($binary)
            }

            # The legacy action still runs before RemoveFiles, while the original executable and digest match.
            # Run the new quiet helper from the Binary table with the original installed path.
            Invoke-MsiStatement $database 'UPDATE `CustomAction` SET `Type` = 1026, `Source` = ''CQLegacyQuietService'', `Target` = ''service uninstall --owner=winget --legacy-msi-executable="[#CQExe]"'' WHERE `Action` = ''UninstallCQServices'''
            Invoke-MsiStatement $database 'INSERT INTO `CustomAction` (`Action`, `Type`, `Source`, `Target`) VALUES (''RollbackLegacyCQServices'', 1282, ''CQLegacyQuietService'', ''service install --owner=winget --legacy-msi-executable="[#CQExe]"'')'
            Invoke-MsiStatement $database 'INSERT INTO `InstallExecuteSequence` (`Action`, `Condition`, `Sequence`) VALUES (''RollbackLegacyCQServices'', ''Installed AND REMOVE~="ALL" AND UPGRADINGPRODUCTCODE'', 3498)'

            # Windows Installer removes the marker with the legacy executable component.
            Invoke-MsiStatement $database ('INSERT INTO `Registry` (`Registry`, `Root`, `Key`, `Name`, `Value`, `Component_`) VALUES (''CQLegacyQuietUpgrade'', 1, ''Software\jacobcxdev\CQ\Installer'', ''LegacyQuietUpgrade'', ''{0}'', ''CQExecutable'')' -f $legacy.PatchCode)

            $summary = $database.SummaryInformation(1)
            try {
                $summary.Property(9) = [guid]::NewGuid().ToString("B").ToUpperInvariant()
                [void]$summary.Persist()
            }
            finally {
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($summary)
            }
            [void]$database.Commit()
        }
        finally {
            [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database)
        }
    }
    finally {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
    }

    $source = Join-Path $scratch "legacy.wxs"
    $patch = Join-Path $scratch "legacy.msp"
    $baselinePath = [Security.SecurityElement]::Escape($PreviousMSI)
    $updatedPath = [Security.SecurityElement]::Escape($updated)
    @"
<Wix xmlns="http://wixtoolset.org/schemas/v4/wxs">
  <Patch Id="$($legacy.PatchCode)" DisplayName="CQ 0.53.5 quiet upgrade" Description="Run the CQ 0.53.5 service removal without a console." Manufacturer="jacobcxdev" TargetProductName="CQ" Classification="Update">
    <Media Id="1" Cabinet="legacy.cab" EmbedCab="yes">
      <PatchBaseline Id="CQLegacy0535" BaselineFile="$baselinePath" UpdateFile="$updatedPath" />
    </Media>
    <PatchFamily Id="CQLegacyQuiet" ProductCode="$productCode" Version="0.53.5.1">
      <BinaryRef Id="CQLegacyQuietService" />
      <ComponentRef Id="CQExecutable" />
      <CustomActionRef Id="UninstallCQServices" />
      <CustomActionRef Id="RollbackLegacyCQServices" />
    </PatchFamily>
  </Patch>
</Wix>
"@ | Set-Content -LiteralPath $source -Encoding utf8

    & $Wix build $source -arch $legacy.WixArchitecture -intermediatefolder $scratch -pdbtype none -out $patch
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $patch -PathType Leaf)) {
        throw "WiX did not build the CQ legacy patch"
    }
    [void][IO.Directory]::CreateDirectory((Split-Path -Parent $Output))
    [IO.File]::Move($patch, $Output)
}
finally {
    $folder = Get-Item -LiteralPath $scratch
    if ($folder.FullName -cne $scratch -or ($folder.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw "The CQ legacy patch scratch path changed"
    }
    Remove-Item -LiteralPath $scratch -Recurse
}
