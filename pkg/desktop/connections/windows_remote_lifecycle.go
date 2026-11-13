package connections

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

// Argv word of the remote icebath that runs one Windows SSH runtime operation
const windowsSSHRuntimeEntry = "__icebath_windows_ssh_runtime__"

// WindowsRuntime is the located install of a Windows remote, its executable and boot selecting runtime command
type WindowsRuntime struct {
	OS             string
	Arch           string
	HermesHome     string
	HermesPath     string
	Executable     string
	LauncherPath   string
	RuntimeCommand []string
}

// Argv of one runtime helper operation through the remote icebath
func windowsHelperArgv(runtime *WindowsRuntime, operation string, args []string) []string {
	return append([]string{runtime.HermesPath, windowsSSHRuntimeEntry, operation}, args...)
}

const windowsNoReparsePs = "function Assert-NoReparse([string]$candidate,[bool]$allowMissing=$false){\r\n" +
	`if([string]::IsNullOrWhiteSpace($candidate)){throw "Empty Windows path"}` + "\r\n" +
	`$current=[IO.Path]::GetFullPath($candidate);$first=$true` + "\r\n" +
	`while($true){` + "\r\n" +
	`try{$item=Get-Item -LiteralPath $current -Force -ErrorAction Stop}catch [Management.Automation.ItemNotFoundException]{if(-not $allowMissing -and $first){throw "Path was not found: $candidate"};$parent=[IO.Path]::GetDirectoryName($current);if(-not $parent -or $parent -eq $current){break};$current=$parent;$first=$false;continue}` + "\r\n" +
	`if(($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){throw "Path contains a link or reparse point: $current"}` + "\r\n" +
	`$parent=[IO.Path]::GetDirectoryName($current);if(-not $parent -or $parent -eq $current){break};$current=$parent;$first=$false` + "\r\n" +
	"}\r\n" +
	"}"

// Same reparse guard that tolerates an empty candidate, the form the platform and marker probes use
var windowsLenientNoReparse = []string{
	`function Assert-NoReparse([string]$candidate,[bool]$allowMissing=$false){`,
	`if([string]::IsNullOrWhiteSpace($candidate)){return}`,
	`$current=[IO.Path]::GetFullPath($candidate);$first=$true`,
	`while($true){`,
	`try{$item=Get-Item -LiteralPath $current -Force -ErrorAction Stop}catch [Management.Automation.ItemNotFoundException]{if(-not $allowMissing -and $first){throw "Path was not found: $candidate"};$parent=[IO.Path]::GetDirectoryName($current);if(-not $parent -or $parent -eq $current){break};$current=$parent;$first=$false;continue}`,
	`if(($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){throw "Path contains a link or reparse point: $current"}`,
	`$parent=[IO.Path]::GetDirectoryName($current);if(-not $parent -or $parent -eq $current){break};$current=$parent;$first=$false`,
	`}`,
	`}`,
}

// PowerShell single quoted literal
func PsLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Base64 of the UTF-16LE script, the -EncodedCommand form
func EncodedPowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(units)*2)
	for _, u := range units {
		b = append(b, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// powershell.exe invocation running script from -EncodedCommand
func PowerShellCommand(script string) string {
	return "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + EncodedPowerShell(script)
}

var (
	clixmlBlock = regexp.MustCompile(`(?s)#< CLIXML.*?</Objs>`)
	psErrorTag  = regexp.MustCompile(`(?s)<S S="Error">(.*?)</S>`)
	psHexEscape = regexp.MustCompile(`_x([0-9a-fA-F]{4})_`)
	readyLineRE = regexp.MustCompile(`(?m)^ICEBATH_(?:BACKEND|DASHBOARD)_READY port=(\d+)`)
	creationNs  = regexp.MustCompile(`^[0-9]{10,20}$`)
	windowsAbs  = regexp.MustCompile(`(?i)^[a-z]:[\\/]`)
)

// Meaningful stdout lines of a PowerShell exec, the BOM and every CLIXML progress block dropped
func StripPowerShellNoise(stdout string) []string {
	text := strings.TrimPrefix(stdout, string(rune(0xfeff)))
	text = clixmlBlock.ReplaceAllString(text, "")
	var out []string
	for _, line := range crlfSplit.Split(text, -1) {
		if strings.TrimFunc(line, jsSpace) != "" && !strings.HasPrefix(strings.TrimLeftFunc(line, jsSpace), "#< CLIXML") {
			out = append(out, line)
		}
	}
	return out
}

// Self contained bootstrap that inflates the script packed fifteen bits per BMP character
func compressedPowerShellBootstrap(script string) string {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, 9)
	_, _ = w.Write([]byte(script))
	_ = w.Close()
	var chars []rune
	bitBuf, bitCount := uint32(0), uint(0)
	emit := func() {
		value := bitBuf & 0x7fff
		base := uint32(0x0200)
		if value < 0x1f00 {
			base = 0x0100
		}
		chars = append(chars, rune(base+value))
	}
	for _, b := range buf.Bytes() {
		bitBuf |= uint32(b) << bitCount
		bitCount += 8
		for bitCount >= 15 {
			emit()
			bitBuf >>= 15
			bitCount -= 15
		}
	}
	if bitCount > 0 {
		emit()
	}
	return `$ProgressPreference="SilentlyContinue";[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false);$m=[IO.MemoryStream]::new();$b=0;$n=0;foreach($c in [char[]]'` +
		string(chars) +
		`'){$v=[int]$c;$v-=if($v-lt8192){256}else{512};$b=$b-bor($v-shl$n);$n+=15;while($n-ge8){$m.WriteByte($b-band255);$b=$b-shr8;$n-=8}};$m.Position=0;& ([ScriptBlock]::Create([IO.StreamReader]::new([IO.Compression.DeflateStream]::new($m,[IO.Compression.CompressionMode]::Decompress),[Text.Encoding]::UTF8).ReadToEnd()))`
}

// PowerShell command carrying a deflated script under the cmd.exe length limit
func CompressedPowerShellCommand(script string) string {
	return PowerShellCommand(compressedPowerShellBootstrap(script))
}

// JSON.parse of the last meaningful line, its error text as V8 words it
func parseLastJSONLine(lines []string) (any, error) {
	text := "null"
	if len(lines) > 0 && lines[len(lines)-1] != "" {
		text = lines[len(lines)-1]
	}
	v, err := jsjson.ParseOrdered(text)
	if err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) && int(se.Offset) > 0 && int(se.Offset) <= len(text) {
			return nil, fmt.Errorf("Unexpected token '%c', \"%s\" is not valid JSON", []rune(text[se.Offset-1:])[0], text)
		}
		return nil, errors.New("Unexpected end of JSON input")
	}
	return v, nil
}

