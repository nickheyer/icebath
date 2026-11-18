$ErrorActionPreference = 'Stop'
$toolsDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
Install-ChocolateyZipPackage -PackageName 'icebath' -Url64bit '@URL@' -Checksum64 '@SHA256@' -ChecksumType64 'sha256' -UnzipLocation $toolsDir
Set-Content -Path (Join-Path $toolsDir '.install_method') -Value 'chocolatey' -Encoding ascii
$icebathHome = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
$env:ICEBATH_HOME = $icebathHome
& (Join-Path $toolsDir 'icebath.exe') pm install --extra nemo-relay
$relayExit = $LASTEXITCODE
Remove-Item Env:ICEBATH_HOME
Remove-Item -Recurse -Force $icebathHome -ErrorAction SilentlyContinue
if ($relayExit -ne 0) { throw "NeMo Relay runtime provisioning failed with exit code $relayExit" }
