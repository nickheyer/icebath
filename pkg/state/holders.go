package state

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// One holder of a state database, PID -1 marking a scan that could not prove quiescence
type Holder struct {
	PID    int
	Target string
}

// SQLite connection the repair probe drives, closed by the probe once it settles
type RepairConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Close() error
}

// File URI opening the resolved path read only, percent encoding characters that would truncate it
func ReadOnlyDBURI(dbPath string) (string, error) {
	resolved, err := pyRealpath(dbPath)
	if err != nil {
		return "", err
	}
	return pathAsURI(resolved) + "?mode=ro", nil
}

// pathlib as_uri of an absolute path, drive letter paths under file triple slash and UNC shares under file host
func pathAsURI(p string) string {
	if runtime.GOOS == "windows" {
		drive := filepath.VolumeName(p)
		posix := filepath.ToSlash(p)
		if len(drive) == 2 && drive[1] == ':' {
			return "file:///" + drive + urlQuoteBytes(posix[2:])
		}
		if drive != "" {
			return "file:" + urlQuoteBytes(posix)
		}
	}
	return "file://" + urlQuoteBytes(p)
}

// urllib quote_from_bytes with slash safe, every other byte outside the unreserved set percent encoded
func urlQuoteBytes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var (
	icebathExecutables             = setOf("icebath", "icebath-agent", "icebath-acp")
	icebathPythonModules           = setOf("acp_adapter", "icebath_cli.main")
	icebathPythonScripts           = []string{"icebath_cli/main.py", "run_agent.py"}
	pythonShortOptionsWithOperands = setOf("Q", "W", "X")
	pythonLongOptionsWithOperands  = setOf("--check-hash-based-pycs", "--jit")
)

// Interpreter short options taking the next argv token as operand, shared with the gateway inline source walk
func PythonShortOptionTakesOperand(letter string) bool { return pythonShortOptionsWithOperands[letter] }

// Interpreter long options taking the next argv token as operand, shared with the gateway inline source walk
func PythonLongOptionTakesOperand(option string) bool { return pythonLongOptionsWithOperands[option] }

// Current process id as the holder scans see it, swapped by tests projecting a fake process table
var holderGetpid = os.Getpid

// Reads the procfs argv of pid keeping NUL boundaries, false when unreadable or empty
var readProcArgv = func(pid int) ([]string, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	argv := strings.Split(pyDecodeUTF8Replace(raw), "\x00")
	if argv[len(argv)-1] == "" {
		argv = argv[:len(argv)-1]
	}
	return argv, len(argv) > 0
}

// Operator facing PID label with the first 80 characters of its whitespace collapsed command line
func DescribeHolderPID(pid int) string {
	argv, ok := readProcArgv(pid)
	if !ok {
		if cmd, err := proc.Cmdline(pid); err == nil && len(cmd) > 0 {
			argv, ok = cmd, true
		}
	}
	who := "command line unavailable"
	if ok {
		joined := strings.Join(append([]string{pyBasename(argv[0])}, argv[1:]...), " ")
		r := []rune(strings.Join(pySplitWhitespace(joined), " "))
		who = string(r[:min(len(r), 80)])
	}
	return fmt.Sprintf("PID %d (%s)", pid, who)
}

// Lower case program basename without a trailing .exe
func programName(program string) string {
	return strings.TrimSuffix(strings.ToLower(pyBasename(program)), ".exe")
}

// Whether a program names a python or pypy interpreter with an optional dotted version suffix
func looksLikePythonExecutable(program string) bool {
	name := programName(program)
	for _, prefix := range []string{"python", "pypy"} {
		if suffix, ok := strings.CutPrefix(name, prefix); ok {
			for _, r := range suffix {
				if !unicode.IsDigit(r) && r != '.' {
					return false
				}
			}
			return true
		}
	}
	return false
}