func ordered(v any) *jsjson.OrderedObject {
	o, _ := v.(*jsjson.OrderedObject)
	return o
}

func ostr(o *jsjson.OrderedObject, key string) string {
	if o == nil {
		return ""
	}
	return jsOrString(o.Get(key))
}

// Platform JSON of the Windows file probe, the launcher, home and runtime executable it located
func probeWindowsFiles(ssh RemoteShell, explicitHermesPath string) (*WindowsRuntime, error) {
	lines := []string{`$ProgressPreference="SilentlyContinue"`, `$ErrorActionPreference="Stop"`}
	lines = append(lines, windowsLenientNoReparse...)
	lines = append(lines,
		`$explicit=`+PsLiteral(explicitHermesPath),
		`if($explicit){Assert-NoReparse $explicit $false;$explicitPython=[IO.Path]::Combine([IO.Path]::GetDirectoryName($explicit), "python.exe");Assert-NoReparse $explicitPython $true}`,
		`$hermesHome=$env:ICEBATH_HOME`,
		`if(-not $hermesHome -or -not (Test-Path -LiteralPath $hermesHome -PathType Container)){$hermesHome=Join-Path $env:LOCALAPPDATA "icebath"}`,
		`Assert-NoReparse $hermesHome $true`,
		`$candidate=[IO.Path]::Combine($hermesHome, "icebath-agent\venv\Scripts\icebath.exe")`,
		`$candidatePython=[IO.Path]::Combine([IO.Path]::GetDirectoryName($candidate), "python.exe")`,
		`Assert-NoReparse $candidate $true`,
		`Assert-NoReparse $candidatePython $true`,
		`$profileCandidate=[IO.Path]::Combine($HOME, "icebath-agent\.venv\Scripts\icebath.exe")`,
		`$profileCandidatePython=[IO.Path]::Combine([IO.Path]::GetDirectoryName($profileCandidate), "python.exe")`,
		`Assert-NoReparse $profileCandidate $true`,
		`Assert-NoReparse $profileCandidatePython $true`,
		`$fallbackHomeCandidate=Join-Path $hermesHome "icebath-agent\venv\Scripts\icebath.exe"`,
		`$fallbackProfileCandidate=Join-Path $HOME "icebath-agent\.venv\Scripts\icebath.exe"`,
		`$candidates=@()`,
		`if($explicit){$candidates+=$explicit}`,
		`$cmd=@(Get-Command icebath -CommandType Application -ErrorAction SilentlyContinue)|Select-Object -First 1`,
		`if($cmd){Assert-NoReparse $cmd.Source $true;$cmdPython=[IO.Path]::Combine([IO.Path]::GetDirectoryName($cmd.Source), "python.exe");Assert-NoReparse $cmdPython $true;$candidates+=$cmd.Source}`,
		`$candidates+=Join-Path $hermesHome "bin\icebath.exe"`,
		`$candidates+=Join-Path $hermesHome "bin\icebath.cmd"`,
		`$candidates+=$fallbackHomeCandidate`,
		`$candidates+=$fallbackProfileCandidate`,
		`$hermes=$null`,
		`foreach($candidate in $candidates){Assert-NoReparse $candidate $true;$candidatePython=[IO.Path]::Combine([IO.Path]::GetDirectoryName($candidate), "python.exe");Assert-NoReparse $candidatePython $true;try{$item=Get-Item -LiteralPath $candidate -Force -ErrorAction Stop;if(($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0 -and -not $item.PSIsContainer){$pair=Test-Path -LiteralPath $candidatePython -PathType Leaf;if($pair -or (Split-Path -Leaf (Split-Path -Parent $candidate)) -ieq "bin"){$hermes=$item.FullName;break}}}catch [Management.Automation.ItemNotFoundException]{continue}}`,
		`if(-not $hermes){throw "Icebath is not installed on the remote Windows host."}`,
		`Assert-NoReparse $hermes $false`,
		`if($explicit -and $hermes -ne $explicit){throw "The configured Icebath path is not an executable file."}`,
		`$python=[IO.Path]::Combine([IO.Path]::GetDirectoryName($hermes), "python.exe")`,
		`if(Test-Path -LiteralPath $python -PathType Leaf){Assert-NoReparse $python $false}else{$python=""}`,
		`[ordered]@{os="Windows";arch=$env:PROCESSOR_ARCHITECTURE;hermesHome=$hermesHome;hermesPath=$hermes;launcherPath=$hermes;executable=$python}|ConvertTo-Json -Compress`,
	)
	out, err := ssh.Exec(CompressedPowerShellCommand(strings.Join(lines, "\r\n")), SSHExecOptions{})
	if err != nil {
		return nil, err
	}
	meaningful := StripPowerShellNoise(out)
	parsed, err := parseLastJSONLine(meaningful)
	if err != nil {
		return nil, err
	}
	o := ordered(parsed)
	if ostr(o, "os") == "" || ostr(o, "arch") == "" {
		last := ""
		if len(meaningful) > 0 {
			last = meaningful[len(meaningful)-1]
		}
		return nil, fmt.Errorf("Windows probe did not return the expected platform JSON: %s", utf16Prefix(last, 200))
	}
	return &WindowsRuntime{OS: ostr(o, "os"), Arch: ostr(o, "arch"), HermesHome: ostr(o, "hermesHome"), HermesPath: ostr(o, "hermesPath"),
		LauncherPath: ostr(o, "launcherPath"), Executable: ostr(o, "executable")}, nil
}

// First n UTF-16 units of s, the slice a JavaScript string takes
func utf16Prefix(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}

// Located Windows install with its boot selecting runtime, resolved only after the marker gate
func probeWindowsRemote(ssh RemoteShell, explicitHermesPath string) (*WindowsRuntime, error) {
	runtime, err := probeWindowsFiles(ssh, explicitHermesPath)
	if err != nil {
		return nil, err
	}
	if runtime.LauncherPath != "" {
		if err := AssertWindowsRemoteInstallUpdateClear(ssh, runtime.HermesHome, ""); err != nil {
			return nil, err
		}
		return resolveWindowsLauncher(ssh, runtime)
	}
	return runtime, nil
}

func isTransportKind(kind string) bool {
	return kind == SSHErrorAuthFailed || kind == SSHErrorHostKeyChanged || kind == SSHErrorTimeout || kind == SSHErrorUnreachable
}

