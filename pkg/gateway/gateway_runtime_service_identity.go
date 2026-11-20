package gateway

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Unit properties whose loaded values bind a systemd service to an account, home and command
var SystemdIdentityProperties = []string{
	"User", "DynamicUser", "ExecStart", "Environment", "EnvironmentFiles",
	"PassEnvironment", "UnsetEnvironment", "PAMName", "RootDirectory", "RootImage",
	"ExecStartPre", "ExecCondition", "DropInPaths",
}

var systemdOmittedWhenEmpty = []string{"EnvironmentFiles", "ExecStartPre", "ExecCondition"}

// ServiceIdentityError is a binding refusal, Foreign when the service serves another install root
type ServiceIdentityError struct {
	Reason  string
	Foreign bool
}

func (e *ServiceIdentityError) Error() string { return e.Reason }

func unverifiedIdentity() error { return &ServiceIdentityError{Reason: "service_identity_unverified"} }

// Resolved absolute path of a configured value, unverified when empty or relative
func identityAbsolute(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", unverifiedIdentity()
	}
	return pypath.Resolve(value)
}

// Root anchoring a home, the parent of its dot directory for a profiles child and the parent otherwise
func identityInstallRoot(h string) string {
	parts := strings.Split(filepath.ToSlash(h), "/")
	if len(parts) >= 3 && parts[len(parts)-3] == home.DirName && parts[len(parts)-2] == "profiles" {
		return filepath.Dir(filepath.Dir(filepath.Dir(h)))
	}
	return filepath.Dir(h)
}

// Accepts the same home, refuses a sibling profile as a mismatch and another root as foreign
func VerifyHome(h, configured string) error {
	ours, err := pypath.Resolve(h)
	if err != nil {
		return err
	}
	theirs, err := identityAbsolute(configured)
	if err != nil {
		return err
	}
	if pypath.Normcase(theirs) == pypath.Normcase(ours) {
		return nil
	}
	if pypath.Normcase(identityInstallRoot(theirs)) != pypath.Normcase(identityInstallRoot(ours)) {
		return &ServiceIdentityError{Reason: "foreign_root", Foreign: true}
	}
	return &ServiceIdentityError{Reason: "profile_mismatch"}
}

var pythonExecutableRE = regexp.MustCompile(`^python(?:w|\d+(?:\.\d+)*)?(?:\.exe)?$`)

// Whether executable is the store launcher of this install root, never another checkout's
func ownStoreLauncher(executable string) bool {
	root, err := pypath.Resolve(pm.RepoRoot())
	if err != nil {
		return false
	}
	resolved, err := pypath.Resolve(executable)
	if err != nil {
		return false
	}
	return pypath.Normcase(resolved) == pypath.Normcase(filepath.Join(root, home.DirName, "bin", "icebath"))
}

// Whether a token is the hidden default home pin of a runtime command
func runtimePinToken(token string) (string, bool) {
	if token == pm.PinDefaultHomeFlag {
		return "", true
	}
	if value, ok := strings.CutPrefix(token, pm.PinDefaultHomeFlag+"="); ok {
		return value, true
	}
	return "", false
}

// Module and argv of an installer persisted launch for this install root, false for any other form
func launchModule(argv []string, legacy bool) (string, []string, bool) {
	executable := ""
	if len(argv) > 0 {
		executable = strings.ToLower(filepath.Base(argv[0]))
	}
	if pythonExecutableRE.MatchString(executable) {
		if legacy && len(argv) > 2 && argv[1] == "-m" {
			return argv[2], argv[3:], true
		}
		return "", nil, false
	}
	if len(argv) > 2 && argv[1] == "--run-module" && ownStoreLauncher(argv[0]) {
		return argv[2], argv[3:], true
	}
	if len(argv) > 1 {
		if pinned, ok := runtimePinToken(argv[1]); ok {
			root, err := pypath.Resolve(pm.RepoRoot())
			if err != nil {
				return "", nil, false
			}
			regenerated := pm.InstallRuntimeCommand(root, nil, pm.LaunchCommandOptions{Home: pinned})
			if len(regenerated) == 2 && regenerated[1] == argv[1] && sameResolvedPath(regenerated[0], argv[0]) {
				return "icebath_cli.main", argv[2:], true
			}
		}
	}
	return "", nil, false
}

