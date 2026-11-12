$ErrorActionPreference = 'Stop'
$toolsDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
Install-ChocolateyZipPackage -PackageName 'icebath' -Url64bit '@URL@' -Checksum64 '@SHA256@' -ChecksumType64 'sha256' -UnzipLocation $toolsDir
Set-Content -Path (Join-Path $toolsDir '.install_method') -Value 'chocolatey' -Encoding ascii