// Runtime command the launcher prints, refusing a launcher that cannot report one
func resolveWindowsLauncher(ssh RemoteShell, runtime *WindowsRuntime) (*WindowsRuntime, error) {
	script := strings.Join([]string{
		`$ProgressPreference="SilentlyContinue"`,
		`$ErrorActionPreference="Stop"`,
		windowsNoReparsePs,
		"Assert-NoReparse " + PsLiteral(runtime.HermesPath) + " $false",
		"$lines=@(& " + PsLiteral(runtime.HermesPath) + " '--print-runtime-command');$code=$LASTEXITCODE",
		`if($code -ne 0){throw "The Icebath launcher does not support --print-runtime-command; refresh the Windows installation launcher."}`,
		`$command=$lines[-1]|ConvertFrom-Json`,
		`if($command -isnot [array] -or $command.Count -lt 2 -or -not [IO.Path]::IsPathRooted([string]$command[0])){throw "Invalid Icebath runtime command"}`,
		`foreach($part in $command){if($part -isnot [string] -or $part.Contains([char]0)){throw "Invalid Icebath runtime argument"}}`,
		`Assert-NoReparse ([string]$command[0]) $false`,
		`[ordered]@{executable=[string]$command[0];runtimeCommand=@($command)}|ConvertTo-Json -Depth 5 -Compress`,
	}, "\r\n")
	resolved, err := func() (*WindowsRuntime, error) {
		out, err := ssh.Exec(CompressedPowerShellCommand(script), SSHExecOptions{})
		if err != nil {
			return nil, err
		}
		lines := StripPowerShellNoise(out)
		parsed, err := parseLastJSONLine(lines)
		if err != nil {
			return nil, err
		}
		o := ordered(parsed)
		invalid := errors.New("Invalid Windows launcher runtime response.")
		if o == nil {
			return nil, invalid
		}
		parts, ok := o.Get("runtimeCommand").([]any)
		if !ok {
			return nil, invalid
		}
		command := make([]string, 0, len(parts))
		for _, p := range parts {
			s, ok := p.(string)
			if !ok || strings.Contains(s, "\x00") {
				return nil, invalid
			}
			command = append(command, s)
		}
		exe, ok := o.Get("executable").(string)
		if !ok || !windowsAbs.MatchString(exe) {
			return nil, invalid
		}
		merged := *runtime
		merged.Executable = exe
		merged.RuntimeCommand = command
		return &merged, nil
	}()
	if err != nil {
		if isTransportKind(errKind(err)) {
			return nil, err
		}
		return nil, lifecycleError("update-required", "Refresh the remote Windows Icebath launcher: its boot-selecting runtime could not be resolved.", err)
	}
	return resolved, nil
}

// Defines $checkoutRoots, the install checkout and the checkout of the runtime executable
func windowsCheckoutRootsScript(executable string) string {
	runtimeRoot := "$null"
	if executable != "" {
		runtimeRoot = "[IO.Path]::GetDirectoryName([IO.Path]::GetDirectoryName([IO.Path]::GetDirectoryName(" + PsLiteral(executable) + ")))"
	}
	return `$checkoutRoots=@([IO.Path]::Combine($installRoot,"icebath-agent"),` + runtimeRoot + `)`
}

const markerNoFollowType = "Add-Type -TypeDefinition @'\n" +
	"using System;\n" +
	"using System.IO;\n" +
	"using System.Runtime.InteropServices;\n" +
	"using Microsoft.Win32.SafeHandles;\n" +
	"public static class HermesMarkerNoFollow {\n" +
	"  [DllImport(\"kernel32.dll\", CharSet=CharSet.Unicode, SetLastError=true)]\n" +
	"  private static extern SafeFileHandle CreateFile(string name, uint access, uint share, IntPtr security, uint creation, uint flags, IntPtr template);\n" +
	"  public static FileStream OpenRead(string name) {\n" +
	"    var handle=CreateFile(name, 0x80000000, 0x00000007, IntPtr.Zero, 3, 0x00200000, IntPtr.Zero);\n" +
	"    if(handle.IsInvalid) Marshal.ThrowExceptionForHR(Marshal.GetHRForLastWin32Error());\n" +
	"    return new FileStream(handle, FileAccess.Read);\n" +
	"  }\n" +
	"}\n" +
	"'@\n"

// Marker gate probe reading the install wide update marker fail closed without following links
func windowsUpdateMarkerProbeScript(hermesHome, executable string) string {
	lines := []string{`$ProgressPreference="SilentlyContinue"`, `$ErrorActionPreference="Stop"`, markerNoFollowType, WindowsMarkerJudgePs, WindowsCheckoutLockPs}
	lines = append(lines, windowsLenientNoReparse...)
	lines = append(lines,
		"$hermesHome="+PsLiteral(hermesHome),
		`$installRoot=$hermesHome`,
		`$parent=Split-Path -Parent $hermesHome`,
		`if((Split-Path -Leaf $parent) -ieq "profiles"){$installRoot=Split-Path -Parent $parent}`,
		`$marker=Join-Path $installRoot ".icebath-update-in-progress"`,
		windowsCheckoutRootsScript(executable),
		`$result="UNCERTAIN"`,
		`$stream=$null;$memory=$null`,
		`try{`,
		`Assert-NoReparse $marker $true`,
		`if(-not (Test-Path -LiteralPath $marker -PathType Leaf)){$result="CLEAR"}else{$stream=[HermesMarkerNoFollow]::OpenRead($marker)`,
		`Assert-NoReparse $marker $false`,
		`$memory=New-Object IO.MemoryStream;$stream.CopyTo($memory);$bytes=$memory.ToArray()`,
		`if($bytes.Length -le 4096){`,
		`$utf8=[Text.UTF8Encoding]::new($false,$true)`,
		`$text=$utf8.GetString($bytes)`,
		`$result=Get-MarkerVerdict $text`,
		`if($result -eq "CLEAR" -and (Test-CheckoutLockHeld $checkoutRoots)){$result="HELD"}`,
		`}`,
		`}}catch [IO.FileNotFoundException]{$result="CLEAR"}catch{$result="UNCERTAIN"}finally{if($memory){$memory.Dispose()};if($stream){$stream.Dispose()}}`,
		`Write-Output $result`,
	)
	return strings.Join(lines, "\r\n")
}

