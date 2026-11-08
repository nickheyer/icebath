package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const launchdLabelBase = "ai.icebath.gateway"

var (
	scanPPID            = proc.PPID
	scanProcDir         = "/proc"
	scanGetpid          = os.Getpid
	scanGeteuid         = os.Geteuid
	listInstallProfiles = installProfileNames
	servicePIDsHook     = getServicePIDs
	homeForPIDHook      = HermesHomeForPID
)

// Python int of trimmed text when it parses
func pyIntText(s string) (int, bool) {
	n, ok := pyint.Parse(pystr.Strip(s))
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

// Value when it parses as a positive int
func positivePID(value string) (int, bool) {
	if value == "" {
		value = "0"
	}
	n, ok := pyint.Parse(value)
	if !ok || !n.IsInt64() || n.Int64() <= 0 {
		return 0, false
	}
	return int(n.Int64()), true
}

// Launchd label of the profile in effect
func GetLaunchdLabel(ctx context.Context) (string, error) {
	suffix, err := ProfileSuffix(ctx)
	if err != nil {
		return "", err
	}
	if suffix != "" {
		return launchdLabelBase + "-" + suffix, nil
	}
	return launchdLabelBase, nil
}

type installProfile struct {
	Name      string
	IsDefault bool
}

// Profiles of this install in list_profiles order, raising the errors its config and alias reads raise
func installProfileNames(ctx context.Context) ([]installProfile, error) {
	var out []installProfile
	def := profiles.DefaultHome()
	if st, err := os.Stat(def); err == nil && st.IsDir() {
		if _, _, err := profiles.ReadConfigModel(ctx, def); err != nil {
			return nil, err
		}
		out = append(out, installProfile{"default", true})
	}
	named, err := profiles.IterNamedProfileDirs(true)
	if err != nil || len(named) == 0 {
		return out, err
	}
	aliases, err := profiles.BuildAliasMap()
	if err != nil {
		return nil, err
	}
	for _, entry := range named {
		canon, err := profiles.NormalizeProfileName(filepath.Base(entry))
		if err != nil {
			return nil, err
		}
		if _, _, err := profiles.ReadConfigModel(ctx, entry); err != nil {
			return nil, err
		}
		if alias := aliases[canon]; alias != "" {
			if _, err := profiles.FindOurWrapper(alias); err != nil {
				return nil, err
			}
		}
		out = append(out, installProfile{filepath.Base(entry), false})
	}
	return out, nil
}

// Launchd labels of every profile of this install, root first then named profiles sorted
func LaunchdGatewayLabelsForInstall(ctx context.Context) ([]string, error) {
	list, err := listInstallProfiles(ctx)
	if err != nil {
		return nil, err
	}
	var root, named []string
	for _, p := range list {
		if p.IsDefault {
			root = append(root, launchdLabelBase)
		} else if serviceProfileNameRE.MatchString(p.Name) {
			named = append(named, launchdLabelBase+"-"+p.Name)
		}
	}
	sort.Strings(named)
	return append(root, named...), nil
}

// Pid of a launchctl list record, false when absent or not positive
func ParseLaunchdPIDFromListOutput(output string) (int, bool) {
	for _, line := range pystr.Splitlines(output, false) {
		s := pystr.Strip(line)
		if (strings.HasPrefix(s, `"PID"`) || strings.HasPrefix(s, "PID")) && strings.Contains(s, "=") {
			v := pystr.Strip(strings.SplitN(s, "=", 2)[1])
			return positivePID(strings.Trim(strings.TrimRight(v, ";"), `"`))
		}
	}
	return 0, false
}

// Live pid of a launchctl print record, the first pid line wins
func ParseLaunchdPIDFromPrintOutput(output string) (int, bool) {
	for _, line := range pystr.Splitlines(output, false) {
		s := pystr.Strip(line)
		if strings.HasPrefix(s, "pid = ") {
			return positivePID(pystr.Strip(s[len("pid = "):]))
		}
	}
	return 0, false
}

// Loaded flag and pid of domain/label, a timeout propagates
func LaunchdPrintServicePID(ctx context.Context, domain, label string) (bool, int, bool, error) {
	res, err := RunServiceCommand(ctx, 5*time.Second, "launchctl", "print", domain+"/"+label)
	if errors.Is(err, ErrCommandNotFound) {
		return false, 0, false, nil
	}
	if err != nil {
		return false, 0, false, err
	}
	if res.ReturnCode != 0 {
		return false, 0, false, nil
	}
	pid, ok := ParseLaunchdPIDFromPrintOutput(res.Stdout)
	return true, pid, ok, nil
}

// True when launchd knows the label, command failures propagate
func LaunchdServiceRegistered(ctx context.Context, label string, timeout time.Duration) (bool, error) {
	res, err := RunServiceCommand(ctx, timeout, "launchctl", "list", label)
	if err != nil {
		return false, err
	}
	return res.ReturnCode == 0, nil
}

// Domain and pid of a label probing the gui then the user domain, empty domain when unloaded
func LocateLaunchdGatewayService(ctx context.Context, label string) (string, int, bool, error) {
	uid := serviceGetuid()
	for _, domain := range []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)} {
		loaded, pid, ok, err := LaunchdPrintServicePID(ctx, domain, label)
		if err != nil {
			return "", 0, false, err
		}
		if loaded {
			return domain, pid, ok, nil
		}
	}
	return "", 0, false, nil
}

