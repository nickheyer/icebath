package codeexec

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	cuaDriverCmdEnv     = "ICEBATH_CUA_DRIVER_CMD"
	cuaDriverDefaultCmd = "cua-driver"
)

var (
	cuaDriverArgs               = []string{"mcp"}
	cuaDriverRuntimeContractMin = [3]int{0, 20, 0}
	cuaSemverRE                 = regexp.MustCompile(`^v?(\p{Nd}+)\.(\p{Nd}+)\.(\p{Nd}+)(?:[-+].*)?$`)
	windowsAbsPathRE            = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
)

// Required verb flags of the runtime contract in the key order the missing text follows
var cuaDriverRuntimeContractArgs = []struct {
	verb  string
	flags []string
}{
	{"mcp", []string{"--socket", "--grant"}},
	{"serve", []string{"--socket", "--permission-mode", "--capability-manifest", "--approve-capability-manifest", "--embedded"}},
	{"stop", []string{"--socket"}},
}

// Parsed JSON object stdout of a driver verb, nil on spawn failure, empty or non object output or a nonzero exit
func driverJSON(env *tools.Env, driverCmd string, timeout float64, args ...string) *pyjson.Object {
	proc, err := runDriver(env, driverCmd, timeout, args...)
	if err != nil || proc == nil {
		return nil
	}
	out := pystr.Strip(proc.Stdout)
	if out == "" || proc.Returncode != 0 {
		return nil
	}
	return jsonObject(out)
}

// Decoded JSON object of text, nil when unparseable or not an object
func jsonObject(text string) *pyjson.Object {
	data, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	obj, _ := data.(*pyjson.Object)
	return obj
}