// Whether two paths resolve to the same case normalised path
func sameResolvedPath(a, b string) bool {
	ra, errA := pypath.Resolve(a)
	rb, errB := pypath.Resolve(b)
	return errA == nil && errB == nil && pypath.Normcase(ra) == pypath.Normcase(rb)
}

// Accepts only an installer form of gateway run for home with its quiet and supervisor flags
func VerifyGatewayArgv(argv []string, h string, wrapped bool) error {
	module, rest, ok := launchModule(argv, !wrapped)
	ownLauncherOnly := wrapped
	if ok && module == "icebath_cli.stderr_timestamp" {
		if len(rest) < 4 || rest[0] != "--error-log" || rest[2] != "--" {
			return unverifiedIdentity()
		}
		argv, ownLauncherOnly = rest[3:], true
	}
	if len(argv) == 0 || !LooksLikeGatewayCommandLine(pysubprocess.List2cmdline(argv)) {
		return unverifiedIdentity()
	}
	executable := strings.ToLower(filepath.Base(argv[0]))
	var args []string
	pinned := false
	if len(argv) > 1 {
		_, pinned = runtimePinToken(argv[1])
	}
	if pythonExecutableRE.MatchString(executable) || pinned {
		entry, entryArgs, isEntry := launchModule(argv, !wrapped)
		if !isEntry || entry != "icebath_cli.main" {
			return unverifiedIdentity()
		}
		args = entryArgs
	} else if (executable == "icebath" || executable == "icebath.exe") && (!ownLauncherOnly || ownStoreLauncher(argv[0])) {
		args = argv[1:]
	} else {
		return unverifiedIdentity()
	}
	var profiles, filtered []string
	for i := 0; i < len(args); i++ {
		token := args[i]
		switch {
		case token == "--profile" || token == "-p":
			i++
			if i == len(args) {
				return unverifiedIdentity()
			}
			profiles = append(profiles, args[i])
		case strings.HasPrefix(token, "--profile="):
			profiles = append(profiles, strings.SplitN(token, "=", 2)[1])
		default:
			filtered = append(filtered, token)
		}
	}
	if len(profiles) > 1 {
		return unverifiedIdentity()
	}
	if len(profiles) == 1 {
		expected := home.ProfileNameFor(h)
		if expected == "" {
			expected = "default"
			if filepath.Base(filepath.Dir(h)) == "profiles" {
				expected = filepath.Base(h)
			}
		}
		if profiles[0] != expected {
			return &ServiceIdentityError{Reason: "profile_mismatch"}
		}
	}
	if len(filtered) < 2 || filtered[0] != "gateway" || filtered[1] != "run" {
		return unverifiedIdentity()
	}
	for _, arg := range filtered[2:] {
		if arg != "--quiet" && arg != "--external-supervisor" {
			return unverifiedIdentity()
		}
	}
	return nil
}

// Key value pairs of a systemd environment list, unverified on C escapes or a bare word
func EnvironmentPairs(value string) (map[string]string, error) {
	if strings.Contains(value, "\\") {
		return nil, unverifiedIdentity()
	}
	items, err := pyshlex.SplitPOSIX(value, false)
	if err != nil {
		return nil, unverifiedIdentity()
	}
	result := map[string]string{}
	for _, item := range items {
		key, val, sep := strings.Cut(item, "=")
		if !sep {
			return nil, unverifiedIdentity()
		}
		result[key] = val
	}
	return result, nil
}

var systemdExecStartRE = regexp.MustCompile(`^\{ path=(.*?) ; argv\[\]=(.*?) ; ignore_errors=(?:yes|no) ;(?:.*?)\}$`)

// Account of a systemd user field, a decimal uid or a passwd name
func systemdServiceUID(user string) (int, error) {
	decimal := user != ""
	for _, r := range user {
		decimal = decimal && pystr.IsDecimal(r)
	}
	if decimal {
		digits := strings.Map(func(r rune) rune {
			if v, ok := pystr.DecimalValue(r); ok {
				return rune('0' + v)
			}
			return r
		}, user)
		return strconv.Atoi(digits)
	}
	p, err := pypwd.Getpwnam(user)
	if err != nil {
		var keyErr *pypwd.KeyError
		if errors.As(err, &keyErr) {
			return 0, unverifiedIdentity()
		}
		return 0, err
	}
	return p.UID, nil
}