// Fail closed install marker gate of a Windows remote through PowerShell and .NET only
func AssertWindowsRemoteInstallUpdateClear(ssh RemoteShell, hermesHome, executable string) error {
	out, err := ssh.Exec(CompressedPowerShellCommand(windowsUpdateMarkerProbeScript(hermesHome, executable)), SSHExecOptions{})
	if err != nil {
		return lifecycleError("update-in-progress", "Could not prove that the remote Icebath install is clear for SSH startup.", err)
	}
	lines := StripPowerShellNoise(out)
	observation := ""
	if len(lines) > 0 {
		observation = lines[len(lines)-1]
	}
	if observation == "CLEAR" {
		return nil
	}
	message := "The remote Icebath update marker is unreadable or malformed; refusing SSH startup."
	if live := liveVerdictRE.FindStringSubmatch(observation); live != nil {
		message = "Remote Icebath update process " + live[1] + " is still running; SSH startup is paused."
	} else if observation == "HELD" {
		message = "A process the remote Icebath update started still holds the install; SSH startup is paused."
	}
	return lifecycleError("update-in-progress", message, nil)
}

// Error text of a PowerShell CLIXML error stream, its escapes and entities decoded
func powerShellErrorDetail(raw string) string {
	var parts []string
	for _, m := range psErrorTag.FindAllStringSubmatch(raw, -1) {
		parts = append(parts, m[1])
	}
	detail := raw
	if len(parts) > 0 {
		detail = strings.Join(parts, " ")
	}
	detail = psHexEscape.ReplaceAllStringFunc(detail, func(m string) string {
		n, _ := strconv.ParseUint(m[2:6], 16, 16)
		return string(utf16.Decode([]uint16{uint16(n)}))
	})
	for _, r := range [][2]string{{"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&apos;", "'"}, {"&amp;", "&"}} {
		detail = strings.ReplaceAll(detail, r[0], r[1])
	}
	return detail
}

// Remote platform from uname, or the Windows install the PowerShell probe locates, transport failures surfaced as such
func DetectRemotePlatform(ssh RemoteShell, explicitHermesPath string) (RemotePlatform, error) {
	out, err := ssh.Exec("uname -s; uname -m", SSHExecOptions{})
	if err == nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if lines[0] == "Linux" || lines[0] == "Darwin" {
			arch := ""
			if len(lines) > 1 {
				arch = lines[1]
			}
			return RemotePlatform{OS: lines[0], Arch: arch}, nil
		}
	} else if isTransportKind(errKind(err)) {
		return RemotePlatform{}, err
	}
	runtime, err := probeWindowsRemote(ssh, explicitHermesPath)
	if err == nil {
		return RemotePlatform{OS: runtime.OS, Arch: runtime.Arch, Windows: runtime}, nil
	}
	kind := errKind(err)
	if isTransportKind(kind) {
		return RemotePlatform{}, err
	}
	detail := strings.TrimFunc(sshControlCharRE.ReplaceAllString(RedactSecrets(powerShellErrorDetail(err.Error())), " "), jsSpace)
	message := "The remote operating system is not supported by Desktop SSH."
	if kind != "" {
		message = "The Windows remote probe failed."
	}
	if detail != "" {
		message += " (probe: " + utf16Prefix(detail, 300) + ")"
	}
	if kind == "" {
		kind = "unsupported-platform"
	}
	return RemotePlatform{}, lifecycleError(kind, message, err)
}

func helperCommand(runtime *WindowsRuntime, operation string, args []string) string {
	argv := windowsHelperArgv(runtime, operation, args)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = PsLiteral(a)
	}
	return PowerShellCommand(strings.Join([]string{
		`$ProgressPreference="SilentlyContinue"`,
		`$ErrorActionPreference="Stop"`,
		"& " + strings.Join(quoted, " "),
		`if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}`,
	}, ";"))
}

// Runs one runtime helper operation and answers its parsed JSON, an error field raised as the error
func WindowsHelper(ssh RemoteShell, runtime *WindowsRuntime, operation string, args []string, stdinData *string) (any, error) {
	out, err := ssh.Exec(helperCommand(runtime, operation, args), SSHExecOptions{StdinData: stdinData})
	if err != nil {
		return nil, err
	}
	parsed, err := parseLastJSONLine(StripPowerShellNoise(out))
	if err != nil {
		return nil, err
	}
	if o := ordered(parsed); o != nil && jsTruthy(o.Get("error")) {
		return nil, errors.New(jsString(o.Get("error")))
	}
	return parsed, nil
}

func psArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = PsLiteral(a)
	}
	return strings.Join(quoted, " ")
}

// WindowsSpawnReservation is the ownership record an atomic Windows spawn publishes
type WindowsSpawnReservation struct {
	OwnershipID      string
	SpawnNonce       string
	Profile          string
	HermesPath       string
	HermesHome       string
	TokenFingerprint string
	DevServer        string
	StartedAt        string
}

// One PowerShell command holding the marker lock across the spawn and the lock publication
func atomicWindowsSpawnCommand(runtime *WindowsRuntime, r WindowsSpawnReservation, payload *string) string {
	spawn := psArgv(windowsHelperArgv(runtime, "spawn", nil))
	helper := func(op string) string { return psArgv(windowsHelperArgv(runtime, op, nil)) }
	invokeSpawn := "& " + spawn
	if payload != nil {
		invokeSpawn = PsLiteral(*payload) + " | & " + spawn
	}
	lines := []string{
		`$ProgressPreference="SilentlyContinue"`,
		`$ErrorActionPreference="Stop"`,
		`$OutputEncoding=[Text.UTF8Encoding]::new($false)`,
		WindowsMarkerJudgePs,
		WindowsCheckoutLockPs,
		"$hermesHome=" + PsLiteral(runtime.HermesHome),
		`$installRoot=$hermesHome`,
		`$parent=Split-Path -Parent $hermesHome`,
		`if((Split-Path -Leaf $parent) -ieq "profiles"){$installRoot=Split-Path -Parent $parent}`,
		`$marker=Join-Path $installRoot ".icebath-update-in-progress"`,
		windowsCheckoutRootsScript(runtime.Executable),
		`$clock=[Diagnostics.Stopwatch]::StartNew();$mutex=$null`,
		`while(-not $mutex){try{$mutex=[IO.File]::Open($marker+".lock",[IO.FileMode]::OpenOrCreate,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)}catch{if($clock.ElapsedMilliseconds -ge 10000){throw "remote update marker lock is busy"};Start-Sleep -Milliseconds 25}}`,
		`try{`,
		`  if([IO.File]::Exists($marker)){$verdict=Get-MarkerVerdict ([IO.File]::ReadAllText($marker));if($verdict -eq "CLEAR" -and (Test-CheckoutLockHeld $checkoutRoots)){$verdict="HELD"};if($verdict -ne "CLEAR"){throw "remote update marker is $verdict"};[IO.File]::Delete($marker)}`,
	}
	if r.OwnershipID != "" {
		lines = append(lines,
			"  $existingLines=@(& "+helper("read-lock")+" "+PsLiteral(r.OwnershipID)+"); $existingExit=$LASTEXITCODE; "+
				"  if($existingExit -eq 0 -and $existingLines.Count -gt 0){try{$existing=$existingLines[-1]|ConvertFrom-Json}catch{$existing=$null}; "+
				"if($existing -and [int]$existing.pid -gt 0){try{$p=[Diagnostics.Process]::GetProcessById([int]$existing.pid); "+
				"if(-not $p.HasExited){[ordered]@{existing=$true}|ConvertTo-Json -Compress;exit 0}}catch{}finally{if($p){$p.Dispose()}}}; "+
				"& "+helper("remove-lock")+" "+PsLiteral(r.OwnershipID)+"|Out-Null}",
			"  $spawnLines=@("+invokeSpawn+"); $spawnExit=$LASTEXITCODE",
			"  if($spawnExit -ne 0){exit $spawnExit}",
			"  $spawned=$spawnLines[-1]|ConvertFrom-Json; $lock=[ordered]@{schemaVersion=2;protocolVersion=1;ownershipId="+PsLiteral(r.OwnershipID)+
				";spawnNonce="+PsLiteral(r.SpawnNonce)+";pid=[int]$spawned.pid;creationTimeNs=[string]$spawned.creationTimeNs;port=0;profile="+PsLiteral(r.Profile)+
				";hermesPath="+PsLiteral(r.HermesPath)+";hermesHome="+PsLiteral(r.HermesHome)+";tokenFingerprint="+PsLiteral(r.TokenFingerprint)+
				";devServer="+PsLiteral(r.DevServer)+";startedAt="+PsLiteral(r.StartedAt)+"}|ConvertTo-Json -Compress; "+
				"  $lock | & "+helper("write-lock")+" "+PsLiteral(r.OwnershipID)+" | Out-Null; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}; $spawnLines|Write-Output",
		)
	} else {
		lines = append(lines, "  "+invokeSpawn, `  if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}`)
	}
	lines = append(lines,
		`  if([IO.File]::Exists($marker)){throw "remote update marker claimed during backend spawn"}`,
		`}finally{$mutex.Dispose()}`,
	)
	return CompressedPowerShellCommand(strings.Join(lines, ";"))
}

