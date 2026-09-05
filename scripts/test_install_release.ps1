param([Parameter(Mandatory=$true)][string]$Archive, [Parameter(Mandatory=$true)][string]$Checksums)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'install-release.ps1'
$Archive = [IO.Path]::GetFullPath($Archive)
$Checksums = [IO.Path]::GetFullPath($Checksums)
$work = Join-Path ([IO.Path]::GetTempPath()) ('rkc-installer-test-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($work) | Out-Null

function Invoke-InstallerTest([string[]]$Arguments, [bool]$Success) {
    $output = & powershell -NoProfile -File $installer @Arguments 2>&1
    $status = $LASTEXITCODE
    if (($status -eq 0) -ne $Success) { throw "Unexpected installer status $status : $output" }
}

try {
    $prefix = Join-Path $work "prefix with 'quote"
    $arguments = @('-Archive', $Archive, '-Checksums', $Checksums, '-Prefix', $prefix, '-NoPath')
    Invoke-InstallerTest $arguments $true
    $binary = Join-Path $prefix 'bin/rkc.exe'
    $before = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
    if (!(Test-Path -LiteralPath (Join-Path $prefix 'share/doc/rkc/LICENSES/Go.txt'))) { throw 'Installed license tree is incomplete.' }
    # Existing-file replacement must also work on Windows PowerShell 5.1.
    Invoke-InstallerTest $arguments $true
    if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne $before) { throw 'Repeat installation changed binary bytes.' }

    $architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
    if ($architecture -eq 'x64') { $architecture = 'amd64' }
    $name = "rkc-windows-$architecture.zip"
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
    Write-Host 'Windows installer: repeat install, preserved licenses, invalid receipts, and traversal rejection passed.'
} finally { Remove-Item -LiteralPath $work -Recurse -Force }