// Binds loaded systemd properties and the manager environment to this account, home and gateway argv
func VerifySystemd(props map[string]string, managerEnv, h string, system bool) error {
	merged := map[string]string{}
	for _, key := range systemdOmittedWhenEmpty {
		merged[key] = ""
	}
	for k, v := range props {
		merged[k] = v
	}
	props = merged
	for _, key := range SystemdIdentityProperties {
		if _, ok := props[key]; !ok {
			return unverifiedIdentity()
		}
	}
	if props["DynamicUser"] != "no" {
		return unverifiedIdentity()
	}
	for _, key := range []string{"EnvironmentFiles", "PAMName", "RootDirectory", "RootImage", "ExecStartPre", "ExecCondition"} {
		if props[key] != "" {
			return unverifiedIdentity()
		}
	}
	uid := os.Getuid()
	serviceUID := uid
	if user := props["User"]; user != "" {
		var err error
		if serviceUID, err = systemdServiceUID(user); err != nil {
			return err
		}
	} else if system {
		serviceUID = 0
	}
	if serviceUID != uid {
		return &ServiceIdentityError{Reason: "service_account_mismatch"}
	}
	inherited, err := EnvironmentPairs(managerEnv)
	if err != nil {
		return err
	}
	env := inherited
	if system {
		env = map[string]string{}
		passed := map[string]bool{}
		for _, name := range pystr.SplitWhitespace(props["PassEnvironment"], -1) {
			passed[name] = true
		}
		for k, v := range inherited {
			if passed[k] {
				env[k] = v
			}
		}
	}
	own, err := EnvironmentPairs(props["Environment"])
	if err != nil {
		return err
	}
	for k, v := range own {
		env[k] = v
	}
	unset, err := pyshlex.SplitPOSIX(props["UnsetEnvironment"], false)
	if err != nil {
		return unverifiedIdentity()
	}
	for _, item := range unset {
		key, value, sep := strings.Cut(item, "=")
		if current, ok := env[key]; !sep || (ok && current == value) {
			delete(env, key)
		}
	}
	configured := pystr.Strip(env["ICEBATH_HOME"])
	if configured == "" {
		base := env["HOME"]
		if base == "" {
			account, err := pypwd.Getpwuid(uid)
			if err != nil {
				return err
			}
			base = account.Dir
		}
		abs, err := identityAbsolute(base)
		if err != nil {
			return err
		}
		configured = filepath.Join(abs, home.DirName)
	}
	if err := VerifyHome(h, configured); err != nil {
		return err
	}
	match := systemdExecStartRE.FindStringSubmatch(props["ExecStart"])
	if match == nil || strings.ContainsAny(match[2], "$\\{}") {
		return unverifiedIdentity()
	}
	argv, err := pyshlex.SplitPOSIX(match[2], false)
	if err != nil {
		return unverifiedIdentity()
	}
	if len(argv) == 0 || match[1] != argv[0] {
		return unverifiedIdentity()
	}
	return VerifyGatewayArgv(argv, h, false)
}

// Checks the command a launchd job runs, unwrapping the Local Network wrapper first
func verifyLaunchdArgv(argv []string, h string) error {
	command, wrapped, err := launchdCommand(argv)
	if err != nil {
		return err
	}
	return VerifyGatewayArgv(command, h, wrapped)
}

// Inner command of a launchd job, accepting the osascript wrapper only when regenerated byte for byte
func launchdCommand(argv []string) ([]string, bool, error) {
	if len(argv) == 0 || argv[0] != "/usr/bin/osascript" {
		return argv, false, nil
	}
	script, opener := argv[len(argv)-1], "$.system("
	var tokens []string
	if at := strings.Index(script, opener); at >= 0 {
		var shell any
		if json.NewDecoder(strings.NewReader(script[at+len(opener):])).Decode(&shell) == nil {
			if text, ok := shell.(string); ok {
				if split, err := pyshlex.SplitPOSIX(text, false); err == nil {
					tokens = split
				}
			}
		}
	}
	n := len(tokens)
	if n < 6 || tokens[0] != "exec" || tokens[n-4] != ">>" || tokens[n-2] != "2>>" {
		return nil, false, unverifiedIdentity()
	}
	command := tokens[1 : n-4]
	regenerated := LaunchdProgramArguments(command, tokens[n-3], tokens[n-1])
	if len(regenerated) != len(argv) {
		return nil, false, unverifiedIdentity()
	}
	for i := range argv {
		if regenerated[i] != argv[i] {
			return nil, false, unverifiedIdentity()
		}
	}
	return command, true, nil
}