// Runs the atomic spawn, an error field raised with its kind
func atomicWindowsSpawn(ssh RemoteShell, runtime *WindowsRuntime, payload string, r WindowsSpawnReservation) (*jsjson.OrderedObject, error) {
	out, err := ssh.Exec(atomicWindowsSpawnCommand(runtime, r, &payload), SSHExecOptions{})
	if err != nil {
		return nil, err
	}
	parsed, err := parseLastJSONLine(StripPowerShellNoise(out))
	if err != nil {
		return nil, err
	}
	o := ordered(parsed)
	if o != nil && jsTruthy(o.Get("error")) {
		kind := jsOrString(o.Get("kind"))
		if kind == "" {
			kind = "remote-helper-error"
		}
		return nil, lifecycleError(kind, jsString(o.Get("error")), nil)
	}
	return o, nil
}

// Whether a helper read lock is a full ownership record of this id, a port zero record included
func validWindowsLock(lock *jsjson.OrderedObject, ownershipID string) bool {
	if lock == nil {
		return false
	}
	pid, pidOK := jsInteger(lock.Get("pid"))
	port, portOK := jsInteger(lock.Get("port"))
	_, pathOK := lock.Get("hermesPath").(string)
	_, homeOK := lock.Get("hermesHome").(string)
	return lock.Get("schemaVersion") == float64(LockfileSchemaVersion) && lock.Get("protocolVersion") == float64(RemoteProtocolVersion) &&
		lock.Get("ownershipId") == ownershipID && spawnNonceRE.MatchString(jsOrString(lock.Get("spawnNonce"))) &&
		pidOK && pid > 0 && creationNs.MatchString(jsOrString(lock.Get("creationTimeNs"))) &&
		portOK && port >= 0 && port <= 65535 && ownershipIDRE.MatchString(jsOrString(lock.Get("tokenFingerprint"))) && pathOK && homeOK
}

func windowsLockPID(lock *jsjson.OrderedObject) string { return jsString(lock.Get("pid")) }

func processState(ssh RemoteShell, runtime *WindowsRuntime, lock *jsjson.OrderedObject) (*jsjson.OrderedObject, error) {
	state, err := WindowsHelper(ssh, runtime, "process-state", []string{windowsLockPID(lock), jsString(lock.Get("creationTimeNs")), jsString(lock.Get("hermesPath")), jsString(lock.Get("spawnNonce"))}, nil)
	if err != nil {
		return nil, err
	}
	return helperObject(state, "alive")
}

// Object view of a helper answer, a JSON null raising the TypeError reading key raises and other scalars reading as empty
func helperObject(v any, key string) (*jsjson.OrderedObject, error) {
	if v == nil {
		return nil, fmt.Errorf("Cannot read properties of null (reading '%s')", key)
	}
	if o := ordered(v); o != nil {
		return o, nil
	}
	return jsjson.NewOrderedObject(), nil
}

// Terminates an owned live serve and drops its token, log and lock, a failed terminate keeping the lock
func cleanupOwned(ssh RemoteShell, runtime *WindowsRuntime, ownershipID string, lock *jsjson.OrderedObject) error {
	if lock != nil {
		state, err := processState(ssh, runtime, lock)
		if err != nil {
			return err
		}
		if jsTruthy(state.Get("alive")) && jsTruthy(state.Get("owned")) {
			if _, err := WindowsHelper(ssh, runtime, "terminate", []string{windowsLockPID(lock), jsString(lock.Get("creationTimeNs")), jsString(lock.Get("hermesPath")), jsString(lock.Get("spawnNonce"))}, nil); err != nil {
				return err
			}
		}
		if nonce := jsOrString(lock.Get("spawnNonce")); nonce != "" {
			_, _ = WindowsHelper(ssh, runtime, "remove-token", []string{ownershipID, jsString(lock.Get("spawnNonce"))}, nil)
			_, _ = WindowsHelper(ssh, runtime, "remove-log", []string{ownershipID, jsString(lock.Get("spawnNonce"))}, nil)
		}
	}
	_, _ = WindowsHelper(ssh, runtime, "remove-lock", []string{ownershipID}, nil)
	return nil
}

func windowsLockMatchesManagedUpdateScope(lock *jsjson.OrderedObject, e ManagedUpdateExpected) bool {
	field := func(key, want string) bool {
		s, ok := lock.Get(key).(string)
		return ok && s == want
	}
	pid, ok := lock.Get("pid").(float64)
	return field("ownershipId", e.OwnershipID) && ok && pid == float64(e.PID) && field("spawnNonce", e.SpawnNonce) &&
		field("creationTimeNs", e.CreationTimeNs) && field("profile", e.Profile) && field("hermesPath", e.HermesPath) && field("hermesHome", e.HermesHome)
}

