# Installs the icebath release binary for this machine into ICEBATH_INSTALL_DIR or the user programs folder
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

if ($IsLinux -or $IsMacOS) { throw 'icebath: on Linux and macOS run   curl -fsSL icebath.app | sh' }

$releases = 'https://github.com/nickheyer/icebath/releases'
$base = if ($env:ICEBATH_VERSION) { "$releases/download/v$($env:ICEBATH_VERSION.TrimStart('v'))" } else { "$releases/latest/download" }
$dir = if ($env:ICEBATH_INSTALL_DIR) { $env:ICEBATH_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\icebath' }
$arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
  'X64' { 'amd64' }
  'Arm64' { 'arm64' }
  default { throw "icebath: no build for $_, see https://icebath.app" }
}

$name = "icebath-windows-$arch.exe"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile "$tmp\icebath.exe"
  Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile "$tmp\checksums.txt"
  $want = Get-Content "$tmp\checksums.txt" | ForEach-Object { $f = $_ -split '\s+'; if ($f[1] -eq $name) { $f[0] } }
  if (-not $want) { throw "icebath: checksums.txt has no entry for $name" }
  $got = (Get-FileHash -Algorithm SHA256 "$tmp\icebath.exe").Hash.ToLowerInvariant()
  if ($got -ne $want) { throw "icebath: checksum mismatch for $name" }
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  Move-Item -Force "$tmp\icebath.exe" (Join-Path $dir 'icebath.exe')
} finally {
  Remove-Item -Recurse -Force $tmp
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($userPath -split ';') -notcontains $dir) {
  [Environment]::SetEnvironmentVariable('Path', $(if ($userPath) { "$userPath;$dir" } else { $dir }), 'User')
  $env:Path = "$env:Path;$dir"
}
Write-Host "installed $(& (Join-Path $dir 'icebath.exe') -version) to $(Join-Path $dir 'icebath.exe')"
