<#
.SYNOPSIS
Install checksum-verified RKC portable binaries on Windows amd64 or arm64.
.DESCRIPTION
No administrator access, Go toolchain, or model download is required. Installs
under LOCALAPPDATA\RKC by default and adds its bin directory to the user PATH.
Use -NoPath to leave PATH unchanged. Use -Archive and -Checksums for offline
installation. Protected workbench commands, Python isolation, and model
execution require Linux user-systemd/cgroup v2; no guard is disabled here.
#>
param(
    [string]$Version = 'latest',
    [string]$Prefix = '',
    [string]$Archive = '',
    [string]$Checksums = '',
    [switch]$NoPath
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-RkcArchitecture {
    # Framework RuntimeInformation is absent before .NET 4.7.1 and older
    # implementations report the emulated x86/x64 architecture on Windows ARM.
    # Ask the OS for its native machine, independently of this PowerShell host.
    # https://learn.microsoft.com/windows/win32/api/wow64apiset/nf-wow64apiset-iswow64process2
    if (!('RkcInstaller.NativePlatform' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
namespace RkcInstaller {
    public static class NativePlatform {
        [DllImport("kernel32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool IsWow64Process2(IntPtr process, out ushort processMachine, out ushort nativeMachine);
        [DllImport("kernel32.dll")]
        private static extern void GetNativeSystemInfo(out SystemInfo info);
        [StructLayout(LayoutKind.Sequential)]
        private struct SystemInfo {
            public ushort Architecture, Reserved;
            public uint PageSize;
            public IntPtr MinimumAddress, MaximumAddress;
            public UIntPtr ProcessorMask;
            public uint ProcessorCount, ProcessorType, AllocationGranularity;
            public ushort ProcessorLevel, ProcessorRevision;
        }
        public static string Architecture() {
            ushort processMachine, nativeMachine;
            try {
                if (!IsWow64Process2(new IntPtr(-1), out processMachine, out nativeMachine))
                    throw new Win32Exception(Marshal.GetLastWin32Error());
            } catch (EntryPointNotFoundException) {
                // Early Windows 10 predates both this API and Windows ARM64.
                SystemInfo info;
                GetNativeSystemInfo(out info);
                nativeMachine = info.Architecture == 9 ? (ushort)0x8664 : (ushort)0;
            }
            if (nativeMachine == 0x8664) return "amd64";
            if (nativeMachine == 0xaa64) return "arm64";
            throw new PlatformNotSupportedException("Supported Windows architectures are amd64 and arm64.");
        }
    }
}
'@
    }
    return [RkcInstaller.NativePlatform]::Architecture()
}

function Get-RkcDownload([string]$Address, [string]$Destination, [long]$Maximum) {
    Add-Type -AssemblyName System.Net.Http
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $false
    $client = [System.Net.Http.HttpClient]::new($handler)
    $client.DefaultRequestHeaders.UserAgent.ParseAdd('RKC-Installer/1')
    $cancellation = [System.Threading.CancellationTokenSource]::new(300000)
    try {
        for ($redirect = 0; $redirect -le 5; $redirect++) {
            $uri = [Uri]$Address
            if ($uri.Scheme -ne 'https') { throw 'Downloads and redirects must use HTTPS.' }
            $response = $client.GetAsync($uri, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead, $cancellation.Token).GetAwaiter().GetResult()
            try {
                $status = [int]$response.StatusCode
                if ($status -ge 300 -and $status -lt 400) {
                    if (!$response.Headers.Location) { throw 'Release redirect has no location.' }
                    $Address = [Uri]::new($uri, $response.Headers.Location).AbsoluteUri
                    continue
                }
                $response.EnsureSuccessStatusCode() | Out-Null
                if ($response.Content.Headers.ContentLength -gt $Maximum) { throw 'Release download exceeds its size bound.' }
                $inputStream = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
                $outputStream = [IO.File]::Open($Destination, [IO.FileMode]::CreateNew)
                try {
                    $buffer = New-Object byte[] 65536
                    [long]$copied = 0
                    while (($count = $inputStream.ReadAsync($buffer, 0, $buffer.Length, $cancellation.Token).GetAwaiter().GetResult()) -gt 0) {
                        $copied += $count
                        if ($copied -gt $Maximum) { throw 'Release download exceeds its size bound.' }
                        $outputStream.Write($buffer, 0, $count)
                    }
                } finally { $inputStream.Dispose(); $outputStream.Dispose() }
                return
            } finally { $response.Dispose() }
        }
        throw 'Too many release redirects.'
    } finally { $cancellation.Dispose(); $client.Dispose(); $handler.Dispose() }
}

function Assert-RkcPath([string]$Path, [bool]$Directory) {
    $current = [IO.Path]::GetFullPath($Path)
    $leaf = $true
    while ($current) {
        if (Test-Path -LiteralPath $current) {
            $item = Get-Item -Force -LiteralPath $current
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw "Refusing linked destination: $current" }
            if (($leaf -and $Directory -and !$item.PSIsContainer) -or (!$leaf -and !$item.PSIsContainer) -or ($leaf -and !$Directory -and $item.PSIsContainer)) { throw "Unexpected destination type: $current" }
        }
        $leaf = $false
        $current = [IO.Path]::GetDirectoryName($current)
    }
}

function Install-RkcRelease {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Use install-release.sh on Linux or macOS.' }
    if ($Version -notmatch '^[a-zA-Z0-9._-]+$') { throw 'Invalid release tag.' }
    $architecture = Get-RkcArchitecture
    if (!$Prefix) {
        if (!$env:LOCALAPPDATA) { throw 'LOCALAPPDATA is unavailable; specify -Prefix.' }
        $Prefix = Join-Path $env:LOCALAPPDATA 'RKC'
    }
    if ($Prefix -match '[\r\n]' -or $Prefix -in @('.', '..')) { throw 'Unsafe install prefix.' }
    if (!$NoPath -and $Prefix.Contains(';')) { throw 'A prefix containing semicolons cannot be added to PATH; use -NoPath and the absolute launch path.' }
    $installRoot = [IO.Path]::GetFullPath($Prefix).TrimEnd('\')
    if (!$installRoot -or $installRoot.TrimEnd('\') -eq [IO.Path]::GetPathRoot($installRoot).TrimEnd('\')) { throw 'Refusing a filesystem-root install prefix.' }
    Assert-RkcPath $installRoot $true
    $asset = "rkc-windows-$architecture.zip"
    $work = Join-Path ([IO.Path]::GetTempPath()) ('rkc-download-' + [Guid]::NewGuid().ToString('N'))
    [IO.Directory]::CreateDirectory($work) | Out-Null
    try {
        if (!$Archive -and !$Checksums) {
            $base = 'https://github.com/neuroforge-io/RKC/releases'
            if ($Version -eq 'latest') { $base += '/latest/download' } else { $base += "/download/$Version" }
            $Archive = Join-Path $work $asset
            $Checksums = Join-Path $work 'SHA256SUMS.txt'
            Get-RkcDownload "$base/SHA256SUMS.txt" $Checksums 65536
            Get-RkcDownload "$base/$asset" $Archive 134217728
        } elseif (!$Archive -or !$Checksums) { throw '-Archive and -Checksums must be supplied together.' }
        foreach ($inputPath in @($Archive, $Checksums)) {
            if (!(Test-Path -LiteralPath $inputPath -PathType Leaf)) { throw 'Download input is missing.' }
            Assert-RkcPath $inputPath $false
        }
        if ((Get-Item -LiteralPath $Archive).Length -gt 134217728 -or (Get-Item -LiteralPath $Checksums).Length -gt 65536) { throw 'Download exceeds archive or receipt bounds.' }
        $entries = @(Get-Content -LiteralPath $Checksums | Where-Object { $_ -match ('  ' + [regex]::Escape($asset) + '$') })
        if ($entries.Count -ne 1 -or $entries[0] -notmatch ('^([0-9a-f]{64})  ' + [regex]::Escape($asset) + '$')) { throw 'Receipt must contain exactly one valid checksum for this platform.' }
        $expected = $Matches[1]
        if ((Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw 'Archive checksum mismatch; nothing was installed.' }
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [IO.Compression.ZipFile]::OpenRead([IO.Path]::GetFullPath($Archive))
        try {
            $names = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
            [long]$expanded = 0
            if ($zip.Entries.Count -gt 1024) { throw 'Too many archive entries.' }
            foreach ($entry in $zip.Entries) {
                $name = $entry.FullName
                if ($name -notmatch '^[a-zA-Z0-9_./@+!-]+$' -or $name -match '(^|/)\.\.?(/|$)' -or $name.StartsWith('/') -or !$names.Add($name)) { throw 'Unsafe or duplicate archive path.' }
                if ($name -notmatch '^(bin/rkc(?:-mcp)?\.exe|share/doc/rkc/.+|share/rkc/(models|schemas)/.+)$') { throw "Unexpected archive path: $name" }
                if ((($entry.ExternalAttributes -shr 16) -band 61440) -ne 32768) { throw 'Archive contains a link, directory, or special file.' }
                $expanded += $entry.Length
                if ($expanded -gt 268435456) { throw 'Expanded archive exceeds 256 MiB.' }
                Assert-RkcPath (Join-Path $installRoot $name) $false
            }
            foreach ($required in @('bin/rkc.exe', 'bin/rkc-mcp.exe', 'share/doc/rkc/portable-manifest.json', 'share/doc/rkc/VERSION')) {
                if (!$names.Contains($required)) { throw "Archive omits required file: $required" }
            }
            foreach ($entry in $zip.Entries) {
                $destination = Join-Path $installRoot $entry.FullName
                $directory = [IO.Path]::GetDirectoryName($destination)
                Assert-RkcPath $directory $true
                [IO.Directory]::CreateDirectory($directory) | Out-Null
                $temporary = Join-Path $directory ('.rkc-install-' + [Guid]::NewGuid().ToString('N'))
                try {
                    [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $temporary, $false)
                    Assert-RkcPath $destination $false
                    if ([IO.File]::Exists($destination)) {
                        # Windows PowerShell coerces $null to an empty string for
                        # this .NET string parameter. Pass a true null backup path
                        # while retaining atomic replacement of the existing file.
                        [IO.File]::Replace($temporary, $destination, [System.Management.Automation.Language.NullString]::Value)
                    }
                    else { [IO.File]::Move($temporary, $destination) }
                } finally { if (Test-Path -LiteralPath $temporary) { Remove-Item -Force -LiteralPath $temporary } }
            }
        } finally { $zip.Dispose() }
        $bin = Join-Path $installRoot 'bin'
        if (!$NoPath) {
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
            if ($bin -notin @($userPath -split ';')) {
                [Environment]::SetEnvironmentVariable('Path', ($bin + ';' + $userPath).TrimEnd(';'), 'User')
            }
            if ($bin -notin @($env:Path -split ';')) { $env:Path = $bin + ';' + $env:Path }
        }
        Write-Host "RKC installed in $bin"
        Write-Host ("First run: & '" + (Join-Path $bin 'rkc.exe').Replace("'", "''") + "' gui")
        Write-Host 'Protected workbench, Python isolation, and model execution require Linux user-systemd/cgroup v2.'
    } finally { Remove-Item -LiteralPath $work -Recurse -Force }
}

Install-RkcRelease