// Terminates a Windows serve only while its record and creation time proof match the drained scope
func TerminateOwnedWindowsDashboardForUpdate(ssh RemoteShell, runtime *WindowsRuntime, expected ManagedUpdateExpected) (TerminateResult, error) {
	read := func() (*jsjson.OrderedObject, error) {
		v, err := WindowsHelper(ssh, runtime, "read-lock", []string{expected.OwnershipID}, nil)
		return ordered(v), err
	}
	lock, err := read()
	if err != nil {
		return TerminateResult{}, err
	}
	if !validWindowsLock(lock, expected.OwnershipID) || !windowsLockMatchesManagedUpdateScope(lock, expected) {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote Windows ownership record changed before the managed update.", nil)
	}
	state, err := processState(ssh, runtime, lock)
	if err != nil {
		return TerminateResult{}, err
	}
	pid := int(lock.Get("pid").(float64))
	if jsTruthy(state.Get("indeterminate")) {
		return TerminateResult{}, lifecycleError("transient-transport-error", "Could not prove the remote Windows process identity for the managed update.", nil)
	}
	if !jsTruthy(state.Get("alive")) {
		return TerminateResult{PID: pid, AlreadyStopped: true}, nil
	}
	if !jsTruthy(state.Get("owned")) {
		return TerminateResult{}, lifecycleError("foreign-backend", "Refusing to terminate a remote Windows process whose ownership is unproven.", nil)
	}
	if lock, err = read(); err != nil {
		return TerminateResult{}, err
	}
	if !validWindowsLock(lock, expected.OwnershipID) || !windowsLockMatchesManagedUpdateScope(lock, expected) {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote Windows ownership record changed during process verification.", nil)
	}
	if state, err = processState(ssh, runtime, lock); err != nil {
		return TerminateResult{}, err
	}
	if jsTruthy(state.Get("indeterminate")) || !jsTruthy(state.Get("alive")) || !jsTruthy(state.Get("owned")) {
		kind := "ownership-changed"
		if jsTruthy(state.Get("indeterminate")) {
			kind = "transient-transport-error"
		}
		return TerminateResult{}, lifecycleError(kind, "The remote Windows process identity changed during managed update drain.", nil)
	}
	if _, err := WindowsHelper(ssh, runtime, "terminate", []string{windowsLockPID(lock), jsString(lock.Get("creationTimeNs")), jsString(lock.Get("hermesPath")), jsString(lock.Get("spawnNonce"))}, nil); err != nil {
		return TerminateResult{}, err
	}
	return TerminateResult{PID: int(lock.Get("pid").(float64)), Terminated: true}, nil
}

// Polls the spawn log for the last ready line while the owned process lives
func waitWindowsReady(ssh RemoteShell, runtime *WindowsRuntime, ownershipID string, lock *jsjson.OrderedObject, timeout time.Duration, signal context.Context) (int, error) {
	deadline := time.Now().Add(timeout)
	nonce := jsString(lock.Get("spawnNonce"))
	readLog := func() string {
		v, err := WindowsHelper(ssh, runtime, "read-log", []string{ownershipID, nonce}, nil)
		if err != nil {
			return ""
		}
		return ostr(ordered(v), "content")
	}
	for time.Now().Before(deadline) {
		if err := AssertBootstrapNotSuperseded(signal); err != nil {
			return 0, err
		}
		state, err := processState(ssh, runtime, lock)
		if err != nil {
			time.Sleep(remoteReadyPoll)
			continue
		}
		if !jsTruthy(state.Get("indeterminate")) && (!jsTruthy(state.Get("alive")) || !jsTruthy(state.Get("owned"))) {
			detail := readLog()
			units := utf16.Encode([]rune(detail))
			if len(units) > 2000 {
				detail = string(utf16.Decode(units[len(units)-2000:]))
			}
			return 0, lifecycleError("spawn-failed", "Remote Windows backend exited before announcing its port. state="+jsjson.Stringify(state, "", "")+" "+detail, nil)
		}
		port := 0
		for _, m := range readyLineRE.FindAllStringSubmatch(readLog(), -1) {
			port, _ = strconv.Atoi(m[1])
		}
		if port != 0 {
			return port, nil
		}
		time.Sleep(remoteReadyPoll)
	}
	return 0, lifecycleError("ready-timeout", fmt.Sprintf("Timed out waiting for the remote Windows backend (%dms).", timeout.Milliseconds()), nil)
}

func waitForWindowsSpawnCompletion(ssh RemoteShell, runtime *WindowsRuntime, ownershipID string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		v, err := WindowsHelper(ssh, runtime, "read-lock", []string{ownershipID}, nil)
		if err != nil {
			return false, err
		}
		if !jsTruthy(v) {
			return false, nil
		}
		lock := ordered(v)
		if validWindowsLock(lock, ownershipID) && lock.Get("port").(float64) > 0 {
			return true, nil
		}
		time.Sleep(remoteReadyPoll)
	}
	return false, lifecycleError("spawn-failed", "Timed out waiting for the concurrent Windows SSH connection to publish its backend.", nil)
}

// A launcher runtime is recognised as ours only by a helper advertising boot runtime ownership
func windowsHelperOwnsRuntime(runtime *WindowsRuntime, inspection *jsjson.OrderedObject) bool {
	return runtime.RuntimeCommand == nil || (inspection != nil && inspection.Get("runtimeBootstrapOwnership") == float64(1))
}

// WindowsSupport is the remote version and whether Connect would accept it
type WindowsSupport struct {
	Version   any
	Supported bool
}

// Version and Connect capability of the remote Windows install
func InspectWindowsSupport(ssh RemoteShell, runtime *WindowsRuntime) (WindowsSupport, error) {
	v, err := WindowsHelper(ssh, runtime, "inspect", []string{runtime.HermesPath}, nil)
	if err != nil {
		return WindowsSupport{}, err
	}
	inspection, err := helperObject(v, "version")
	if err != nil {
		return WindowsSupport{}, err
	}
	return WindowsSupport{Version: inspection.Get("version"), Supported: jsTruthy(inspection.Get("supported")) && windowsHelperOwnsRuntime(runtime, inspection)}, nil
}