// Binds launchctl print output of a loaded job to this account, home and gateway argv
func VerifyLaunchdLoaded(output, h string, uid int, username string) error {
	block := func(name string) ([]string, error) {
		re := regexp.MustCompile(`(?ms)^\t` + name + ` = \{\n(.*?)^\t\}`)
		found := re.FindAllStringSubmatch(output, -1)
		if len(found) != 1 {
			return nil, unverifiedIdentity()
		}
		return pystr.Splitlines(found[0][1], false), nil
	}
	for _, pair := range [][2]string{{"uid", strconv.Itoa(uid)}, {"username", username}} {
		values := regexp.MustCompile(`(?m)^\t`+pair[0]+` = (.+)$`).FindAllStringSubmatch(output, -1)
		if len(values) > 0 && (len(values) != 1 || values[0][1] != pair[1]) {
			return &ServiceIdentityError{Reason: "service_account_mismatch"}
		}
	}
	lines, err := block("environment")
	if err != nil {
		return err
	}
	env := map[string]string{}
	for _, line := range lines {
		if pystr.Strip(line) == "" {
			continue
		}
		key, value, sep := strings.Cut(pystr.Strip(line), " => ")
		if _, dup := env[key]; !sep || dup {
			return unverifiedIdentity()
		}
		env[key] = value
	}
	if err := VerifyHome(h, env["ICEBATH_HOME"]); err != nil {
		return err
	}
	if filepath.Base(filepath.Dir(h)) != "profiles" && env["ICEBATH_SUPERVISED_CHILD"] == "" {
		return unverifiedIdentity()
	}
	argLines, err := block("arguments")
	if err != nil {
		return err
	}
	argv := make([]string, len(argLines))
	for i, line := range argLines {
		argv[i] = pystr.Strip(line)
	}
	programs := regexp.MustCompile(`(?m)^\tprogram = (.+)$`).FindAllStringSubmatch(output, -1)
	if len(programs) != 1 || len(argv) == 0 || programs[0][1] != argv[0] {
		return unverifiedIdentity()
	}
	return verifyLaunchdArgv(argv, h)
}

// Binds an installed launchd plist to its label, this home and a gateway argv
func VerifyLaunchdPlist(definition map[string]any, label, h string) error {
	if definition["Label"] != label {
		return unverifiedIdentity()
	}
	if plistTruthy(definition["Disabled"]) {
		return &ServiceIdentityError{Reason: "service_disabled"}
	}
	if plistTruthy(definition["UserName"]) || plistTruthy(definition["GroupName"]) || plistTruthy(definition["RootDirectory"]) {
		return unverifiedIdentity()
	}
	envValue, present := definition["EnvironmentVariables"]
	if !present {
		envValue = map[string]any{}
	}
	env, ok := envValue.(map[string]any)
	if !ok {
		return unverifiedIdentity()
	}
	configured, _ := env["ICEBATH_HOME"].(string)
	if _, isString := env["ICEBATH_HOME"].(string); !isString && env["ICEBATH_HOME"] != nil {
		return unverifiedIdentity()
	}
	if err := VerifyHome(h, configured); err != nil {
		return err
	}
	list, ok := definition["ProgramArguments"].([]any)
	if !ok {
		return unverifiedIdentity()
	}
	argv := make([]string, len(list))
	for i, item := range list {
		text, isString := item.(string)
		if !isString {
			return unverifiedIdentity()
		}
		argv[i] = text
	}
	var first any
	if len(argv) > 0 {
		first = argv[0]
	}
	program, hasProgram := definition["Program"]
	if !hasProgram {
		program = first
	}
	if program != first {
		return unverifiedIdentity()
	}
	if err := verifyLaunchdArgv(argv, h); err != nil {
		return err
	}
	if filepath.Base(filepath.Dir(h)) != "profiles" && !plistTruthy(env["ICEBATH_SUPERVISED_CHILD"]) {
		return unverifiedIdentity()
	}
	return nil
}

