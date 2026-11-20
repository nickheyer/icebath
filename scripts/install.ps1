# Downloads and verifies the icebath release archive for this machine, then hands every option to icebath install-self
[CmdletBinding(PositionalBinding=$false)]
param(
    [string]$Branch = "",
    [string]$Commit = "",
    [string]$Channel = $(if ($env:ICEBATH_INSTALL_CHANNEL) { $env:ICEBATH_INSTALL_CHANNEL } else { "" }),
    [string]$IcebathHome = $(if ($env:ICEBATH_HOME) { $env:ICEBATH_HOME } elseif ($IsLinux -or $IsMacOS) { Join-Path $HOME ".icebath" } else { "$env:LOCALAPPDATA\icebath" }),
    [string]$InstallDir = "",
    [switch]$NonInteractive,
    [switch]$SkipSetup,
    [switch]$SkipBrowser,
    [switch]$SkipComputerUse,
    [switch]$ShowResolvedPaths
)

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

if ($SkipSetup) { $NonInteractive = $true }

# A dot-source loads the functions only, the install never runs
$script:IsDotSourced = $MyInvocation.InvocationName -eq '.'
# Only a script file owns its process and may exit, iex'd text runs in the caller's session
$script:RunAsFile = [bool]{}.File
$script:BoundParams = $PSBoundParameters
$script:BinDirOnCallerPath = $null
$script:OnWindows = -not ($IsLinux -or $IsMacOS)
$script:LongProfileRoot = $null
$script:LastResolver = 'none'
$script:NormalizedPathRewrites = @{}

# Path diagnostics on stderr, silent under the machine readable -ShowResolvedPaths
function Write-PathDiag {
    param([string]$Message)
    if ($ShowResolvedPaths) { return }
    [Console]::Error.WriteLine("[icebath] $Message")
}