// Establishes or reuses the Windows remote dashboard of an ownership id and a tunnel to it
func ConnectWindowsRemote(d RemoteConnectDeps) (RemoteConnectResult, error) {
	ssh, ownershipID, reuseToken := d.SSH, d.OwnershipID, d.ReuseToken
	log := func(msg string) {
		if d.RememberLog != nil {
			d.RememberLog(msg)
		}
	}
	readyTimeout := ResolveReadyTimeout(os.Getenv)
	if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
		return RemoteConnectResult{}, err
	}
	runtime, err := probeWindowsRemote(ssh, d.RemoteHermesPath)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	gate := func() error {
		return AssertWindowsRemoteInstallUpdateClear(ssh, runtime.HermesHome, runtime.Executable)
	}
	if err := gate(); err != nil {
		return RemoteConnectResult{}, err
	}
	v, err := WindowsHelper(ssh, runtime, "inspect", []string{runtime.HermesPath}, nil)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	inspection := ordered(v)
	if !windowsHelperOwnsRuntime(runtime, inspection) {
		return RemoteConnectResult{}, lifecycleError("update-required", "The remote Windows SSH helper must be updated with boot-selecting runtime ownership support before connecting.", nil)
	}
	if inspection == nil && v == nil {
		return RemoteConnectResult{}, errors.New("Cannot read properties of null (reading 'supported')")
	}
	if inspection == nil || !jsTruthy(inspection.Get("supported")) {
		return RemoteConnectResult{}, lifecycleError("update-required", "Update Icebath on the remote Windows host before connecting with Desktop SSH.", nil)
	}
	runtime.HermesPath = jsNullishString(inspection.Get("path"))
	hermesVersion := ostr(inspection, "version")
	log("[ssh-lifecycle] remote platform Windows/" + runtime.Arch)
	log("[ssh-lifecycle] located icebath at " + runtime.HermesPath)
	if err := gate(); err != nil {
		return RemoteConnectResult{}, err
	}
	lv, err := WindowsHelper(ssh, runtime, "read-lock", []string{ownershipID}, nil)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	lock := ordered(lv)
	platform := RemotePlatform{OS: "Windows", Arch: runtime.Arch, Windows: runtime}
	if validWindowsLock(lock, ownershipID) {
		state, err := processState(ssh, runtime, lock)
		if err != nil {
			return RemoteConnectResult{}, err
		}
		if jsTruthy(state.Get("indeterminate")) {
			return RemoteConnectResult{}, lifecycleError("transient-transport-error", "Could not determine the state of the existing remote backend.", nil)
		}
		reusable := jsTruthy(state.Get("alive")) && jsTruthy(state.Get("owned")) &&
			LockServesConnect(&RemoteLock{Object: lock}, d.Profile, reuseToken, runtime.HermesPath, runtime.HermesHome, d.DevServer)
		if reusable {
			if err := gate(); err != nil {
				return RemoteConnectResult{}, err
			}
			localPort, err := d.PickLocalPort()
			if err != nil {
				return RemoteConnectResult{}, err
			}
			remotePort := int(lock.Get("port").(float64))
			if err := d.Forward(localPort, remotePort); err != nil {
				return RemoteConnectResult{}, err
			}
			result, reused, err := func() (RemoteConnectResult, bool, error) {
				baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
				classification, err := d.ProbeReuseProof(baseURL, reuseToken, jsString(lock.Get("spawnNonce")))
				if err != nil {
					return RemoteConnectResult{}, false, err
				}
				if classification == "authenticated-ok" {
					return RemoteConnectResult{
						BaseURL: baseURL, Token: reuseToken, RemotePort: remotePort, LocalPort: localPort, PID: int(lock.Get("pid").(float64)),
						Reused: true, Platform: platform, HermesPath: runtime.HermesPath, HermesVersion: hermesVersion, OwnershipID: ownershipID,
						SpawnNonce: jsString(lock.Get("spawnNonce")), CreationTimeNs: jsString(lock.Get("creationTimeNs")), HermesHome: runtime.HermesHome,
						ExecutablePath: runtime.Executable,
					}, true, nil
				}
				if classification != "authenticated-stale" {
					return RemoteConnectResult{}, false, errors.New("Invalid SSH reuse classification.")
				}
				if err := d.CancelForward(localPort, remotePort); err != nil {
					return RemoteConnectResult{}, false, err
				}
				if err := gate(); err != nil {
					return RemoteConnectResult{}, false, err
				}
				return RemoteConnectResult{}, false, cleanupOwned(ssh, runtime, ownershipID, lock)
			}()
			if err != nil {
				if cancelErr := d.CancelForward(localPort, remotePort); cancelErr != nil {
					return RemoteConnectResult{}, cancelErr
				}
				return RemoteConnectResult{}, err
			}
			if reused {
				return result, nil
			}
		} else {
			if err := gate(); err != nil {
				return RemoteConnectResult{}, err
			}
			if err := cleanupOwned(ssh, runtime, ownershipID, lock); err != nil {
				return RemoteConnectResult{}, err
			}
		}
	} else if jsTruthy(lv) {
		if err := gate(); err != nil {
			return RemoteConnectResult{}, err
		}
		if _, err := WindowsHelper(ssh, runtime, "remove-lock", []string{ownershipID}, nil); err != nil {
			return RemoteConnectResult{}, err
		}
	}
	if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
		return RemoteConnectResult{}, err
	}
	if err := gate(); err != nil {
		return RemoteConnectResult{}, err
	}
	token := randomHex(32)
	spawnNonce := randomHex(8)
	if _, err := WindowsHelper(ssh, runtime, "upload-token", []string{ownershipID, spawnNonce}, &token); err != nil {
		return RemoteConnectResult{}, err
	}
	startedAt := isoNow()
	tokenFingerprint := FingerprintToken(token)
	removeToken := func() error {
		_, err := WindowsHelper(ssh, runtime, "remove-token", []string{ownershipID, spawnNonce}, nil)
		return err
	}
	spawned, err := func() (*jsjson.OrderedObject, error) {
		if err := gate(); err != nil {
			return nil, err
		}
		payload := jsjson.Stringify(jsjson.NewOrderedObject("ownershipId", ownershipID, "spawnNonce", spawnNonce, "profile", d.Profile, "hermesPath", runtime.HermesPath, "devServer", d.DevServer), "", "")
		return atomicWindowsSpawn(ssh, runtime, payload, WindowsSpawnReservation{
			OwnershipID: ownershipID, SpawnNonce: spawnNonce, Profile: d.Profile, HermesPath: runtime.HermesPath, HermesHome: runtime.HermesHome,
			TokenFingerprint: tokenFingerprint, DevServer: d.DevServer, StartedAt: startedAt,
		})
	}()
	if err != nil {
		if removeErr := removeToken(); removeErr != nil {
			return RemoteConnectResult{}, removeErr
		}
		return RemoteConnectResult{}, err
	}
	if spawned != nil && jsTruthy(spawned.Get("existing")) {
		if err := removeToken(); err != nil {
			return RemoteConnectResult{}, err
		}
		if reuseToken == "" {
			return RemoteConnectResult{}, lifecycleError("remote-ownership-contended", "Another SSH connection owns this remote dashboard; a session token is required to reuse it.", nil)
		}
		if _, err := waitForWindowsSpawnCompletion(ssh, runtime, ownershipID, readyTimeout); err != nil {
			return RemoteConnectResult{}, err
		}
		return ConnectWindowsRemote(d)
	}
	if spawned == nil {
		spawned = jsjson.NewOrderedObject()
	}
	owned := jsjson.NewOrderedObject("schemaVersion", float64(LockfileSchemaVersion), "protocolVersion", float64(RemoteProtocolVersion),
		"ownershipId", ownershipID, "spawnNonce", spawnNonce)
	owned.SetRaw("pid", spawned.Get("pid"))
	owned.SetRaw("creationTimeNs", spawned.Get("creationTimeNs"))
	for _, kv := range [][2]any{{"port", 0.0}, {"profile", d.Profile}, {"hermesPath", runtime.HermesPath}, {"hermesHome", runtime.HermesHome},
		{"tokenFingerprint", tokenFingerprint}, {"devServer", d.DevServer}, {"startedAt", startedAt}} {
		owned.Set(kv[0].(string), kv[1])
	}
	localPort, remotePort := 0, 0
	result, err := func() (RemoteConnectResult, error) {
		record := jsjson.Stringify(owned, "", "")
		if _, err := WindowsHelper(ssh, runtime, "write-lock", []string{ownershipID}, &record); err != nil {
			return RemoteConnectResult{}, err
		}
		port, err := waitWindowsReady(ssh, runtime, ownershipID, owned, readyTimeout, d.Signal)
		if err != nil {
			return RemoteConnectResult{}, err
		}
		remotePort = port
		if localPort, err = d.PickLocalPort(); err != nil {
			return RemoteConnectResult{}, err
		}
		if err := d.Forward(localPort, remotePort); err != nil {
			return RemoteConnectResult{}, err
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
		if err := d.WaitForHermes(baseURL, token); err != nil {
			return RemoteConnectResult{}, err
		}
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return RemoteConnectResult{}, err
		}
		published := owned.Clone()
		published.Set("port", float64(remotePort))
		text := jsjson.Stringify(published, "", "")
		if _, err := WindowsHelper(ssh, runtime, "write-lock", []string{ownershipID}, &text); err != nil {
			return RemoteConnectResult{}, err
		}
		pid, _ := spawned.Get("pid").(float64)
		return RemoteConnectResult{
			BaseURL: baseURL, Token: token, RemotePort: remotePort, LocalPort: localPort, PID: int(pid), Platform: platform,
			HermesPath: runtime.HermesPath, HermesVersion: hermesVersion, OwnershipID: ownershipID, SpawnNonce: spawnNonce,
			CreationTimeNs: jsNullishString(spawned.Get("creationTimeNs")), HermesHome: runtime.HermesHome, ExecutablePath: runtime.Executable,
		}, nil
	}()
	if err == nil {
		return result, nil
	}
	if localPort != 0 && remotePort != 0 {
		if cancelErr := d.CancelForward(localPort, remotePort); cancelErr != nil {
			return RemoteConnectResult{}, cancelErr
		}
	}
	if cleanupErr := cleanupOwned(ssh, runtime, ownershipID, owned); cleanupErr != nil {
		return RemoteConnectResult{}, cleanupErr
	}
	return RemoteConnectResult{}, err
}