// Python truthiness of a decoded plist value, dicts and data included
func plistTruthy(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) > 0
	case []byte:
		return len(x) > 0
	case int:
		return x != 0
	}
	return pyTruthy(v)
}

// Bytes of a regular service definition up to one MiB, never blocking on a FIFO
func ReadDefinition(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, unverifiedIdentity()
	}
	value, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(value) > 1024*1024 {
		return nil, unverifiedIdentity()
	}
	return value, nil
}

// Text of a launcher, UTF-16 behind a byte order mark and UTF-8 with an optional signature otherwise
func LauncherText(raw []byte) (string, error) {
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe}) || bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
		if len(raw)%2 != 0 {
			return "", unverifiedIdentity()
		}
		big := raw[0] == 0xfe
		units := make([]uint16, 0, len(raw)/2-1)
		for i := 2; i+1 < len(raw); i += 2 {
			if big {
				units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
			} else {
				units = append(units, uint16(raw[i+1])<<8|uint16(raw[i]))
			}
		}
		for i := 0; i < len(units); i++ {
			if utf16.IsSurrogate(rune(units[i])) {
				if units[i] >= 0xdc00 || i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return "", unverifiedIdentity()
				}
				i++
			}
		}
		return string(utf16.Decode(units)), nil
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) {
		return "", unverifiedIdentity()
	}
	return string(raw), nil
}

const vbsLiteral = `"(?:[^"\r\n]|"")*"`

var (
	vbsEnvLineRE    = regexp.MustCompile(`^env.Item\("([A-Z_]+)"\) = (` + vbsLiteral + `)$`)
	vbsPathPrefixRE = regexp.MustCompile(`^  env.Item\("PYTHONPATH"\) = ` + vbsLiteral + ` & existing_pp$`)
	vbsPathOnlyRE   = regexp.MustCompile(`^  env.Item\("PYTHONPATH"\) = ` + vbsLiteral + `$`)
	vbsCwdRE        = regexp.MustCompile(`^sh.CurrentDirectory = ` + vbsLiteral + `$`)
	vbsRunRE        = regexp.MustCompile(`^sh.Run (` + vbsLiteral + `), 0, False$`)
)

// Home and argv of the vendor scheduled task vbs template, unverified for any other script
func vbsIdentity(script string) (string, []string, error) {
	lines := pystr.Splitlines(script, false)
	if len(lines) < 16 || !strings.HasPrefix(lines[0], "' ") {
		return "", nil, unverifiedIdentity()
	}
	fixed := []string{"Option Explicit", "Dim sh, env, existing_pp", `Set sh = CreateObject("WScript.Shell")`, `Set env = sh.Environment("PROCESS")`}
	for i, want := range fixed {
		if lines[1+i] != want {
			return "", nil, unverifiedIdentity()
		}
	}
	allowed := map[string]bool{"ICEBATH_HOME": true, "ICEBATH_SUPERVISED_CHILD": true, "ICEBATH_GATEWAY_DETACHED": true, "PYTHONIOENCODING": true, "VIRTUAL_ENV": true}
	env := map[string]string{}
	index := 5
	for ; index < len(lines); index++ {
		match := vbsEnvLineRE.FindStringSubmatch(lines[index])
		if match == nil {
			break
		}
		if _, dup := env[match[1]]; !allowed[match[1]] || dup {
			return "", nil, unverifiedIdentity()
		}
		env[match[1]] = strings.ReplaceAll(match[2][1:len(match[2])-1], `""`, `"`)
	}
	tail := lines[index:]
	if len(tail) != 8 || tail[0] != `existing_pp = env.Item("PYTHONPATH")` || tail[1] != "If Len(existing_pp) > 0 Then" || tail[3] != "Else" || tail[5] != "End If" {
		return "", nil, unverifiedIdentity()
	}
	if !vbsPathPrefixRE.MatchString(tail[2]) || !vbsPathOnlyRE.MatchString(tail[4]) || !vbsCwdRE.MatchString(tail[6]) {
		return "", nil, unverifiedIdentity()
	}
	run := vbsRunRE.FindStringSubmatch(tail[7])
	if run == nil || env["ICEBATH_SUPERVISED_CHILD"] == "" {
		return "", nil, unverifiedIdentity()
	}
	command := strings.ReplaceAll(run[1][1:len(run[1])-1], `""`, `"`)
	if strings.Contains(command, "%") || strings.Contains(env["ICEBATH_HOME"], "%") {
		return "", nil, unverifiedIdentity()
	}
	parts, err := pyshlex.SplitNonPOSIX(command)
	if err != nil {
		return "", nil, unverifiedIdentity()
	}
	argv := make([]string, len(parts))
	for i, part := range parts {
		argv[i] = strings.Trim(part, `"`)
	}
	return env["ICEBATH_HOME"], argv, nil
}

