# Build the Windows installer: the agent, built exactly as scripts/build-release.sh builds
# aisathome-node-windows-amd64.exe, packed by Inno Setup 6 (preinstalled on GitHub's
# windows-2025 image). Writes aisathome-node-windows-amd64.exe and
# aisathome-node-setup-windows-amd64.exe into OutDir.
#
#   pwsh node/packaging/windows/build-installer.ps1 -Version <version> -OutDir <dir> [-EngineDir <dir>]
#
# Check out with core.autocrlf false: the agent embeds its page's files, and CRLF line ends
# would make a different binary from the release's.
#
# SPDX-License-Identifier: Apache-2.0
param(
  [Parameter(Mandatory)][string]$Version,
  [Parameter(Mandatory)][string]$OutDir,
  [string]$EngineDir = ''
)
$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^[A-Za-z0-9._-]+$') { throw "bad version: $Version" }
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
New-Item -ItemType Directory -Force $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path
$exe = Join-Path $OutDir 'aisathome-node-windows-amd64.exe'

Push-Location $root
try {
  $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = 'amd64'
  go build -trimpath `
    -ldflags "-s -w -X main.version=$Version -X github.com/aisathome/aisathome/node.Version=$Version" `
    -o $exe ./node/cmd/aisathome-node
  if ($LASTEXITCODE -ne 0) { throw "go build failed ($LASTEXITCODE)" }
} finally { Pop-Location }

# Inno Setup's ExtractTemporaryFile (the update's "service stop") finds the file by name; the
# same name for source and destination leaves no doubt which one.
$stage = Join-Path $OutDir 'stage'
New-Item -ItemType Directory -Force $stage | Out-Null
$nodeExe = Join-Path $stage 'aisathome-node.exe'
Copy-Item -Force $exe $nodeExe

# The installer's numeric file version: 0.5.1 from v0.5.1 or v0.5.1-rc1, else 0.0.0.
$num = '0.0.0'
if ($Version -match '^v?(\d+\.\d+\.\d+)') { $num = $Matches[1] }

$iscc = @(
  (Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6\ISCC.exe'),
  (Join-Path $env:ProgramFiles 'Inno Setup 6\ISCC.exe')
) | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $iscc) { $iscc = (Get-Command ISCC.exe -ErrorAction SilentlyContinue).Source }
if (-not $iscc) { throw 'Inno Setup 6 (ISCC.exe) not found' }

$defs = @("/DAppVersion=$Version", "/DFileVersion=$num", "/DNodeExe=$nodeExe", "/DOutDir=$OutDir")
if ($EngineDir) { $defs += "/DEngineDir=$((Resolve-Path $EngineDir).Path)" }
& $iscc /Qp @defs (Join-Path $PSScriptRoot 'aisathome-node.iss')
if ($LASTEXITCODE -ne 0) { throw "ISCC failed ($LASTEXITCODE)" }
Get-ChildItem $OutDir -Filter 'aisathome-node-*.exe' | Format-Table Name, Length
