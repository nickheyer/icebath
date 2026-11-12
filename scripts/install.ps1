# Icebath bootstrap for Windows: download the release archive for this
# machine, verify it against the release checksums, unpack icebath.exe and
# hand every option to `icebath install-self`, which owns the stages.
#
# Stage protocol kept for the setup app:
#   -Manifest             print the stage list as JSON
#   -Stage NAME [-Json]   run one stage
#   -NonInteractive       skip stages that need input
#   -IncludeDesktop       install the desktop app too
param(
    [string]$Branch = "",
    [string]$Commit = "",
    [string]$Channel = $(if ($env:ICEBATH_INSTALL_CHANNEL) { $env:ICEBATH_INSTALL_CHANNEL } else { "" }),
    [string]$IcebathHome = $(if ($env:ICEBATH_HOME) { $env:ICEBATH_HOME } else { "$env:LOCALAPPDATA\icebath" }),
    [string]$InstallDir = $(if ($env:ICEBATH_INSTALL_DIR) { $env:ICEBATH_INSTALL_DIR } elseif ($env:ICEBATH_HOME) { "$env:ICEBATH_HOME\icebath-agent" } else { "$env:LOCALAPPDATA\icebath\icebath-agent" }),
    [switch]$Manifest,
    [string]$Stage,
    [switch]$ProtocolVersion,
    [switch]$NonInteractive,
    [switch]$SkipSetup,
    [switch]$Json,
    [switch]$IncludeDesktop,
    [switch]$SkipBrowser,
    [switch]$SkipComputerUse,
    [switch]$ShowResolvedPaths
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

if ($Channel -and $Channel -notin @("stable", "main")) {
    [Console]::Error.WriteLine("-Channel must be stable or main, not $Channel")
    exit 2
}

$RepoUrl = if ($env:ICEBATH_REPO_URL) { $env:ICEBATH_REPO_URL.TrimEnd("/") } else { "https://github.com/nickheyer/icebath" }
$Version = $env:ICEBATH_VERSION

function Fail([string]$Message) {
    [Console]::Error.WriteLine("x $Message")
    exit 1
}

$arch = (Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment").PROCESSOR_ARCHITECTURE
if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
$goarch = switch ($arch) {
    "ARM64" { "arm64" }
    "AMD64" { "amd64" }
    default { Fail "unsupported architecture $arch" }
}

if (-not $Version) {
    $api = ($RepoUrl -replace "^https://github.com/", "https://api.github.com/repos/") + "/releases/latest"
    try {
        $release = Invoke-RestMethod -Uri $api -Headers @{ Accept = "application/vnd.github+json" }
    } catch {
        Fail "could not resolve the latest release from ${api}: $($_.Exception.Message)"
    }
    $Version = $release.tag_name
    if (-not $Version) { Fail "the latest release has no tag" }
}
$Version = $Version.TrimStart("v")

$work = Join-Path ([IO.Path]::GetTempPath()) ("icebath-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $work | Out-Null
try {
    $archive = "icebath_${Version}_windows_${goarch}.zip"
    $base = "$RepoUrl/releases/download/v$Version"
    Write-Host "-> Downloading icebath $Version for windows/$goarch"
    try {
        Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $work $archive) -UseBasicParsing
        Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $work "checksums.txt") -UseBasicParsing
    } catch {
        Fail "download failed: $($_.Exception.Message)"
    }
    $want = $null
    foreach ($line in Get-Content (Join-Path $work "checksums.txt")) {
        $parts = $line -split "\s+", 2
        if ($parts.Count -eq 2 -and ($parts[1] -eq $archive -or $parts[1] -eq "*$archive")) { $want = $parts[0].ToLowerInvariant(); break }
    }
    if (-not $want) { Fail "checksums.txt has no entry for $archive" }
    $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $work $archive)).Hash.ToLowerInvariant()
    if ($got -ne $want) { Fail "checksum mismatch for ${archive}: expected $want, got $got" }
    Expand-Archive -Path (Join-Path $work $archive) -DestinationPath (Join-Path $work "unpacked") -Force
    $exe = Join-Path $work "unpacked\icebath.exe"
    if (-not (Test-Path $exe)) { Fail "$archive has no icebath.exe" }

    $forward = @("install-self", "--dir", $InstallDir, "--icebath-home", $IcebathHome, "--version", $Version)
    if ($Branch) { $forward += @("--branch", $Branch) }
    if ($Commit) { $forward += @("--commit", $Commit) }
    if ($Channel) { $forward += @("--channel", $Channel) }
    if ($Stage) { $forward += @("--stage", $Stage) }
    if ($Manifest) { $forward += "--manifest" }
    if ($ProtocolVersion) { $forward += "--protocol-version" }
    if ($Json) { $forward += "--json" }
    if ($NonInteractive -or $SkipSetup) { $forward += "--non-interactive" }
    if ($IncludeDesktop) { $forward += "--include-desktop" }
    if ($SkipBrowser) { $forward += "--skip-browser" }
    if ($SkipComputerUse) { $forward += "--skip-computer-use" }
    if ($ShowResolvedPaths) { $forward += "--show-resolved-paths" }
    if ($VerbosePreference -eq "Continue") { $forward += "--verbose" }

    $env:ICEBATH_HOME = $IcebathHome
    & $exe @forward
    exit $LASTEXITCODE
} finally {
    Remove-Item -Recurse -Force -Path $work -ErrorAction SilentlyContinue
}
