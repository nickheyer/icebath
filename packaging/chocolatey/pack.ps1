# Fills the package templates for a release tag from its checksums and packs the nupkg into an output directory
param([Parameter(Mandatory)][string]$Tag, [Parameter(Mandatory)][string]$Checksums, [Parameter(Mandatory)][string]$Out)
$ErrorActionPreference = 'Stop'
$version = $Tag.TrimStart('v')
$zip = "icebath_${version}_windows_amd64.zip"
$sha = Get-Content $Checksums | ForEach-Object { $f = $_ -split '\s+'; if ($f[1] -eq $zip) { $f[0] } }
if (-not $sha) { throw "pack: $Checksums has no entry for $zip" }
$work = Join-Path ([IO.Path]::GetTempPath()) "icebath-choco-$version"
Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
Copy-Item -Recurse $PSScriptRoot $work
Remove-Item (Join-Path $work 'pack.ps1')
(Get-Content "$work\icebath.nuspec" -Raw).Replace('@VERSION@', $version) | Set-Content "$work\icebath.nuspec" -NoNewline
$install = (Get-Content "$work\tools\chocolateyinstall.ps1" -Raw).Replace('@URL@', "https://github.com/nickheyer/icebath/releases/download/$Tag/$zip").Replace('@SHA256@', $sha)
Set-Content "$work\tools\chocolateyinstall.ps1" $install -NoNewline
New-Item -ItemType Directory -Force -Path $Out | Out-Null
choco pack "$work\icebath.nuspec" --outputdirectory $Out
if ($LASTEXITCODE -ne 0) { throw "pack: choco pack failed" }
