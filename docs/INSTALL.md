# Install RKC

Portable downloads install the `rkc` command and `rkc-mcp` agent server without
Go, Python, a model, or a database server. They include license notices and
build/provenance receipts. They do not download model weights.

## Release availability

The [release page](https://github.com/neuroforge-io/RKC/releases) is the source of
portable downloads and their native test receipts. The commands below install
its latest published portable release. For an unreleased checkout, or if no
portable assets are listed, use [Build from source](#build-from-source).

The release workflow builds Linux, macOS, and Windows assets for `amd64` and
`arm64`, then requires installation, compilation, and GUI checks on all six
native platforms. A downloadable release must include matching native test
receipts; building an archive alone does not qualify it. See
[Release validation](RELEASE_VALIDATION.md#portable-download-gate).

## Requirements

| Platform | Runtime baseline | Download installer needs |
| --- | --- | --- |
| Linux `amd64` / `arm64` | A reachable user-systemd manager and delegated cgroup v2 CPU, memory, I/O, and process controllers for compilation and protected jobs | POSIX shell, `curl`, `unzip`, and either `sha256sum` or `shasum` |
| macOS `amd64` / `arm64` | macOS 12 or newer | Terminal shell, `curl`, `unzip`, and `shasum` or `sha256sum` |
| Windows `amd64` / `arm64` | Windows 10 or newer | Windows PowerShell 5.1 or newer, or PowerShell 7 |

`amd64` means Intel/AMD 64-bit; `arm64` includes Apple silicon and Windows on ARM.
These are runtime baselines, not a statement that every listed OS version has
completed native release testing. The GUI needs a browser. GitHub acquisition
needs network access; compiling an existing local folder does not.

On Windows, compilation uses local source, output, and working-storage volumes.
Network shares and mapped remote drives cannot prove the path separation and
host-local transaction coordination required during publication and crash
recovery, so RKC rejects those paths. Different local drives are supported.

On Linux, the default protected envelope is one CPU core with a 4.5 GiB hard
memory ceiling and deliberately low scheduling priority. If your session lacks
the required user-systemd delegation, RKC refuses protected work rather than
silently removing its limits. Run `rkc doctor` and follow the
[operations guide](OPERATIONS.md#protected-first-run).

macOS and Windows provide native built-in compilation, search, context, HTTP,
MCP, and the local GUI. They do not provide Linux’s cgroup guarantees. Python
workers, protected helper processes, model execution, and guarded development
automation have separate Linux requirements; they are unnecessary for the
standard GUI source workflow.

## Linux and macOS

Install the latest published portable release for the detected OS and architecture:

```sh
curl -fsSL https://github.com/neuroforge-io/RKC/releases/latest/download/install-release.sh | sh
```

The default location is `$HOME/.local`. Make its commands available in the
current shell, then open RKC:

```sh
export PATH="$HOME/.local/bin:$PATH"
rkc gui
```

The installer also prints the absolute first-run command. You can always use
`"$HOME/.local/bin/rkc" gui` with the default location. To retain the PATH change
in future terminals, add the printed export line to your shell’s startup file.
No root install is required.

To inspect the installer or use options, download it first:

```sh
curl -fsSL https://github.com/neuroforge-io/RKC/releases/latest/download/install-release.sh -o install-release.sh
sh ./install-release.sh --help
```

| Option | Effect |
| --- | --- |
| `--prefix DIRECTORY` | Install under another user-owned prefix; commands go in its `bin` directory |
| `--version TAG` | Download an existing release tag instead of `latest` |
| `--archive PATH --checksums PATH` | Install a downloaded platform ZIP using its matching checksum receipt |
| `RKC_INSTALL_PREFIX` | Set the default prefix when `--prefix` is absent |

For example, a custom location:

```sh
sh ./install-release.sh --prefix "$HOME/tools/rkc"
"$HOME/tools/rkc/bin/rkc" gui
```

## Windows

Run this in PowerShell:

```powershell
irm https://github.com/neuroforge-io/RKC/releases/latest/download/install-release.ps1 | iex
rkc gui
```

RKC installs under `$env:LOCALAPPDATA\RKC` and adds its `bin` directory to your
user PATH and the current PowerShell session. No administrator account is
required. Open a new terminal to make the PATH change visible to other shells.
The direct launch command is:

```powershell
& "$env:LOCALAPPDATA\RKC\bin\rkc.exe" gui
```

The downloaded script accepts these PowerShell parameters:

| Parameter | Effect |
| --- | --- |
| `-Prefix DIRECTORY` | Choose another user-owned install location |
| `-Version TAG` | Choose an existing release tag instead of `latest` |
| `-Archive PATH -Checksums PATH` | Install an already-downloaded platform ZIP and matching receipt |
| `-NoPath` | Leave both the saved user PATH and current session PATH unchanged |

For example, use a custom location and leave PATH unchanged:

```powershell
& ([scriptblock]::Create((irm https://github.com/neuroforge-io/RKC/releases/latest/download/install-release.ps1))) -Prefix "$env:LOCALAPPDATA\RKC" -NoPath
& "$env:LOCALAPPDATA\RKC\bin\rkc.exe" gui
```

## Manual or offline installation

From the same release, download the installer, `SHA256SUMS.txt`, and the ZIP
matching your machine:

| OS | Archive name |
| --- | --- |
| Linux | `rkc-linux-amd64.zip` or `rkc-linux-arm64.zip` |
| macOS | `rkc-darwin-amd64.zip` or `rkc-darwin-arm64.zip` |
| Windows | `rkc-windows-amd64.zip` or `rkc-windows-arm64.zip` |

Run the downloaded installer with the archive and receipt together. This
example is for Linux on Intel/AMD 64-bit:

```sh
sh ./install-release.sh --archive ./rkc-linux-amd64.zip --checksums ./SHA256SUMS.txt
```

On Windows on Intel/AMD 64-bit, from the folder containing the downloads:

```powershell
& ([scriptblock]::Create((Get-Content -Raw .\install-release.ps1))) -Archive .\rkc-windows-amd64.zip -Checksums .\SHA256SUMS.txt
```

The installers check the selected ZIP against the receipt before installing
files and reject unsafe archive paths or linked destinations. Checksums detect
mismatched downloads; obtain both archive and receipt from the release you
intend to trust. POSIX offline installation needs `unzip` and a SHA-256 utility
but does not need `curl` once all files are local.

## Build from source

This is the available installation route before portable release publication.
For Linux or macOS, install Git, Make, and the exact toolchain declared in
[`go.mod`](../go.mod) (currently Go 1.26.5), then run:

```sh
git clone https://github.com/neuroforge-io/RKC.git
cd RKC
./install.sh
export PATH="$HOME/.local/bin:$PATH"
rkc gui
```

This checkout installer builds the binaries; it is different from the portable
release installer. Its default prefix is also `$HOME/.local`, and it accepts
`--prefix`. Python is needed for the full development/validation workflow,
not to use the installed portable analysis commands.

On Windows, build the native commands from a checkout with Git and the same Go
toolchain:

```powershell
git clone https://github.com/neuroforge-io/RKC.git
Set-Location RKC
$env:CGO_ENABLED = '0'
go build -trimpath -o .\bin\rkc.exe ./cmd/rkc
go build -trimpath -o .\bin\rkc-mcp.exe ./cmd/rkc-mcp
.\bin\rkc.exe gui
```

See [development prerequisites](QUICKSTART.md#2-development-prerequisites) for
full validation. Linux guarded development targets require the same
user-systemd/cgroup setup as protected runtime work.

## Update and first-run help

Run the installer again to update its binaries and packaged support files.
It does not delete your generated atlases. Stop a running RKC app before
updating. The installers refuse symlink/reparse-point destinations and archives
that fail their checks.

If `rkc` is not found, use the absolute launch path above or the PATH command
printed by the installer. If a download returns 404, check that the chosen
release and platform asset have actually been published. For a blocked Linux
launch, use `rkc doctor` and the [operations guide](OPERATIONS.md); the installer
does not bypass resource guards.

Continue with [Quickstart](QUICKSTART.md) to choose a folder or GitHub repository
and create your first atlas.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