// Interactive PowerShell over SSH for the remote terminal, starting in remoteCwd when it exists
func BuildWindowsInteractiveCommand(remoteCwd string) string {
	cwd := strings.TrimSpace(remoteCwd)
	script := []string{`$ErrorActionPreference="Stop"`}
	if cwd != "" {
		script = append(script, "if(Test-Path -LiteralPath "+PsLiteral(cwd)+" -PathType Container){Set-Location -LiteralPath "+PsLiteral(cwd)+"}")
	}
	script = append(script, `$host.UI.RawUI.WindowTitle="Icebath SSH"`, "powershell.exe -NoLogo")
	return PowerShellCommand(strings.Join(script, ";"))
}

// ListWindowsRemoteProfiles reads the profiles the Windows runtime inventory reports, default required
func ListWindowsRemoteProfiles(ssh RemoteShell, runtime *WindowsRuntime) ([]string, error) {
	result, err := readWindowsInventory(ssh, runtime)
	if err != nil {
		return nil, err
	}
	invalid := errors.New("Windows inventory did not return canonical profile names.")
	profiles, ok := result.Get("profiles").([]any)
	if !ok {
		return nil, invalid
	}
	names := make([]string, 0, len(profiles))
	hasDefault := false
	for _, p := range profiles {
		s, ok := p.(string)
		if !ok {
			return nil, invalid
		}
		hasDefault = hasDefault || s == "default"
		names = append(names, s)
	}
	if !hasDefault {
		return nil, invalid
	}
	return ParseRemoteProfileListing(strings.Join(names, "\n")), nil
}

// ReadWindowsRemoteInstallID is the install identity the Windows runtime inventory reports, empty when not an identity
func ReadWindowsRemoteInstallID(ssh RemoteShell, runtime *WindowsRuntime) (string, error) {
	result, err := readWindowsInventory(ssh, runtime)
	if err != nil {
		return "", err
	}
	if !result.Has("installId") {
		return "", errors.New("Windows inventory did not report its install identity.")
	}
	id := ""
	if s, ok := result.Get("installId").(string); ok {
		id = strings.ToLower(strings.TrimFunc(s, jsSpace))
	}
	if ownershipIDRE.MatchString(id) {
		return id, nil
	}
	return "", nil
}

func readWindowsInventory(ssh RemoteShell, runtime *WindowsRuntime) (*jsjson.OrderedObject, error) {
	if runtime == nil {
		return nil, errors.New("Windows inventory did not return valid JSON.")
	}
	if err := AssertWindowsRemoteInstallUpdateClear(ssh, runtime.HermesHome, runtime.Executable); err != nil {
		return nil, err
	}
	parsed, err := WindowsHelper(ssh, runtime, "inventory", nil, nil)
	if err != nil {
		return nil, err
	}
	if _, isArray := parsed.([]any); isArray {
		return jsjson.NewOrderedObject(), nil
	}
	o := ordered(parsed)
	if o == nil {
		return nil, errors.New("Windows inventory did not return valid JSON.")
	}
	return o, nil
}