// Pids of the systemd and launchd gateway services, the whole fleet with allProfiles
func getServicePIDs(ctx context.Context, allProfiles bool) ([]int, error) {
	pids := map[int]bool{}
	if SupportsSystemdServices(ctx) {
		pattern := serviceBase + "*"
		if !allProfiles {
			name, err := GetServiceName(ctx)
			if err != nil {
				return nil, err
			}
			pattern = name
		}
		for _, scope := range [][]string{{"systemctl", "--user"}, {"systemctl"}} {
			if err := collectSystemdScopePIDs(ctx, scope, pattern, pids); err != nil {
				return nil, err
			}
		}
	}
	if IsMacOS() {
		if err := collectLaunchdPIDs(ctx, allProfiles, pids); err != nil {
			return nil, err
		}
	}
	out := make([]int, 0, len(pids))
	for pid := range pids {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out, nil
}

// MainPID of every unit of one systemd scope matching pattern, a missing binary or timeout skips the scope
func collectSystemdScopePIDs(ctx context.Context, scope []string, pattern string, pids map[int]bool) error {
	skip := func(err error) bool { return errors.Is(err, ErrCommandNotFound) || errors.Is(err, ErrCommandTimeout) }
	argv := append(append([]string{}, scope...), "list-units", pattern, "--plain", "--no-legend", "--no-pager")
	res, err := RunServiceCommand(ctx, 5*time.Second, argv...)
	if err != nil {
		if skip(err) {
			return nil
		}
		return err
	}
	for _, line := range pystr.Splitlines(pystr.Strip(res.Stdout), false) {
		parts := pystr.SplitWhitespace(line, -1)
		if len(parts) == 0 || !strings.HasSuffix(parts[0], ".service") {
			continue
		}
		show := append(append([]string{}, scope...), "show", parts[0], "--property=MainPID", "--value")
		out, err := RunServiceCommand(ctx, 5*time.Second, show...)
		if errors.Is(err, ErrCommandTimeout) {
			continue
		}
		if errors.Is(err, ErrCommandNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if pid, ok := pyIntText(out.Stdout); ok && pid > 0 {
			pids[pid] = true
		}
	}
	return nil
}

// Launchd service pids of this profile, the whole fleet and every ai.icebath.gateway agent with allProfiles
func collectLaunchdPIDs(ctx context.Context, allProfiles bool, pids map[int]bool) error {
	label, err := GetLaunchdLabel(ctx)
	if err != nil {
		return err
	}
	labels := map[string]bool{label: true}
	if allProfiles {
		fleet, err := LaunchdGatewayLabelsForInstall(ctx)
		if err != nil {
			return err
		}
		for _, l := range fleet {
			labels[l] = true
		}
	}
	ordered := make([]string, 0, len(labels))
	for l := range labels {
		ordered = append(ordered, l)
	}
	sort.Strings(ordered)
	for _, l := range ordered {
		_, pid, ok, err := LocateLaunchdGatewayService(ctx, l)
		if errors.Is(err, ErrCommandTimeout) {
			continue
		}
		if err != nil {
			return err
		}
		if ok && pid > 0 {
			pids[pid] = true
		}
	}
	if !allProfiles {
		return nil
	}
	res, err := RunServiceCommand(ctx, 5*time.Second, "launchctl", "list")
	if errors.Is(err, ErrCommandNotFound) || errors.Is(err, ErrCommandTimeout) {
		return nil
	}
	if err != nil {
		return err
	}
	if res.ReturnCode != 0 {
		return nil
	}
	for _, line := range pystr.Splitlines(pystr.Strip(res.Stdout), false) {
		parts := pystr.SplitWhitespace(line, -1)
		if len(parts) >= 3 && strings.HasPrefix(parts[len(parts)-1], launchdLabelBase) {
			if pid, ok := pyIntText(parts[0]); ok && pid > 0 {
				pids[pid] = true
			}
		}
	}
	return nil
}

// Parent pid when it is known and positive
func getParentPID(pid int) (int, bool) {
	if pid <= 1 {
		return 0, false
	}
	parent, ok := scanPPID(pid)
	if !ok || parent <= 0 {
		return 0, false
	}
	return parent, true
}

// This process and its ancestors up to init so scans never count the invoking CLI
func getAncestorPIDs() map[int]bool {
	ancestors := map[int]bool{}
	pid := scanGetpid()
	for range 64 {
		ancestors[pid] = true
		parent, ok := getParentPID(pid)
		if !ok || ancestors[parent] {
			break
		}
		pid = parent
	}
	return ancestors
}

// Appends a positive pid that is not this process, excluded or already present
func appendUniquePID(pids []int, pid int, exclude map[int]bool) []int {
	if pid <= 0 || pid == scanGetpid() || exclude[pid] {
		return pids
	}
	for _, p := range pids {
		if p == pid {
			return pids
		}
	}
	return append(pids, pid)
}

type pidCommand struct {
	pid     int
	command string
}

// Pid and space joined command line of every readable proc entry, an unreadable proc dir is an error
func iterProcCmdlines(exclude map[int]bool) ([]pidCommand, error) {
	entries, err := os.ReadDir(scanProcDir)
	if err != nil {
		return nil, err
	}
	me := scanGetpid()
	var out []pidCommand
	for _, entry := range entries {
		if !pystr.StrIsDigit(entry.Name()) {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == me || exclude[pid] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(scanProcDir, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		out = append(out, pidCommand{pid, strings.ReplaceAll(strings.ToValidUTF8(string(raw), "�"), "\x00", " ")})
	}
	return out, nil
}

// Pid and command of one ps line, also accepting ps aux rows
func parsePSLine(line string) (int, string, bool) {
	stripped := pystr.Strip(line)
	if stripped == "" || strings.Contains(stripped, "grep") {
		return 0, "", false
	}
	if parts := pystr.SplitWhitespace(stripped, 1); len(parts) == 2 {
		if pid, ok := pyIntText(parts[0]); ok {
			return pid, parts[1], true
		}
	}
	aux := pystr.SplitWhitespace(stripped, -1)
	if len(aux) > 10 && pystr.StrIsDigit(aux[1]) {
		if pid, ok := pyIntText(aux[1]); ok {
			return pid, strings.Join(aux[10:], " "), true
		}
	}
	return 0, "", false
}

// Pid and command line pairs of a wmic or CIM list listing
func iterWindowsListProcesses(listing string) []pidCommand {
	var out []pidCommand
	current := ""
	for _, line := range strings.Split(listing, "\n") {
		line = pystr.Strip(line)
		if strings.HasPrefix(line, "CommandLine=") {
			current = line[len("CommandLine="):]
		} else if strings.HasPrefix(line, "ProcessId=") {
			if pid, ok := pyIntText(line[len("ProcessId="):]); ok {
				out = append(out, pidCommand{pid, current})
			}
			current = ""
		}
	}
	return out
}

// Process listing of every Windows process from wmic, else Get-CimInstance, false when neither answers
func windowsProcessListing(ctx context.Context) (string, bool) {
	var res CommandResult
	have := false
	if wmic, err := serviceLookPath("wmic"); err == nil {
		r, err := runCommandDecoded(ctx, 10*time.Second, true, wmic, "process", "get", "ProcessId,CommandLine", "/FORMAT:LIST")
		res, have = r, err == nil
	}
	if !have || res.ReturnCode != 0 || res.Stdout == "" {
		shell, err := serviceLookPath("powershell")
		if err != nil {
			if shell, err = serviceLookPath("pwsh"); err != nil {
				return "", false
			}
		}
		psCmd := "Get-CimInstance Win32_Process | ForEach-Object {   'CommandLine=' + ($_.CommandLine -replace \"`r`n\",' ' -replace \"`n\",' ');   'ProcessId=' + $_.ProcessId;   '' }"
		r, err := runCommandDecoded(ctx, 15*time.Second, true, shell, "-NoProfile", "-Command", psCmd)
		if err != nil {
			return "", false
		}
		res = r
	}
	if res.ReturnCode != 0 {
		return "", false
	}
	return res.Stdout, true
}

// Drops venv launcher stubs that parent another matched pid
func filterVenvLauncherStubs(pids []int) []int {
	set := map[int]bool{}
	for _, p := range pids {
		set[p] = true
	}
	drop := map[int]bool{}
	for _, pid := range pids {
		if ppid, ok := scanPPID(pid); ok && set[ppid] {
			drop[ppid] = true
		}
	}
	var out []int
	for _, p := range pids {
		if !drop[p] {
			out = append(out, p)
		}
	}
	return out
}

// Owner uid of a live process from proc or ps on macOS, false when unknown
func pidUID(ctx context.Context, pid int) (int, bool) {
	if uid, ok := fileOwnerUID(filepath.Join(scanProcDir, strconv.Itoa(pid))); ok {
		return uid, true
	}
	if !IsMacOS() {
		return 0, false
	}
	res, err := RunServiceCommand(ctx, 2*time.Second, "ps", "-o", "uid=", "-p", strconv.Itoa(pid))
	if err != nil || res.ReturnCode != 0 || pystr.Strip(res.Stdout) == "" {
		return 0, false
	}
	return pyIntText(res.Stdout)
}

// True when another Unix user than this non root caller owns the pid
func PidIsOtherUsers(ctx context.Context, pid int) bool {
	me := 0
	if !IsWindows() {
		me = scanGeteuid()
	}
	if me == 0 {
		return false
	}
	owner, ok := pidUID(ctx, pid)
	return ok && owner != me
}

// Argv of a running process from proc or ps with shell splitting, false on Windows or when unreadable
func dashboardCmdlineForPID(ctx context.Context, pid int) ([]string, bool) {
	if IsWindows() {
		return nil, false
	}
	path := filepath.Join(scanProcDir, strconv.Itoa(pid), "cmdline")
	if _, err := os.Stat(path); err == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		var argv []string
		for _, part := range strings.Split(string(raw), "\x00") {
			if part != "" {
				argv = append(argv, strings.ToValidUTF8(part, "�"))
			}
		}
		return argv, len(argv) > 0
	}
	res, err := RunServiceCommand(ctx, 10*time.Second, "ps", "-p", strconv.Itoa(pid), "-o", "command=")
	if err != nil || res.ReturnCode != 0 {
		return nil, false
	}
	command := pystr.Strip(res.Stdout)
	if command == "" {
		return nil, false
	}
	argv, err := pyshlex.SplitPOSIX(command, false)
	if err != nil {
		argv = pystr.SplitWhitespace(command, -1)
	}
	return argv, len(argv) > 0
}

// Login home of the user the pid runs as, empty when unknown
func pidPasswdHome(pid int) string {
	uid, err := proc.RealUID(pid)
	if err != nil {
		var ok bool
		if uid, ok = fileOwnerUID(filepath.Join(scanProcDir, strconv.Itoa(pid))); !ok {
			return ""
		}
	}
	pw, err := pypwd.Getpwuid(uid)
	if err != nil {
		return ""
	}
	return pw.Dir
}

// Value of the first profile flag in argv
func argvProfileFlagValue(argv []string) (string, bool) {
	for i, tok := range argv {
		if (tok == "--profile" || tok == "-p") && i+1 < len(argv) {
			return argv[i+1], true
		}
		if strings.HasPrefix(tok, "--profile=") {
			return strings.SplitN(tok, "=", 2)[1], true
		}
	}
	return "", false
}

// Home a process runs under from its exec time environment and argv, requiring pinned launch identity when asked
func HermesHomeForPID(ctx context.Context, pid int, requireLaunchIdentity bool) (string, bool, error) {
	env, err := proc.Environ(pid)
	if err != nil {
		return "", false, nil
	}
	argv, ok := dashboardCmdlineForPID(ctx, pid)
	if requireLaunchIdentity && !ok {
		if argv, err = proc.Cmdline(pid); err != nil {
			return "", false, err
		}
	}
	if requireLaunchIdentity && len(argv) == 0 {
		return "", false, nil
	}
	envHome := pystr.Strip(env["ICEBATH_HOME"])
	profile, hasProfile := argvProfileFlagValue(argv)
	if requireLaunchIdentity {
		anchor := env["HOME"]
		if IsWindows() {
			if anchor = env["LOCALAPPDATA"]; anchor == "" {
				anchor = env["USERPROFILE"]
			}
		}
		if envHome != "" && !filepath.IsAbs(envHome) {
			return "", false, nil
		}
		if envHome == "" && (anchor == "" || !filepath.IsAbs(anchor)) {
			return "", false, nil
		}
	}
	if !hasProfile && envHome != "" && (filepath.Base(pypath.PathParent(envHome)) == "profiles" || env["ICEBATH_UPDATE_POST_SWAP"] == "1") {
		return envHome, true, nil
	}
	var defaultHome string
	if IsWindows() {
		base := pystr.Strip(env["LOCALAPPDATA"])
		if base == "" {
			up := env["USERPROFILE"]
			if up == "" {
				up, _ = os.UserHomeDir()
			}
			base = filepath.Join(up, "AppData", "Local")
		}
		defaultHome = filepath.Join(base, "icebath")
	} else {
		hd := env["HOME"]
		if hd == "" {
			hd = pidPasswdHome(pid)
		}
		if hd == "" {
			hd, _ = os.UserHomeDir()
		}
		defaultHome = filepath.Join(hd, ".icebath")
	}
	root := profiles.ProfileRootForEnvHome(envHome, defaultHome)
	fixed := tools.IsDesktopSSHBackendArgv(argv)
	for _, k := range []string{"ICEBATH_SUPERVISED_CHILD", "ICEBATH_S6_SUPERVISED_CHILD", "ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "ICEBATH_GATEWAY_PROFILE_PIN"} {
		fixed = fixed || env[k] != ""
	}
	if !hasProfile && !fixed {
		if requireLaunchIdentity {
			return "", false, nil
		}
		profile, hasProfile = profiles.GetActiveProfile(root), true
	}
	canon := "default"
	if hasProfile && profile != "" {
		if canon, err = profiles.NormalizeProfileName(profile); err != nil {
			return "", false, err
		}
	}
	if canon == "default" {
		return root, true, nil
	}
	return filepath.Join(root, "profiles", canon), true, nil
}

// Install identity key of a home so symlinked or differently spelled roots compare equal
func NormalizedHomeForCompare(h string) string {
	resolved, err := pypath.Resolve(h)
	if err != nil {
		resolved = h
	}
	return pypath.Normcase(resolved)
}

// This home's own gateway pid, only on fresh ownership evidence
func ownedGatewayPID(ctx context.Context, pidFile, ownHome string) (int, bool) {
	pid, found, err := GetRunningPid(pidFile, true)
	if err != nil || !found {
		return 0, false
	}
	owned, err := pidOwnedByHome(ctx, pidFile, pid, ownHome)
	if err != nil || !owned {
		return 0, false
	}
	again, found, err := GetRunningPid(pidFile, true)
	if err != nil || !found || again != pid {
		return 0, false
	}
	return pid, true
}

// Ownership ladder cheapest proof first, dialling the control socket only for the host pid
func pidOwnedByHome(ctx context.Context, pidFile string, pid int, ownHome string) (bool, error) {
	record := readHostRecord(roleGateway, false)
	isHost := record != nil && record.PID == pid && recordNamesLiveOwner(record)
	if isHost {
		same, err := sameHermesHome(hostRecordHome(record), ownHome)
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
	}
	owns, err := launchRecordOwns(pidFile, pid, ownHome)
	if err != nil || owns {
		return owns, err
	}
	if isHost {
		invalidateHostGatewayCache()
		owner := hostGateway()
		if owner != nil && owner.PID == pid && owner.ServedKnown {
			return sameHermesHome(owner.Home, ownHome)
		}
	}
	processHome, ok, err := homeForPIDHook(ctx, pid, true)
	if err != nil || !ok {
		return false, err
	}
	return sameHermesHome(processHome, ownHome)
}

// True when the pid file names this exact incarnation as launched from the home
func launchRecordOwns(pidFile string, pid int, ownHome string) (bool, error) {
	record := readPidRecord(pidFile)
	if record == nil {
		return false, nil
	}
	started, recordedHome := recGet(record, "start_time"), recGet(record, "hermes_home")
	recorded, isStr := recordedHome.(string)
	if !pyEq(recGet(record, "kind"), gatewayKind) || started == nil || !isStr {
		return false, nil
	}
	if got, ok := pidFromRecord(record, "pid"); !ok || got != pid || !pyEq(started, statusProcessStartTime(pid)) {
		return false, nil
	}
	return sameHermesHome(recorded, ownHome)
}

// Process table scan for gateway pids of the home in effect, every profile with allProfiles
func scanGatewayPIDs(ctx context.Context, exclude map[int]bool, allProfiles, includeRestartManagers bool) ([]int, error) {
	excl := map[int]bool{}
	for p := range exclude {
		excl[p] = true
	}
	for p := range getAncestorPIDs() {
		excl[p] = true
	}
	currentHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	currentHomeLC := strings.TrimRight(strings.ReplaceAll(pystr.Lower(currentHome), `\`, "/"), "/")
	profileArgText, err := profileArg(ctx, currentHome, "")
	if err != nil {
		return nil, err
	}
	currentProfile := ""
	if fields := pystr.SplitWhitespace(profileArgText, -1); profileArgText != "" && len(fields) > 0 {
		currentProfile = fields[len(fields)-1]
	}
	currentProfileLC := pystr.Lower(currentProfile)
	native := false
	if currentProfile == "" {
		if native, err = homeOwnsBareServiceName(ctx, currentHome); err != nil {
			return nil, err
		}
	}
	matchesCurrent := func(pid int, command string) (bool, error) {
		commandLC := strings.ReplaceAll(pystr.Lower(command), `\`, "/")
		flag, hasFlag := ProfileFlagValue(command)
		if currentProfile != "" {
			if hasFlag && flag == currentProfileLC {
				return true, nil
			}
			return CommandLineNamesHermesHome(commandLC, currentHomeLC), nil
		}
		if hasFlag && flag != "default" {
			return false, nil
		}
		if len(HermesHomeAssignments(commandLC)) > 0 {
			return CommandLineNamesHermesHome(commandLC, currentHomeLC), nil
		}
		owner, ok, err := homeForPIDHook(ctx, pid, false)
		if err != nil {
			return false, err
		}
		if ok {
			return NormalizedHomeForCompare(owner) == NormalizedHomeForCompare(currentHome), nil
		}
		return native, nil
	}
	var pids []int
	consider := func(pc pidCommand) error {
		runtimeMatch := LooksLikeGatewayCommandLine(pc.command) || (includeRestartManagers && LooksLikeGatewayRuntimeCommandLine(pc.command))
		if !runtimeMatch || PidIsOtherUsers(ctx, pc.pid) {
			return nil
		}
		ok := allProfiles
		if !ok {
			var err error
			if ok, err = matchesCurrent(pc.pid, pc.command); err != nil {
				return err
			}
		}
		if ok {
			pids = appendUniquePID(pids, pc.pid, excl)
		}
		return nil
	}
	if IsWindows() {
		listing, ok := windowsProcessListing(ctx)
		if !ok {
			return nil, nil
		}
		for _, pc := range iterWindowsListProcesses(listing) {
			if err := consider(pc); err != nil {
				return nil, err
			}
		}
		if len(pids) > 1 {
			pids = filterVenvLauncherStubs(pids)
		}
		return pids, nil
	}
	foundViaProc := false
	if st, err := os.Stat(scanProcDir); err == nil && st.IsDir() {
		if entries, err := iterProcCmdlines(excl); err == nil {
			foundViaProc = true
			for _, pc := range entries {
				if err := consider(pc); err != nil {
					foundViaProc = false
					break
				}
			}
		}
	}
	if !foundViaProc {
		res, err := RunServiceCommand(ctx, 10*time.Second, "ps", "-Aww", "-o", "pid=,command=")
		if err != nil {
			return nil, nil
		}
		if res.ReturnCode != 0 {
			return nil, nil
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if pid, command, ok := parsePSLine(line); ok {
				if err := consider(pidCommand{pid, command}); err != nil {
					return nil, err
				}
			}
		}
	}
	return pids, nil
}

// Running gateway pids of the profile in effect, or of every profile with allProfiles
func FindGatewayPIDsFor(ctx context.Context, exclude map[int]bool, allProfiles bool) ([]int, error) {
	return findGatewayPIDs(ctx, filepath.Join(processHermesHome(), "gateway.pid"), processHermesHome(), exclude, allProfiles)
}

// Running gateway pids of the profile whose home is h
func FindGatewayPIDs(ctx context.Context, h string) ([]int, error) {
	return findGatewayPIDs(home.WithOverride(ctx, h), filepath.Join(h, "gateway.pid"), h, nil, false)
}

// Owned pid, then service pids, then the process table scan, each deduplicated against exclude
func findGatewayPIDs(ctx context.Context, pidFile, ownHome string, exclude map[int]bool, allProfiles bool) ([]int, error) {
	excl := map[int]bool{}
	for p := range exclude {
		excl[p] = true
	}
	var pids []int
	if !allProfiles {
		if pid, ok := ownedGatewayPID(ctx, pidFile, ownHome); ok {
			pids = appendUniquePID(pids, pid, excl)
		}
	}
	service, err := servicePIDsHook(ctx, allProfiles)
	if err != nil {
		return nil, err
	}
	for _, pid := range service {
		pids = appendUniquePID(pids, pid, excl)
	}
	scanned, err := scanGatewayPIDs(ctx, excl, allProfiles, !SupportsSystemdServices(ctx))
	if err != nil {
		return nil, err
	}
	for _, pid := range scanned {
		pids = appendUniquePID(pids, pid, excl)
	}
	return pids, nil
}

// Runtime lock path of the gateway whose home is h
func GatewayRuntimeLockPath(h string) string {
	return gatewayLockPathLive(filepath.Join(h, "gateway.pid"))
}