// Module or script an interpreter argv runs, false for -c, a bare stdin run or a missing operand
func pythonExecutionTarget(argv []string) (kind, value string, ok bool) {
	for index := 1; index < len(argv); {
		arg := argv[index]
		switch {
		case arg == "--":
			index++
			if index < len(argv) {
				return "script", argv[index], true
			}
			return "", "", false
		case pythonLongOptionsWithOperands[arg]:
			index += 2
			continue
		case strings.HasPrefix(arg, "--check-hash-based-pycs=") || strings.HasPrefix(arg, "--jit="):
			index++
			continue
		case strings.HasPrefix(arg, "--"):
			index++
			continue
		case strings.HasPrefix(arg, "-") && arg != "-":
			options := []rune(arg[1:])
			consumedNext := false
			for i := 0; i < len(options); i++ {
				option, attached := string(options[i]), string(options[i+1:])
				if option == "c" {
					return "", "", false
				}
				if option == "m" {
					if attached != "" {
						return "module", attached, true
					}
					index++
					if index < len(argv) {
						return "module", argv[index], true
					}
					return "", "", false
				}
				if pythonShortOptionsWithOperands[option] {
					consumedNext = attached == ""
					break
				}
			}
			if consumedNext {
				index += 2
			} else {
				index++
			}
			continue
		}
		return "script", arg, true
	}
	return "", "", false
}