# Long form profile directory, empty when every reachable source is itself an 8.3 alias
function Get-LongProfileRoot {
    if ($null -ne $script:LongProfileRoot) { return $script:LongProfileRoot }
    $script:LongProfileRoot = ''
    $envProfile = [Environment]::GetEnvironmentVariable('USERPROFILE')
    $shellProfile = [Environment]::GetFolderPath('UserProfile')
    $candidates = @($envProfile, $shellProfile, "$env:HOMEDRIVE$env:HOMEPATH")
    foreach ($anchor in @($envProfile, $shellProfile)) {
        if ($anchor -and $env:USERNAME) {
            $parent = Split-Path -Parent $anchor.TrimEnd('\', '/')
            if ($parent) { $candidates += (Join-Path $parent $env:USERNAME) }
        }
    }
    foreach ($candidate in $candidates) {
        if ([string]::IsNullOrWhiteSpace($candidate)) { continue }
        $candidate = $candidate.TrimEnd('\', '/')
        if (-not $candidate) { continue }
        if ($candidate -match '~\d') { continue }
        try {
            if (Test-Path -LiteralPath $candidate -PathType Container) {
                $script:LongProfileRoot = $candidate
                break
            }
        } catch {
            Write-PathDiag "profile root candidate $candidate is unreadable"
        }
    }
    return $script:LongProfileRoot
}

# Path rebuilt onto the long profile root when its aliased component is the profile folder, else unchanged
function Expand-ShortProfileRoot {
    param([string]$Path)
    $longRoot = Get-LongProfileRoot
    if (-not $longRoot) { return $Path }
    $longRootParent = Split-Path -Parent $longRoot
    if (-not $longRootParent) { return $Path }
    $node = $Path
    $tail = ''
    while ($node -and ($node -match '~\d')) {
        $leaf = Split-Path -Leaf $node
        $parent = Split-Path -Parent $node
        if (-not $parent) { return $Path }
        if ($leaf -match '~\d') {
            if ($parent -ne $longRootParent) { return $Path }
            if ($tail) { return (Join-Path $longRoot $tail) }
            return $longRoot
        }
        $tail = if ($tail) { Join-Path $leaf $tail } else { $leaf }
        $node = $parent
    }
    return $Path
}

# Long form of an 8.3 aliased path through kernel32, then COM, then the long profile root
function ConvertTo-LongPath {
    param([string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return $Path }
    if ($Path -notmatch '~\d') {
        $script:LastResolver = 'skipped-long-path'
        return $Path
    }
    try {
        if (-not ([System.Management.Automation.PSTypeName]'IcebathInstall.LongPath').Type) {
            Add-Type -Namespace 'IcebathInstall' -Name 'LongPath' -MemberDefinition @'
[DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
public static extern int GetLongPathNameW(string lpszShortPath, System.Text.StringBuilder lpszLongPath, int cchBuffer);
'@
        }
        $buffer = New-Object System.Text.StringBuilder 4096
        $length = [IcebathInstall.LongPath]::GetLongPathNameW($Path, $buffer, $buffer.Capacity)
        if ($length -gt $buffer.Capacity) {
            $buffer = New-Object System.Text.StringBuilder $length
            $length = [IcebathInstall.LongPath]::GetLongPathNameW($Path, $buffer, $buffer.Capacity)
        }
        if ($length -gt 0) {
            $expanded = $buffer.ToString()
            if ($expanded -and $expanded -notmatch '~\d') {
                $script:LastResolver = 'kernel32'
                return $expanded
            }
        }
    } catch {
        Write-PathDiag "kernel32 cannot expand $Path"
    }
    try {
        $fso = New-Object -ComObject Scripting.FileSystemObject
        $resolved = $null
        if ($fso.FolderExists($Path)) { $resolved = $fso.GetFolder($Path).Path }
        elseif ($fso.FileExists($Path)) { $resolved = $fso.GetFile($Path).Path }
        if ($resolved -and $resolved -notmatch '~\d') {
            $script:LastResolver = 'com'
            return $resolved
        }
    } catch {
        Write-PathDiag "COM cannot expand $Path"
    }
    $rebuilt = Expand-ShortProfileRoot $Path
    $script:LastResolver = if ($rebuilt -ne $Path) { 'profile-root' } else { 'none' }
    return $rebuilt
}

# Rewrites every profile rooted variable the install reads to its long form, true when any changed
function Set-LongProfileEnvVars {
    $rewrote = $false
    $script:NormalizedPathRewrites = @{}
    foreach ($name in @('TEMP', 'TMP', 'LOCALAPPDATA', 'APPDATA', 'USERPROFILE')) {
        $current = [Environment]::GetEnvironmentVariable($name)
        if (-not $current) { continue }
        $expanded = ConvertTo-LongPath $current
        if ($expanded -and $expanded -ne $current) {
            Set-Item -Path "Env:$name" -Value $expanded
            $rewrote = $true
            $script:NormalizedPathRewrites[$name] = $expanded
        }
    }
    return $rewrote
}

function Log([string]$msg) { Write-Host "-> $msg" -ForegroundColor Cyan }
function Write-Ok([string]$msg) { Write-Host "[OK] $msg" -ForegroundColor Green }
function Write-Err([string]$msg) { Write-Host "[X] $msg" -ForegroundColor Red }

# Throws the reason, the entry below owns reporting and the exit code
function Fail([string]$msg) {
    throw $msg
}

# Normalizes the profile paths once per entry, resolves the home and install dir and records the path report
function Initialize-ResolvedPaths {
    $script:NormalizedProfilePaths = Set-LongProfileEnvVars
    $resolvedHome = ConvertTo-LongPath $IcebathHome
    $resolvedDir = if ($script:BoundParams.ContainsKey('InstallDir')) {
        ConvertTo-LongPath $InstallDir
    } elseif ($env:ICEBATH_INSTALL_DIR) {
        ConvertTo-LongPath $env:ICEBATH_INSTALL_DIR
    } else {
        Join-Path $resolvedHome 'icebath-agent'
    }
    $cmpStore = if ($env:ICEBATH_RUNTIME_DIR) { "$env:ICEBATH_RUNTIME_DIR" } else { "$resolvedHome" }
    $cmpStore = $cmpStore.TrimEnd('\', '/')
    $cmpDir = "$resolvedDir".TrimEnd('\', '/')
    $dirPrefix = $cmpDir + [IO.Path]::DirectorySeparatorChar
    $storeInside = $cmpStore -eq $cmpDir -or $cmpStore.StartsWith($dirPrefix, [StringComparison]::OrdinalIgnoreCase)
    if ($storeInside) {
        Fail "IcebathHome ($resolvedHome) cannot be the install directory or live inside it ($resolvedDir): the tool store would land inside the install. Use a separate -IcebathHome, or point ICEBATH_RUNTIME_DIR outside -InstallDir."
    }
    Set-Variable -Scope 1 -Name IcebathHome -Value $resolvedHome
    Set-Variable -Scope 1 -Name InstallDir -Value $resolvedDir
    $env:ICEBATH_HOME = $resolvedHome
    $script:ResolvedPathReport = [ordered]@{
        long_profile_root = (Get-LongProfileRoot)
        normalized        = $script:NormalizedPathRewrites
        resolver          = $script:LastResolver
        temp              = [IO.Path]::GetTempPath()
        icebath_home      = $resolvedHome
        install_dir       = $resolvedDir
    }
}

# Release platform of this machine, the registry architecture on Windows so x64 PowerShell on ARM stages arm64
function Get-ReleasePlatform {
    if ($script:OnWindows) {
        $arch = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment' -ErrorAction SilentlyContinue).PROCESSOR_ARCHITECTURE
        if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
        $os = 'windows'
    } else {
        $arch = "$([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)"
        $os = if ($IsMacOS) { 'darwin' } else { 'linux' }
    }
    $goarch = switch ($arch) {
        { $_ -in @('ARM64', 'Arm64') } { 'arm64' }
        { $_ -in @('AMD64', 'X64') } { 'amd64' }
        default { Fail "no release build for this machine ($arch)" }
    }
    return @{ Os = $os; Arch = $goarch }
}

# Size the server reports for a download, zero when it names none
function Get-RemoteContentLength([string]$Uri) {
    try {
        $resp = Invoke-WebRequest -Uri $Uri -Method Head -UseBasicParsing -ErrorAction Stop
        $len = $resp.Headers['Content-Length']
        if ($len) { return [long]([string]$len -split ',' | Select-Object -First 1) }
    } catch {
        Write-Verbose "no Content-Length for $Uri"
    }
    return 0
}

# Downloads on a runspace while this thread draws progress from the bytes on disk, rethrowing the request's error
function Invoke-DownloadWithProgress {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$OutFile
    )
    if (Test-Path $OutFile) { Remove-Item -Path $OutFile -Force -ErrorAction SilentlyContinue }
    $totalBytes = Get-RemoteContentLength $Uri
    $activity = "Downloading $(Split-Path -Leaf $Uri)"
    $ps = [powershell]::Create()
    $ps.AddScript({
        param($Uri, $OutFile)
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing
    }).AddArgument($Uri).AddArgument($OutFile) | Out-Null
    $handle = $ps.BeginInvoke()
    try {
        while (-not $handle.IsCompleted) {
            Start-Sleep -Milliseconds 200
            $haveBytes = if (Test-Path $OutFile) { (Get-Item $OutFile).Length } else { 0 }
            $haveMb = [math]::Round($haveBytes / 1MB, 1)
            if ($totalBytes -gt 0) {
                $pct = [math]::Min(100, [math]::Round(($haveBytes / $totalBytes) * 100))
                Write-Progress -Activity $activity -Status "$haveMb MB / $([math]::Round($totalBytes / 1MB, 1)) MB" -PercentComplete $pct
            } else {
                Write-Progress -Activity $activity -Status "$haveMb MB (size unknown)" -PercentComplete -1
            }
        }
        $ps.EndInvoke($handle) | Out-Null
        $streamError = if ($ps.Streams.Error.Count) { $ps.Streams.Error[0].Exception } else { $null }
    } catch {
        $inner = $_.Exception.InnerException
        if ($inner) { throw $inner } else { throw }
    } finally {
        Write-Progress -Activity $activity -Completed
        $ps.Dispose()
    }
    if ($streamError) { throw $streamError }
}

# Fetches a release asset, a TLS trust failure kept apart from a download failure
function Receive-ReleaseAsset([string]$Url, [string]$OutFile) {
    try {
        Invoke-DownloadWithProgress -Uri $Url -OutFile $OutFile
    } catch {
        $errorType = $_.Exception.GetType().FullName
        if ($_.Exception -is [System.Net.WebException] -and $_.Exception.Status -in @('TrustFailure', 'SecureChannelFailure')) {
            Fail "TLS failure downloading ${Url}: $($_.Exception.Message)"
        }
        if ($errorType -eq 'System.Net.Http.HttpRequestException') {
            $inner = $_.Exception.InnerException
            if ($inner -and $inner.GetType().FullName -eq 'System.Security.Authentication.AuthenticationException') {
                Fail "TLS failure downloading ${Url}: $($inner.Message)"
            }
        }
        Fail "failed to download ${Url}: $($_.Exception.Message)"
    }
}

# Web root of the release repository, ICEBATH_REPO_URL first, without a trailing slash or .git
function Get-RepoWeb {
    $repo = if ($env:ICEBATH_REPO_URL) { $env:ICEBATH_REPO_URL } else { "https://github.com/nickheyer/icebath" }
    $repo = $repo.TrimEnd('/')
    if ($repo.EndsWith('.git')) { $repo = $repo.Substring(0, $repo.Length - 4) }
    return $repo
}

# Releases API root of a repository, api.github.com for github.com and the GitHub Enterprise path elsewhere
function Get-RepoApi([string]$Web) {
    if ($Web -match '^https://github\.com/([^/]+/[^/]+)$') { return "https://api.github.com/repos/$($Matches[1])" }
    if ($Web -match '^(https?)://([^/]+)/([^/]+/[^/]+)$') { return "$($Matches[1])://$($Matches[2])/api/v3/repos/$($Matches[3])" }
    Fail "repository URL $Web does not name owner/repo"
}

# Downloads the release archive and checksums, verifies the archive and unpacks it, returning the unpacked binary
function Receive-Release([string]$Web, [string]$Version, [hashtable]$Platform, [string]$Work) {
    $ext = if ($Platform.Os -eq 'windows') { 'zip' } else { 'tar.gz' }
    $archive = "icebath_${Version}_$($Platform.Os)_$($Platform.Arch).$ext"
    $base = "$Web/releases/download/v$Version"
    Log "Downloading icebath $Version for $($Platform.Os)/$($Platform.Arch)"
    Receive-ReleaseAsset "$base/$archive" (Join-Path $Work $archive)
    Receive-ReleaseAsset "$base/checksums.txt" (Join-Path $Work "checksums.txt")
    $want = $null
    foreach ($line in Get-Content (Join-Path $Work "checksums.txt")) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and ($parts[1] -eq $archive -or $parts[1] -eq "*$archive")) { $want = $parts[0].ToLowerInvariant(); break }
    }
    if (-not $want) { Fail "checksums.txt has no entry for $archive" }
    $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $Work $archive)).Hash.ToLowerInvariant()
    if ($got -ne $want) { Fail "checksum mismatch for ${archive}: expected $want, got $got" }
    $unpacked = Join-Path $Work "unpacked"
    New-Item -ItemType Directory -Force -Path $unpacked | Out-Null
    if ($ext -eq 'zip') {
        Expand-Archive -Path (Join-Path $Work $archive) -DestinationPath $unpacked -Force
        $exe = Join-Path $unpacked "icebath.exe"
    } else {
        & tar -xzf (Join-Path $Work $archive) -C $unpacked
        if ($LASTEXITCODE) { Fail "could not unpack $archive (tar exit $LASTEXITCODE)" }
        $exe = Join-Path $unpacked "icebath"
    }
    if (-not (Test-Path -LiteralPath $exe)) { Fail "$archive has no $(Split-Path -Leaf $exe)" }
    return $exe
}

# Puts the launcher directory first on this session's PATH, remembering whether the caller's shell already had it
function Set-LauncherSessionPath([string]$BinDir) {
    $sep = [IO.Path]::PathSeparator
    $entries = @($env:PATH -split [regex]::Escape("$sep") | ForEach-Object { $_.TrimEnd('\', '/') })
    $onPath = $entries -contains $BinDir.TrimEnd('\', '/')
    if ($null -eq $script:BinDirOnCallerPath) { $script:BinDirOnCallerPath = $onPath }
    if (-not $onPath) { $env:PATH = "$BinDir$sep$env:PATH" }
}

# Tells a script file run how its parent shell reaches the command, iex'd runs reach it through the session PATH
function Write-PathReloadHint {
    if (-not $script:RunAsFile -or $script:BinDirOnCallerPath -ne $false) { return }
    if ($script:OnWindows) {
        Log 'Restart your terminal to use icebath, or run: $env:PATH = [Environment]::GetEnvironmentVariable(''Path'',''User'') + '';'' + [Environment]::GetEnvironmentVariable(''Path'',''Machine'')'
    } else {
        Log "Restart your terminal to use icebath, or run: `$env:PATH = `"$($script:BinDir):`$env:PATH`""
    }
}

# Exits a script file with code, an iex'd run setting LASTEXITCODE without closing the caller's window
function Complete-Run([int]$Code) {
    if ($script:RunAsFile) { exit $Code }
    $global:LASTEXITCODE = $Code
}

if ($script:IsDotSourced) {
    Write-Verbose "[icebath] install.ps1 was dot-sourced; definitions only, no execution"
    return
}

$work = $null
try {
    Initialize-ResolvedPaths
    if ($VerbosePreference -ne 'SilentlyContinue') { $env:ICEBATH_INSTALL_VERBOSE = "1" }
    if ($ShowResolvedPaths) {
        $script:ResolvedPathReport | ConvertTo-Json -Depth 5 -Compress | Write-Output
        Complete-Run 0
        return
    }
    $platform = Get-ReleasePlatform
    if ($Channel -and $Channel -cnotin @('stable', 'main')) { Fail "-Channel must be stable or main, not $Channel" }
    $web = Get-RepoWeb
    $version = $env:ICEBATH_VERSION
    if (-not $version) {
        $api = (Get-RepoApi $web) + "/releases/latest"
        try {
            $release = Invoke-RestMethod -Uri $api -Headers @{ Accept = "application/vnd.github+json" } -UseBasicParsing
        } catch {
            Fail "could not resolve the latest release from ${api}: $($_.Exception.Message)"
        }
        $version = $release.tag_name
        if (-not $version) { Fail "the latest release of $web has no tag" }
    }
    $version = $version.TrimStart('v')
    $work = Join-Path ([IO.Path]::GetTempPath()) ("icebath-install-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $work | Out-Null
    $exe = Receive-Release $web $version $platform $work
    if (-not $script:OnWindows) { & chmod 0755 $exe }

    $forward = @("install-self", "--dir", $InstallDir, "--icebath-home", $IcebathHome, "--version", $version)
    if ($Branch) { $forward += @("--branch", $Branch) }
    if ($Commit) { $forward += @("--commit", $Commit) }
    if ($Channel) { $forward += @("--channel", $Channel) }
    if ($NonInteractive) { $forward += "--non-interactive" }
    if ($SkipBrowser) { $forward += "--skip-browser" }
    if ($SkipComputerUse) { $forward += "--skip-computer-use" }
    if ($VerbosePreference -ne 'SilentlyContinue') { $forward += "--verbose" }

    $previousPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $exe @forward
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousPreference
    }
    if ($code -eq 0) {
        $script:BinDir = if ($script:OnWindows) { Join-Path $IcebathHome "bin" } else { Join-Path $HOME ".local/bin" }
        Set-LauncherSessionPath $script:BinDir
        Write-PathReloadHint
    }
    Complete-Run $code
} catch {
    Write-Err "$_"
    Complete-Run 1
} finally {
    if ($work) { Remove-Item -Recurse -Force -Path $work -ErrorAction SilentlyContinue }
}