// Argument list of an mcp_invocation object when every item is a string, possibly empty
func validMCPArgs(invocation any) ([]string, bool) {
	inv, ok := invocation.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	raw, _ := inv.Get("args")
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		s, ok := a.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// Whether value holds the host path separator or its alternate
func hasPathSeparator(value string) bool {
	if isWindows {
		return strings.ContainsAny(value, `\/`)
	}
	return strings.Contains(value, "/")
}

var isWSL = hostinfo.IsWSL

// DrvFS form of a Windows absolute manifest command under WSL, every other path unchanged
func wslWindowsPathToPosix(path string) string {
	if !windowsAbsPathRE.MatchString(path) {
		return path
	}
	wsl := isWSL()
	drive, _, rest := pypath.NtSplitRoot(path)
	drive = strings.ToLower(strings.TrimRight(drive, ":"))
	if !wsl || drive == "" {
		return path
	}
	parts := []string{"/mnt", drive}
	for _, p := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		if p != "." {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

var pmInstalled = pm.Installed

// PM selected driver binary without installing, an explicit override is never replaced
func resolveCuaDriverCmd(override *string) (string, error) {
	configured := os.Getenv(cuaDriverCmdEnv)
	if override != nil {
		configured = *override
	}
	configured = pystr.Strip(configured)
	if configured != "" {
		expanded := pypath.Expanduser(configured)
		resolved, ok := pyshutil.Which(expanded, nil)
		if ok && hasPathSeparator(expanded) {
			return expanded, nil
		}
		if ok {
			return resolved, nil
		}
		return "", nil
	}
	installed, err := pmInstalled(context.Background(), "cua-driver", false)
	if err != nil {
		return "", err
	}
	if installed != nil && installed.Binary != "" {
		return installed.Binary, nil
	}
	return "", nil
}

// True when PM or an explicit override selects a local driver
func cuaDriverBinaryAvailable() (bool, error) {
	cmd, err := resolveCuaDriverCmd(nil)
	return cmd != "", err
}

// Install guidance for a missing driver
func cuaDriverInstallHint() string {
	return "cua-driver is not installed. Install the pinned driver with:\n  icebath computer-use install\n" +
		"Or run `icebath tools` and enable the Computer Use toolset to install it automatically."
}

// Args with --no-overlay appended when configured and supported by the driver
func mcpArgsWithOverlayFlag(env *tools.Env, args []string, driverCmd string) []string {
	out := slices.Clone(args)
	if out == nil {
		out = []string{}
	}
	if cuaNoOverlay(env) && cuaDriverSupportsNoOverlay(env, driverCmd) {
		out = append(out, "--no-overlay")
	}
	return out
}

var noOverlayCache struct {
	sync.Mutex
	valid bool
	cmd   string
	value bool
}

// Whether driver help mentions --no-overlay, probed once for the most recent command
var cuaDriverSupportsNoOverlay = func(env *tools.Env, driverCmd string) bool {
	noOverlayCache.Lock()
	if noOverlayCache.valid && noOverlayCache.cmd == driverCmd {
		v := noOverlayCache.value
		noOverlayCache.Unlock()
		return v
	}
	noOverlayCache.Unlock()
	value := false
	if proc, err := runDriver(env, driverCmd, 3.0, "--help"); err == nil {
		value = strings.Contains(proc.Stdout+proc.Stderr, "--no-overlay")
	}
	noOverlayCache.Lock()
	noOverlayCache.valid, noOverlayCache.cmd, noOverlayCache.value = true, driverCmd, value
	noOverlayCache.Unlock()
	return value
}

// Clears the single entry no overlay probe cache
func clearNoOverlayCache() {
	noOverlayCache.Lock()
	noOverlayCache.valid = false
	noOverlayCache.Unlock()
}

// Command and args spawning the stdio MCP server as the driver manifest reports, mcp on any discovery failure
func resolveMCPInvocation(env *tools.Env, driverCmd string, timeout float64) (string, []string) {
	manifest := driverJSON(env, driverCmd, timeout, "manifest")
	var invocation any
	if manifest != nil {
		invocation, _ = manifest.Get("mcp_invocation")
	}
	args, valid := validMCPArgs(invocation)
	command := ""
	if valid {
		if inv, ok := invocation.(*pyjson.Object); ok {
			raw, _ := inv.Get("command")
			command, _ = raw.(string)
		}
	} else {
		args = slices.Clone(cuaDriverArgs)
	}
	if command != "" {
		command = wslWindowsPathToPosix(command)
	}
	if command == "" || !hasPathSeparator(command) {
		command = driverCmd
	}
	return command, mcpArgsWithOverlayFlag(env, args, command)
}

// Why a parsed manifest fails the 0.20 contract, empty when it passes
func manifestContractReason(manifest *pyjson.Object) (string, error) {
	if manifest == nil {
		return "driver manifest is missing or invalid", nil
	}
	raw, _ := manifest.Get("binary_version")
	text := ""
	if pyrepr.Truthy(raw) {
		text = pyrepr.Str(raw)
	}
	m := cuaSemverRE.FindStringSubmatch(pystr.Strip(text))
	if m == nil {
		return "driver manifest does not report a semantic version", nil
	}
	older := false
	for i := range 3 {
		n, _ := pyint.Parse(m[i+1])
		if c := n.Cmp(big.NewInt(int64(cuaDriverRuntimeContractMin[i]))); c != 0 {
			older = c < 0
			break
		}
	}
	if older {
		return "Icebath computer use requires cua-driver 0.20.0 or newer", nil
	}
	inv, _ := manifest.Get("mcp_invocation")
	if args, ok := validMCPArgs(inv); !ok || len(args) == 0 {
		return "driver manifest does not provide an MCP launch command", nil
	}
	advertised := map[string]map[string]bool{}
	subs, _ := manifest.Get("subcommands")
	subList, err := pyIterItems(subs)
	if err != nil {
		return "", err
	}
	for _, c := range subList {
		cmd, ok := c.(*pyjson.Object)
		if !ok {
			continue
		}
		nameRaw, _ := cmd.Get("name")
		name, ok := nameRaw.(string)
		if !ok {
			continue
		}
		set := map[string]bool{}
		argsRaw, _ := cmd.Get("args")
		argList, err := pyIterItems(argsRaw)
		if err != nil {
			return "", err
		}
		for _, a := range argList {
			arg, ok := a.(*pyjson.Object)
			if !ok {
				continue
			}
			if n, ok := arg.Vals["name"].(string); ok {
				set[n] = true
			}
		}
		advertised[name] = set
	}
	var missing []string
	for _, req := range cuaDriverRuntimeContractArgs {
		flags := slices.Clone(req.flags)
		slices.Sort(flags)
		for _, f := range flags {
			if !advertised[req.verb][f] {
				missing = append(missing, req.verb+" "+f)
			}
		}
	}
	if len(missing) > 0 {
		return "driver manifest is missing: " + strings.Join(missing, ", "), nil
	}
	return "", nil
}

// Items a for loop over value or an empty list visits, the TypeError for a non iterable truthy value
func pyIterItems(value any) ([]any, error) {
	if !pyrepr.Truthy(value) {
		return nil, nil
	}
	switch v := value.(type) {
	case []any:
		return v, nil
	case string:
		out := []any{}
		for _, r := range pystr.Runes(v) {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, len(v.Keys))
		for i, k := range v.Keys {
			out[i] = k
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(value))
}

// Readiness of a local driver for the 0.20 integration with its version and the failing reason
func cuaDriverRuntimeContractStatus(env *tools.Env, binary string) (*pyjson.Object, error) {
	resolved := binary
	if resolved == "" {
		var err error
		if resolved, err = resolveCuaDriverCmd(nil); err != nil {
			return nil, err
		}
	}
	var version any
	reason := "cua-driver is not installed"
	if resolved != "" {
		timeout := 5.0
		if cuPlatform == "win32" {
			timeout = 15.0
		}
		result, err := runDriver(env, resolved, timeout, "manifest")
		if err != nil {
			var se *spawnError
			var te *subprocessTimeout
			if !errors.As(err, &se) && !errors.As(err, &te) {
				return nil, err
			}
			result, reason = nil, "manifest check failed: "+err.Error()
		}
		if result != nil && result.Returncode != 0 {
			text := result.Stderr
			if text == "" {
				text = result.Stdout
			}
			if text == "" {
				text = "manifest command failed"
			}
			lines := pystr.Splitlines(pystr.Strip(text), false)
			if len(lines) == 0 {
				return nil, errors.New("list index out of range")
			}
			last := []rune(lines[len(lines)-1])
			result, reason = nil, string(last[:min(len(last), 200)])
		}
		if result != nil {
			manifest := jsonObject(result.Stdout)
			var err error
			if reason, err = manifestContractReason(manifest); err != nil {
				return nil, err
			}
			if manifest != nil {
				raw, _ := manifest.Get("binary_version")
				text := ""
				if pyrepr.Truthy(raw) {
					text = pyrepr.Str(raw)
				}
				if v := pystr.Strip(text); v != "" {
					version = v
				}
			}
		}
	}
	out := pyjson.NewObject()
	out.Set("ready", reason == "")
	if resolved != "" {
		out.Set("binary", resolved)
	} else {
		out.Set("binary", nil)
	}
	out.Set("version", version)
	out.Set("reason", reason)
	return out, nil
}

// Upstream release polling is retired, PM owns the pin
func cuaDriverUpdateCheck() *pyjson.Object { return nil }