type xmlNode struct {
	name     xml.Name
	attrs    map[string]string
	text     string
	children []*xmlNode
}

// Element tree of an XML document with each element's leading text, an error on malformed input
func parseXMLTree(data string) (*xmlNode, error) {
	dec := xml.NewDecoder(strings.NewReader(data))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	var stack []*xmlNode
	var root *xmlNode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			node := &xmlNode{name: t.Name, attrs: map[string]string{}}
			for _, a := range t.Attr {
				if a.Name.Space == "" {
					node.attrs[a.Name.Local] = a.Value
				}
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			} else if root == nil {
				root = node
			}
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && len(stack[len(stack)-1].children) == 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("no element found")
	}
	return root, nil
}

const taskNamespace = "http://schemas.microsoft.com/windows/2004/02/mit/task"

// Children of n along a path of task namespace element names
func (n *xmlNode) findAll(path ...string) []*xmlNode {
	current := []*xmlNode{n}
	for _, step := range path {
		var next []*xmlNode
		for _, c := range current {
			for _, child := range c.children {
				if child.name.Space == taskNamespace && child.name.Local == step {
					next = append(next, child)
				}
			}
		}
		current = next
	}
	return current
}

// Text of the first element along path, def when there is none
func (n *xmlNode) findText(def string, path ...string) string {
	if found := n.findAll(path...); len(found) > 0 {
		return found[0].text
	}
	return def
}

// Binds a scheduled task definition to this account, the vendor launcher, this home and a gateway argv
func VerifyWindowsTask(definition, h, account, sid string) error {
	task, err := parseXMLTree(definition)
	if err != nil {
		return unverifiedIdentity()
	}
	principals := task.findAll("Principals", "Principal")
	actionsList := task.findAll("Actions")
	if len(principals) != 1 || len(actionsList) == 0 || len(actionsList[0].children) != 1 {
		return unverifiedIdentity()
	}
	actions, principal := actionsList[0], principals[0]
	user := principal.findText("", "UserId")
	if user == "" || account == "" || sid == "" || len(principal.findAll("GroupId")) > 0 {
		return unverifiedIdentity()
	}
	if folded := pystr.Casefold(user); folded != pystr.Casefold(account) && folded != pystr.Casefold(sid) {
		return &ServiceIdentityError{Reason: "service_account_mismatch"}
	}
	context, hasContext := actions.attrs["Context"]
	id, hasID := principal.attrs["id"]
	if principal.findText("LeastPrivilege", "RunLevel") != "LeastPrivilege" || hasContext != hasID || context != id {
		return unverifiedIdentity()
	}
	if task.findText("true", "Settings", "Enabled") != "true" {
		return &ServiceIdentityError{Reason: "service_disabled"}
	}
	action := actions.children[0]
	command := strings.ToLower(action.findText("", "Command"))
	if action.name.Space != taskNamespace || action.name.Local != "Exec" || (command != "wscript.exe" && command != `c:\windows\system32\wscript.exe`) {
		return unverifiedIdentity()
	}
	match := windowsTaskArgsRE.FindStringSubmatch(action.findText("", "Arguments"))
	if match == nil {
		return unverifiedIdentity()
	}
	script, err := identityAbsolute(match[1])
	if err != nil {
		return err
	}
	if strings.ToLower(filepath.Ext(script)) != ".vbs" {
		return unverifiedIdentity()
	}
	raw, err := ReadDefinition(script)
	if err != nil {
		return err
	}
	text, err := LauncherText(raw)
	if err != nil {
		return err
	}
	configured, argv, err := vbsIdentity(text)
	if err != nil {
		return err
	}
	if err := VerifyHome(h, configured); err != nil {
		return err
	}
	return VerifyGatewayArgv(argv, h, false)
}

var windowsTaskArgsRE = regexp.MustCompile(`^//B //Nologo "([^"\r\n]+)"$`)
