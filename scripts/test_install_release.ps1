param([Parameter(Mandatory=$true)][string]$Archive, [Parameter(Mandatory=$true)][string]$Checksums)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'install-release.ps1'
$Archive = [IO.Path]::GetFullPath($Archive)
$Checksums = [IO.Path]::GetFullPath($Checksums)
$work = Join-Path ([IO.Path]::GetTempPath()) ('rkc-installer-test-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($work) | Out-Null

function Invoke-InstallerTest([string[]]$Arguments, [bool]$Success, [string]$HostPath = 'powershell') {
    # The Windows ARM runner's emulated host defaults to Restricted. Allow this
    # checked-out local test script for the child process only; RemoteSigned
    # retains remote signing checks and never overrides Group Policy or changes
    # the user's/machine's persisted execution policy.
    $output = & $HostPath -NoProfile -NonInteractive -ExecutionPolicy RemoteSigned -File $installer @Arguments 2>&1
    $status = $LASTEXITCODE
    if (($status -eq 0) -ne $Success) { throw "Unexpected installer status $status from $HostPath : $output" }
}

try {
    $originalUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $prefix = Join-Path $work "prefix with 'quote"
    $arguments = @('-Archive', $Archive, '-Checksums', $Checksums, '-Prefix', $prefix, '-NoPath')
    Invoke-InstallerTest $arguments $true
    $binary = Join-Path $prefix 'bin/rkc.exe'
    $before = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
    if (!(Test-Path -LiteralPath (Join-Path $prefix 'share/doc/rkc/LICENSES/Go.txt'))) { throw 'Installed license tree is incomplete.' }
    # Existing-file replacement must also work on Windows PowerShell 5.1.
    Invoke-InstallerTest $arguments $true
    if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne $before) { throw 'Repeat installation changed binary bytes.' }

    # Reinstallation must replace stale bytes, not merely skip existing files.
    [IO.File]::WriteAllText($binary, 'Previous installation fixture.')
    Invoke-InstallerTest $arguments $true
    if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne $before) { throw 'Reinstallation did not restore the verified release binary.' }

    # Exercise the baseline interpreter under x86 emulation as well, where
    # Framework RuntimeInformation historically hid the native ARM64 machine.
    $emulatedHost = Join-Path $env:WINDIR 'SysWOW64\WindowsPowerShell\v1.0\powershell.exe'
    if (Test-Path -LiteralPath $emulatedHost -PathType Leaf) {
        Invoke-InstallerTest $arguments $true $emulatedHost
        if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne $before) { throw 'Emulated bootstrap selected different binary bytes.' }
    }

    $semicolonPrefix = Join-Path $work 'prefix;with-delimiter'
    Invoke-InstallerTest @('-Archive', $Archive, '-Checksums', $Checksums, '-Prefix', $semicolonPrefix) $false
    if (Test-Path -LiteralPath $semicolonPrefix) { throw 'Unrepresentable PATH prefix was installed before rejection.' }
    Invoke-InstallerTest @('-Archive', $Archive, '-Checksums', $Checksums, '-Prefix', $semicolonPrefix, '-NoPath') $true
    if ([Environment]::GetEnvironmentVariable('Path', 'User') -cne $originalUserPath) { throw '-NoPath or rejected installation changed the saved user PATH.' }

    $platform = (Get-Content -LiteralPath (Join-Path $prefix 'share/doc/rkc/portable-manifest.json') -Raw | ConvertFrom-Json).platform
    if ($platform -notmatch '^windows-(amd64|arm64)$') { throw 'Installed receipt is not a Windows platform.' }
    $name = "rkc-$platform.zip"
    $wrong = Join-Path $work 'wrong-sums.txt'
    [IO.File]::WriteAllText($wrong, ('0' * 64) + "  $name`n")
    Invoke-InstallerTest @('-Archive', $Archive, '-Checksums', $wrong, '-Prefix', $prefix, '-NoPath') $false
    [IO.File]::WriteAllText($wrong, ([IO.File]::ReadAllText($Checksums) * 2))
    Invoke-InstallerTest @('-Archive', $Archive, '-Checksums', $wrong, '-Prefix', $prefix, '-NoPath') $false
    if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne $before) { throw 'Rejected receipt changed the previous installation.' }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $unsafe = Join-Path $work 'unsafe.zip'
    $zip = [IO.Compression.ZipFile]::Open($unsafe, [IO.Compression.ZipArchiveMode]::Create)
    try {
        $entry = $zip.CreateEntry('../escape')
        $entry.ExternalAttributes = -2119958528 # signed int32 form of regular 0644
        $stream = $entry.Open()
        try { $stream.WriteByte(65) } finally { $stream.Dispose() }
    } finally { $zip.Dispose() }
    [IO.File]::WriteAllText($wrong, (Get-FileHash -LiteralPath $unsafe -Algorithm SHA256).Hash.ToLowerInvariant() + "  $name`n")
    Invoke-InstallerTest @('-Archive', $unsafe, '-Checksums', $wrong, '-Prefix', $prefix, '-NoPath') $false
    if (Test-Path -LiteralPath (Join-Path $work 'escape')) { throw 'Archive escaped extraction boundary.' }
    Write-Host 'Windows installer: native/emulated bootstrap, repeat install, preserved licenses/PATH, invalid receipts, and traversal rejection passed.'
} finally { Remove-Item -LiteralPath $work -Recurse -Force }
