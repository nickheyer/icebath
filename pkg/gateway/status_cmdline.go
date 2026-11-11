package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Seams over the three command line sources, procfs, the process table and ps
var (
	statusProcRead       = os.ReadFile
	statusPsutilCmdline  = proc.Cmdline
	statusPsCommand      = psCommandLive
	hostGatewayServesFor = hostGatewayServesHome
)

// Runs ps for one pid with a five second timeout, returning exit code and stdout
func psCommandLive(pid int) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", fmt.Sprint(pid), "-o", "command=")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return 0, "", ctx.Err()
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), pystr.DecodeUTF8Replace(out.Bytes()), nil
	}
	if err != nil {
		return 0, "", err
	}
	return 0, pystr.DecodeUTF8Replace(out.Bytes()), nil
}

// Command line of pid as one string from procfs, the process table, then ps
func readProcessCmdlineLive(pid int) (string, bool) {
	if raw, err := statusProcRead(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && len(raw) > 0 {
		text := strings.ToValidUTF8(string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '})), "")
		return pystr.Strip(text), true
	}
	if parts, err := statusPsutilCmdline(pid); err == nil && len(parts) > 0 {
		return strings.Join(parts, " "), true
	}
	if !statusIsWindows {
		if code, out, err := statusPsCommand(pid); err == nil && code == 0 && pystr.Strip(out) != "" {
			return pystr.Strip(out), true
		}
	}
	return "", false
}

// Tokens of a command line split like shlex non posix, plain whitespace when the quoting is unbalanced
func gatewayRawTokens(command string) []string {
	raw, err := pyshlex.SplitNonPOSIX(command)
	if err != nil {
		return pystr.SplitWhitespace(command, -1)
	}
	return raw
}

func casedTokens(raw []string) []string {
	out := make([]string, len(raw))
	for i, t := range raw {
		out[i] = strings.ReplaceAll(strings.Trim(t, `"'`), `\`, "/")
	}
	return out
}

func lowerAll(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = pystr.Lower(t)
	}
	return out
}

func tokenBasenames(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = t[strings.LastIndex(t, "/")+1:]
	}
	return out
}

// Gateway lifecycle subcommand named by a command line, false unless an icebath gateway entrypoint runs it
func gatewayCommandSubcommand(command string) (string, bool) {
	if command == "" {
		return "", false
	}
	cased := casedTokens(gatewayRawTokens(command))
	tokens := lowerAll(cased)
	if len(tokens) == 0 {
		return "", false
	}
	basenames := tokenBasenames(tokens)
	if CommandLineRunsInlineSource(cased) {
		argv, ok := InlineBootstrapArgv(cased)
		if !ok {
			return "", false
		}
		tokens = lowerAll(argv)
		basenames = tokenBasenames(tokens)
	}
	if basenames[0] == "osascript" || restartWatcherEntryIndex(tokens) >= 0 {
		return "", false
	}
	for _, t := range tokens {
		if t == "gateway/run.py" || strings.HasSuffix(t, "/gateway/run.py") {
			return "run", true
		}
	}
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == "-m" && tokens[i+1] == "gateway.run" {
			return "run", true
		}
	}
	if runsGatewayModuleEntry(tokens) {
		return "run", true
	}
	for _, b := range basenames {
		if b == "desktop-gateway.py" || b == "icebath-gateway" || b == "icebath-gateway.exe" {
			return "run", true
		}
	}
	joined := strings.Join(tokens, " ")
	entry := strings.Contains(joined, "icebath_cli.main") || strings.Contains(joined, "icebath_cli/main.py")
	for _, b := range basenames {
		entry = entry || b == "icebath" || b == "icebath.exe"
	}
	if !entry {
		return "", false
	}
	var filtered []string
	skipNext := false
	for _, token := range tokens {
		switch {
		case skipNext:
			skipNext = false
		case token == "--profile" || token == "-p":
			skipNext = true
		case !strings.HasPrefix(token, "--profile=") && !strings.HasPrefix(token, "-p="):
			filtered = append(filtered, token)
		}
	}
	for i, token := range filtered {
		if token == "gateway" {
			if i+1 < len(filtered) {
				return filtered[i+1], true
			}
			return "run", true
		}
	}
	return "", false
}

// Gateway subcommand a command line eventually launches, peeling an inline source wrapper token wise
func GatewaySpawnIntentSubcommand(command string) (string, bool) {
	if direct, ok := gatewayCommandSubcommand(command); ok || command == "" {
		return direct, ok
	}
	raw := gatewayRawTokens(command)
	start := restartWatcherEntryIndex(lowerAll(casedTokens(raw))) + 1
	if flagIndex, ok := InlineSourceFlagIndex(casedTokens(raw)); ok {
		start = flagIndex + 2
	} else if start == 0 {
		return "", false
	}
	for i := start; i < len(raw); i++ {
		if nested, ok := gatewayCommandSubcommand(strings.Join(raw[i:], " ")); ok {
			return nested, true
		}
	}
	return "", false
}

// Whether a command line is a real gateway run process
func LooksLikeGatewayCommandLine(command string) bool {
	sub, ok := gatewayCommandSubcommand(command)
	return ok && sub == "run"
}

// Whether a command line can host the gateway runtime, run or the in process restart fallback
func LooksLikeGatewayRuntimeCommandLine(command string) bool {
	sub, ok := gatewayCommandSubcommand(command)
	return ok && (sub == "run" || sub == "restart")
}

// Profile id of a profiles child home, false for the root or any other home
func profileNameForHomeDir(profileHome string) (string, bool) {
	if pypath.PathName(pypath.PathParent(profileHome)) == "profiles" {
		return pypath.PathName(profileHome), true
	}
	return "", false
}

// Lower cased -p or --profile argument of a case preserving command line, false when absent
func ProfileFlagValue(command string) (string, bool) {
	split := pystr.SplitWhitespace(command, -1)
	src := split
	if argv, ok := InlineBootstrapArgv(split); ok && len(argv) > 0 {
		src = argv
	}
	tokens := lowerAll(src)
	for i, tok := range tokens {
		if strings.HasPrefix(tok, "--profile=") {
			return tok[len("--profile="):], true
		}
		if (tok == "-p" || tok == "--profile") && i+1 < len(tokens) {
			return tokens[i+1], true
		}
	}
	return "", false
}

const pySpaceClass = `\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`
const pySpaceClassNet = `\t\n\v\f\r\x1c-\x1f \u0085   -     　`

var homeAssignmentRe = regexp.MustCompile(`(?:^|[` + pySpaceClass + `])icebath_home=(?:"([^"]*)"|'([^']*)'|([^` + pySpaceClass + `]+))`)

// Values of every token bounded ICEBATH_HOME assignment in a lower cased command line, trailing slash stripped
func HermesHomeAssignments(command string) []string {
	var out []string
	for _, m := range homeAssignmentRe.FindAllStringSubmatchIndex(command, -1) {
		for g := 1; g <= 3; g++ {
			if m[2*g] >= 0 {
				out = append(out, strings.TrimRight(command[m[2*g]:m[2*g+1]], "/"))
				break
			}
		}
	}
	return out
}

// Whether a lower cased command line assigns exactly home, a space holding unquoted home included
func CommandLineNamesHermesHome(commandLC, homeLC string) bool {
	for _, a := range HermesHomeAssignments(commandLC) {
		if a == homeLC {
			return true
		}
	}
	re := regexp2.MustCompile(`(?:^|[`+pySpaceClassNet+`])icebath_home=`+regexp2.Escape(homeLC)+`/?(?=[`+pySpaceClassNet+`]|$)`, regexp2.None)
	ok, err := re.MatchString(commandLC)
	return err == nil && ok
}

// Whether a gateway command line belongs to the profile home by flag or ICEBATH_HOME assignment
func commandLineBelongsToProfile(command, profileHome string) bool {
	commandLC := strings.ReplaceAll(pystr.Lower(command), `\`, "/")
	profileName, named := profileNameForHomeDir(profileHome)
	homeLC := strings.TrimRight(strings.ReplaceAll(pystr.Lower(pypath.PathStr(profileHome)), `\`, "/"), "/")
	flag, hasFlag := ProfileFlagValue(command)
	if named && profileName != "default" {
		if hasFlag && flag == pystr.Lower(profileName) {
			return true
		}
		return CommandLineNamesHermesHome(commandLC, homeLC)
	}
	if hasFlag && flag != "default" {
		return false
	}
	return len(HermesHomeAssignments(commandLC)) == 0 || CommandLineNamesHermesHome(commandLC, homeLC)
}

// Whether the one host gateway with this pid serves the profile of home, false when the host is unknown
func hostGatewayServesHome(pid int, profileHome string) bool {
	owner := hostGateway()
	return owner != nil && owner.PID == pid && owner.Serves(hostProfileNameForHome(profileHome))
}

// Whether a live pid still identifies as the record's gateway, the record deciding when the command line is unreadable
func recordMatchesLiveGatewayPid(record *pyjson.Object, pid int, expectedHome string) bool {
	live, ok := statusReadProcessCmdline(pid)
	if !ok || live == "" {
		return recordLooksLikeGateway(record)
	}
	if !LooksLikeGatewayRuntimeCommandLine(live) {
		return false
	}
	if expectedHome != "" && hostGatewayServesFor(pid, expectedHome) {
		return true
	}
	return expectedHome == "" || commandLineBelongsToProfile(live, expectedHome)
}