// Whether argv runs an icebath executable or an interpreter on an icebath module or script
func looksLikeIcebath(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	program := programName(argv[0])
	if icebathExecutables[program] {
		return true
	}
	if !looksLikePythonExecutable(program) {
		return false
	}
	kind, value, ok := pythonExecutionTarget(argv)
	if !ok {
		return false
	}
	normalized := strings.ReplaceAll(strings.ToLower(value), `\`, "/")
	if kind == "module" {
		return icebathPythonModules[normalized]
	}
	for _, script := range icebathPythonScripts {
		if normalized == script || strings.HasSuffix(normalized, "/"+script) {
			return true
		}
	}
	return false
}

// Normalized absolute form of a procfs descriptor target with the deleted suffix stripped
func CanonicalSQLitePath(path string) (string, error) {
	abs, err := pyAbspath(strings.TrimSuffix(path, " (deleted)"))
	if err != nil {
		return "", err
	}
	return normcase(abs), nil
}

var (
	homeFlags    = []string{"--icebath-home"}
	profileFlags = []string{"--profile", "-p"}
	stateDBNames = setOf("state.db", "state.db-wal", "state.db-shm")
)

// Case folded normpath of a path token
func normPath(value string) string { return normcase(pyNormpath(value)) }

// Last value given to one of flags as a separate token or after an equals sign, matched token exact
func argvFlagValue(argv []string, flags []string) (string, bool) {
	value, found := "", false
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		if slices.Contains(flags, token) && index+1 < len(argv) {
			value, found = argv[index+1], true
			index++
			continue
		}
		for _, flag := range flags {
			if v, ok := strings.CutPrefix(token, flag+"="); ok {
				value, found = v, true
				break
			}
		}
	}
	return value, found
}

// Value of the last ICEBATH_HOME assignment token on argv
func argvEnvHome(argv []string) (string, bool) {
	for i := len(argv) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(argv[i], home.EnvHome+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Install root and own profile name of the home holding the store, empty when it is no install layout
func storeInstallLayout(thisHome string) (root, profile string, ok, named bool) {
	if profileHome := home.NamedProfileHome(thisHome); profileHome != "" {
		abs, err := pyAbspath(filepath.Dir(filepath.Dir(profileHome)))
		if err != nil {
			slog.Debug(fmt.Sprintf("Could not classify the install layout of %s", thisHome), "error", err)
			return "", "", false, false
		}
		return abs, filepath.Base(profileHome), true, true
	}
	if pyBasename(thisHome) == ".icebath" {
		abs, err := pyAbspath(thisHome)
		if err != nil {
			slog.Debug(fmt.Sprintf("Could not classify the install layout of %s", thisHome), "error", err)
			return "", "", false, false
		}
		return abs, "", true, false
	}
	return "", "", false, false
}

// Whether a normalized token lies under the profiles tree of the install root for a profile other than ours
func namesOtherProfile(normalized, installRoot string, hasRoot bool, ourProfile string, hasProfile bool) bool {
	if !hasRoot {
		return false
	}
	prefix := normPath(filepath.Join(installRoot, home.ProfilesDir)) + string(filepath.Separator)
	rest, ok := strings.CutPrefix(normalized, prefix)
	if !ok {
		return false
	}
	name, _, _ := strings.Cut(rest, string(filepath.Separator))
	return name != "" && !(hasProfile && name == normcase(ourProfile))
}

// ours, other or empty from the home or profile selection the process makes on its own argv
func argvHomeSelection(argv []string, thisHome, installRoot string, hasRoot bool, ourProfile string, hasProfile bool) string {
	homeValue, _ := argvFlagValue(argv, homeFlags)
	if homeValue == "" {
		homeValue, _ = argvEnvHome(argv)
	}
	if homeValue != "" {
		if normPath(homeValue) == normPath(thisHome) {
			return "ours"
		}
		return "other"
	}
	if profileValue, _ := argvFlagValue(argv, profileFlags); profileValue != "" {
		if hasProfile {
			if profileValue == ourProfile {
				return "ours"
			}
			return "other"
		}
		if hasRoot && profileValue == "default" {
			return "ours"
		}
		return "other"
	}
	return ""
}

// One absolute path carried by argv, with the argv index it came from
type argvPathToken struct {
	index int
	path  string
}

// Normalized absolute paths of argv tokens, option values after an equals sign included
func argvPathTokens(argv []string) []argvPathToken {
	var tokens []argvPathToken
	for index, token := range argv {
		pathToken, ok := "", false
		if strings.HasPrefix(token, "/") {
			pathToken, ok = token, true
		} else if strings.HasPrefix(token, "-") && strings.Contains(token, "=") {
			_, value, _ := strings.Cut(token, "=")
			pathToken, ok = value, strings.HasPrefix(value, "/")
		}
		if ok {
			tokens = append(tokens, argvPathToken{index, normPath(pathToken)})
		}
	}
	return tokens
}

// Whether argv proves the process serves a different icebath home and never names this store or home
func argvScopedToOtherHome(argv []string, dbPath string) (bool, error) {
	dbPathStr, err := pyAbspath(dbPath)
	if err != nil {
		return false, err
	}
	thisHome := pyDirname(dbPathStr)
	installRoot, ourProfile, hasRoot, hasProfile := storeInstallLayout(thisHome)
	sidecars := map[string]bool{}
	for _, candidate := range sqliteFamily(dbPathStr) {
		sidecars[normcase(candidate)] = true
	}
	thisHomeNorm := normcase(thisHome)
	rootNorm := ""
	if hasRoot {
		rootNorm = normcase(installRoot)
	}
	pathTokens := argvPathTokens(argv)
	for _, tok := range pathTokens {
		if sidecars[tok.path] {
			return false, nil
		}
	}
	selection := argvHomeSelection(argv, thisHome, installRoot, hasRoot, ourProfile, hasProfile)
	if selection == "ours" {
		return false, nil
	}
	sep := string(filepath.Separator)
	otherHomeSeen := selection == "other"
	for _, tok := range pathTokens {
		normalized := tok.path
		if namesOtherProfile(normalized, installRoot, hasRoot, ourProfile, hasProfile) {
			otherHomeSeen = true
			continue
		}
		if normalized == thisHomeNorm || strings.HasPrefix(normalized, thisHomeNorm+sep) {
			return false, nil
		}
		if hasRoot && (normalized == rootNorm || strings.HasPrefix(normalized, rootNorm+sep)) {
			continue
		}
		if tok.index == 0 && !hasRoot {
			continue
		}
		if strings.Contains(normalized, "/.icebath") || stateDBNames[pyBasename(normalized)] {
			otherHomeSeen = true
		}
	}
	return otherHomeSeen, nil
}

// Whether an argv read from procfs is an icebath process that may serve this store
func icebathHolderArgv(argv []string, dbPath string) (bool, error) {
	if !looksLikeIcebath(argv) {
		return false, nil
	}
	other, err := argvScopedToOtherHome(argv, dbPath)
	return !other, err
}

const (
	rmErrorMoreData = 234
	rmSessionKeyLen = 33
)

// RM_UNIQUE_PROCESS, the process id and its FILETIME start
type rmUniqueProcess struct {
	PID                 uint32
	StartLow, StartHigh uint32
}

// RM_PROCESS_INFO as rstrtmgr fills it
type rmProcessInfo struct {
	Process     rmUniqueProcess
	AppName     [256]uint16
	ServiceName [64]uint16
	AppType     uint32
	AppStatus   uint32
	TSSessionID uint32
	Restartable int32
}

// The four Restart Manager entry points the holder scan calls, each returning its Win32 status
type rmAPI interface {
	StartSession(session *uint32, flags uint32, key []uint16) uint32
	RegisterResources(session uint32, filenames []string) uint32
	GetList(session uint32, needed, count *uint32, apps []rmProcessInfo, reasons *uint32) uint32
	EndSession(session uint32) uint32
}

// OSError raised with a Restart Manager status code and the failing call
type rmCallError struct {
	Code uint32
	Msg  string
}

func (e *rmCallError) Error() string { return fmt.Sprintf("[Errno %d] %s", e.Code, e.Msg) }

// Main database plus the WAL and SHM sidecars SQLite may keep beside it
func sqliteFamily(base string) []string {
	return []string{base, base + "-wal", base + "-shm"}
}

// Foreign processes Restart Manager reports using the database or a sidecar, our own pid excluded
func windowsRestartManagerHolders(dbPath string, api rmAPI) ([]Holder, error) {
	dbAbspath, err := pyAbspath(dbPath)
	if err != nil {
		return nil, err
	}
	var resources []string
	for _, path := range sqliteFamily(dbAbspath) {
		if _, err := os.Stat(path); err == nil {
			resources = append(resources, path)
		}
	}
	if len(resources) == 0 {
		return []Holder{}, nil
	}
	var session uint32
	key := make([]uint16, rmSessionKeyLen)
	if rc := api.StartSession(&session, 0, key); rc != 0 {
		return nil, &rmCallError{rc, "RmStartSession failed"}
	}
	defer api.EndSession(session)
	if rc := api.RegisterResources(session, resources); rc != 0 {
		return nil, &rmCallError{rc, "RmRegisterResources failed"}
	}
	var needed, reasons uint32
	for range 4 {
		var apps []rmProcessInfo
		if needed != 0 {
			apps = make([]rmProcessInfo, needed)
		}
		count := needed
		rc := api.GetList(session, &needed, &count, apps, &reasons)
		if rc == 0 {
			if int(count) > len(apps) {
				return nil, fmt.Errorf("invalid index")
			}
			ownPID := holderGetpid()
			holders := []Holder{}
			for _, app := range apps[:count] {
				if int(app.Process.PID) != ownPID {
					holders = append(holders, Holder{int(app.Process.PID), dbAbspath})
				}
			}
			return holders, nil
		}
		if rc != rmErrorMoreData {
			return nil, &rmCallError{rc, "RmGetList failed"}
		}
	}
	return nil, fmt.Errorf("Restart Manager holder set kept changing")
}

// Foreign holders of the database or a sidecar, a failed or partial scan reported as an unknown holder
func ForeignStateDBHolders(dbPath string) ([]Holder, error) {
	return foreignStateDBHolders(dbPath)
}

// Windows lane of the foreign holder scan, a Restart Manager failure becoming the unknown holder
func restartManagerScan(dbPath string, load func() (rmAPI, error)) []Holder {
	holders, err := func() ([]Holder, error) {
		api, err := load()
		if err != nil {
			return nil, err
		}
		return windowsRestartManagerHolders(dbPath, api)
	}()
	if err != nil {
		slog.Warn("Could not prove state.db has no Windows holders; deferring structural maintenance: " + pyOSErrorStr(err))
		return []Holder{{-1, "Windows Restart Manager scan failed: " + pyOSErrorStr(err)}}
	}
	return holders
}

// Live generations other than exclude this process keeps open on dbPath, as holders of our own pid
func InProcessStateDBHolders(dbPath string, exclude *DB) []Holder {
	out := []Holder{}
	for _, description := range OtherGenerationsForPath(dbPath, exclude) {
		out = append(out, Holder{holderGetpid(), description})
	}
	return out
}

// Operator refusal naming every foreign holder of the store, false when the store is provably quiet
func HeldStoreRefusal(ctx context.Context, dbPath, command, forceHint string) (string, bool, error) {
	holders, err := ForeignStateDBHolders(dbPath)
	if err != nil || len(holders) == 0 {
		return "", false, err
	}
	byPID := map[int]map[string]bool{}
	var unknown []string
	for _, h := range holders {
		if h.PID <= 0 || strings.HasPrefix(h.Target, "uninspectable") {
			unknown = append(unknown, h.Target)
			continue
		}
		if byPID[h.PID] == nil {
			byPID[h.PID] = map[string]bool{}
		}
		byPID[h.PID][pathlibName(strings.TrimSuffix(h.Target, " (deleted)"))] = true
	}
	lines := []string{fmt.Sprintf("Refusing `icebath sessions %s`: another process is using %s.", command, pathlibStr(dbPath))}
	pids := make([]int, 0, len(byPID))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	for _, pid := range pids {
		names := make([]string, 0, len(byPID[pid]))
		for name := range byPID[pid] {
			names = append(names, name)
		}
		slices.Sort(names)
		lines = append(lines, fmt.Sprintf("  %s: %s", DescribeHolderPID(pid), strings.Join(names, ", ")))
	}
	if len(unknown) > 0 {
		r := []rune(unknown[0])
		lines = append(lines, fmt.Sprintf("  cannot prove the database is quiet (holder scan incomplete: %s)", string(r[:min(len(r), 120)])))
	}
	profileArg := ""
	if name := home.ProfileNameFor(home.Dir(ctx)); name != "" && name != "default" {
		profileArg = "-p " + name + " "
	}
	lines = append(lines,
		"Rewriting the database under a live writer is how every agent ends up refusing turns with the "+
			"retired state.db-wal error. Nothing is lost.",
		fmt.Sprintf("Stop them first (`icebath %sgateway stop`, quit the Desktop app, pause cron), then re-run.", profileArg))
	if forceHint != "" {
		lines = append(lines, fmt.Sprintf("Override with %s if you accept the risk.", forceHint))
	}
	lines = append(lines, "Recovery guide: "+StorageRecoveryDocsURL)
	return strings.Join(lines, "\n"), true, nil
}

// Whether repair lacks proven exclusive ownership, any foreign holder or a positive lock probe saying so
func LiveWriterHoldsDB(dbPath string, connectRepairDurable func(dbPath string, timeout float64) (RepairConn, error)) (bool, error) {
	holders, err := ForeignStateDBHolders(dbPath)
	if err != nil {
		return false, err
	}
	if len(holders) > 0 {
		return true, nil
	}
	ctx := context.Background()
	probe, err := connectRepairDurable(dbPath, 0.0)
	if err == nil {
		defer func() {
			_, _ = probe.ExecContext(ctx, "PRAGMA locking_mode=NORMAL")
			_ = probe.Close()
		}()
		for _, stmt := range []string{"PRAGMA locking_mode=EXCLUSIVE", "BEGIN IMMEDIATE", "ROLLBACK"} {
			if _, err = probe.ExecContext(ctx, stmt); err != nil {
				break
			}
		}
		if err == nil {
			return false, nil
		}
	}
	if isOperationalError(err) {
		return IsSQLiteLockError(err), nil
	}
	if isDatabaseError(err) {
		return false, nil
	}
	return false, err
}
