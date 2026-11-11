package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	xterm "github.com/charmbracelet/x/term"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Options that change execution or need frontend facilities the authority does not expose
var gatewayChatUnsupported = []string{"image", "worktree", "w", "no_restore_cwd", "run_budget", "verbose", "compact"}

// Where each refused option lives now
var gatewayChatRelocated = map[string]string{
	"image":                               "`icebath --tui` or the Desktop app to attach the image",
	"worktree":                            "`icebath --tui -w`",
	"w":                                   "`icebath --tui -w`",
	"no_restore_cwd":                      "`--in <dir>` (the gateway keeps the session's frozen cwd)",
	"run_budget":                          "`agent.run_budget_seconds` in config.yaml",
	"verbose":                             "`icebath logs --follow`, or `icebath chat --tui -v`",
	"compact":                             "`display.compact: true` in config.yaml",
	"create-if-missing without -c <name>": "`-c <name> --create-if-missing`",
}

// Runnable bypass launch example the safe mode refusal prints
const gatewayChatSafeModeExample = `icebath chat --safe-mode --provider openrouter --model anthropic/claude-sonnet-4 -q "hello"`

// Owner connection of one chat launch, closed when the launch ends
type gatewayChatConn interface {
	gatewayChatPeer
	Close() error
}

// Attaches to this home's owner the way the chat launch does
func connectGatewayChat(ctx context.Context) (gatewayChatConn, error) {
	client, err := gateway.ConnectGateway(ctx)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Whether the launch freezes code defaults and reads no profile, by flag or by its environment opt in
func BypassLaunch(args map[string]any) bool {
	env := gatewaycli.EnvLaunchFlags()
	for _, name := range []string{"safe_mode", "ignore_user_config"} {
		if _, set := env.Get(name); pyrepr.Truthy(args[name]) || set {
			return true
		}
	}
	return false
}

// Title of a named continue, absent when a resume is also given
func gatewayContinueTitle(args map[string]any) (string, bool) {
	name, ok := args["continue_last"].(string)
	if ok && !pyrepr.Truthy(args["resume"]) {
		return name, true
	}
	return "", false
}

// Whether the launch continues the latest session, a bare continue or a resume of the latest keyword
func gatewayWantsLatest(args map[string]any) bool {
	if resume, ok := args["resume"].(string); ok {
		return pystr.Lower(pystr.Strip(resume)) == "latest"
	}
	return args["continue_last"] == true
}

// Refuses unsupported options naming where each went, and a bypass launch without an explicit model
func gatewayValidateOptions(args map[string]any) error {
	var unsupported []string
	for _, name := range gatewayChatUnsupported {
		if pyrepr.Truthy(args[name]) {
			unsupported = append(unsupported, name)
		}
	}
	if _, titled := gatewayContinueTitle(args); pyrepr.Truthy(args["create_if_missing"]) && !titled {
		unsupported = append(unsupported, "create-if-missing without -c <name>")
	}
	if len(unsupported) > 0 {
		spell := func(name string) string {
			if pystr.Len(name) == 1 {
				return "-" + name
			}
			return "--" + strings.ReplaceAll(name, "_", "-")
		}
		flags := make([]string, len(unsupported))
		var where strings.Builder
		for i, name := range unsupported {
			flags[i] = spell(name)
			where.WriteString(fmt.Sprintf("\n  %s: use %s", spell(name), gatewayChatRelocated[name]))
		}
		return &gateway.GatewayClientError{Msg: fmt.Sprintf("Unsupported gateway CLI options: %s. No local fallback or policy changes were made.%s",
			strings.Join(flags, ", "), where.String())}
	}
	resuming := pyrepr.Truthy(args["resume"]) || pyrepr.Truthy(args["continue_last"])
	if BypassLaunch(args) && !pyrepr.Truthy(args["model"]) && !resuming {
		return &gateway.GatewayClientError{Msg: "--safe-mode / --ignore-user-config (or ICEBATH_SAFE_MODE / ICEBATH_IGNORE_USER_CONFIG) " +
			"read no profile default: pass --model explicitly." + "\n  Example: " + gatewayChatSafeModeExample}
	}
	return nil
}

// Resolved caller working directory, the in flag winning over the process cwd
func gatewayCallerCwd(args map[string]any) (string, error) {
	dir := ""
	if pyrepr.Truthy(args["in_dir"]) {
		dir = pyrepr.Str(args["in_dir"])
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	return pypath.Resolve(pypath.Expanduser(dir))
}

// Workspace identity of the cwd for a continue, its git root or the cwd itself
func gatewayWorkspaceKey(cwd string) (string, error) {
	previous, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if err := os.Chdir(cwd); err != nil {
		return "", err
	}
	defer func() { _ = os.Chdir(previous) }()
	return resolveWorkspaceKey(), nil
}

// session.resume with the launch key re-supplied, a key conflict refused with the mismatch text
func gatewayResume(ctx context.Context, client gatewayChatPeer, args map[string]any, params map[string]any) (map[string]any, error) {
	key := ""
	if pyrepr.Truthy(args["api_key"]) {
		key = pyrepr.Str(args["api_key"])
		params["api_key"] = key
	}
	raw, err := client.RPCTimeout(ctx, "session.resume", params, 0)
	if err != nil {
		var refused *gateway.GatewayClientError
		if key != "" && errors.As(err, &refused) && refused.Msg == "admission_conflict" {
			return nil, &gateway.GatewayClientError{Msg: gateway.ResumeKeyMismatch}
		}
		return nil, err
	}
	return gatewayAnswerMap(raw)
}

// Message of a gateway refusal, empty when the error is not one
func gatewayRefusalText(err error) (string, bool) {
	var refused *gateway.GatewayClientError
	if errors.As(err, &refused) {
		return refused.Msg, true
	}
	return "", false
}

// This terminal breadcrumb session when the owner still has it, else the owner latest CLI session of this workspace first
func gatewayResumeLatest(ctx context.Context, client gatewayChatPeer, args map[string]any) (map[string]any, error) {
	if args["continue_last"] == true {
		if crumb, ok := ReadBreadcrumb(ctx)["session_id"].(string); ok && crumb != "" {
			snapshot, err := gatewayResume(ctx, client, args, map[string]any{"session_id": crumb})
			if err == nil {
				return snapshot, nil
			}
			if msg, refused := gatewayRefusalText(err); !refused || !gatewaycli.ResumeRefusals[msg] {
				return nil, err
			}
		}
	}
	cwd, err := gatewayCallerCwd(args)
	if err != nil {
		return nil, err
	}
	workspace, err := gatewayWorkspaceKey(cwd)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"latest": "cli"}
	if workspace != "" {
		params["workspace"] = workspace
	}
	snapshot, err := gatewayResume(ctx, client, args, params)
	if err != nil {
		if msg, refused := gatewayRefusalText(err); refused && msg == "not_found" {
			return nil, &gateway.GatewayClientError{Msg: "No previous CLI session to continue. Start a new one with `icebath`, " +
				"or list sessions with `icebath sessions list`."}
		}
		return nil, err
	}
	return snapshot, nil
}

// Resumes the named session by id then by title, refusing with how to list or create it
func gatewayResumeNamed(ctx context.Context, client gatewayChatPeer, args map[string]any, name string) (map[string]any, error) {
	snapshot, err := gatewayResume(ctx, client, args, map[string]any{"session_id": name})
	if err == nil {
		return snapshot, nil
	}
	if msg, refused := gatewayRefusalText(err); !refused || msg != "not_found" {
		return nil, err
	}
	snapshot, err = gatewayResume(ctx, client, args, map[string]any{"title": name})
	if err == nil {
		return snapshot, nil
	}
	if msg, refused := gatewayRefusalText(err); !refused || msg != "not_found" {
		return nil, err
	}
	return nil, &gateway.GatewayClientError{Msg: fmt.Sprintf("No session found matching '%s'. Use 'icebath sessions list' to see available "+
		"sessions, or pass -c <name> --create-if-missing to start a new session with that title.", name)}
}

// Creates a session under the launch policy the owner create contract advertises
func gatewayCreateSession(ctx context.Context, client gatewayChatPeer, args map[string]any, description map[string]any,
	oneshot bool, createIfMissing bool, title string, oneshotPrompt bool) (map[string]any, error) {
	contract, _ := gatewayGetOr(description, "session_create", map[string]any{}).(map[string]any)
	sources, _ := gatewayGetOr(contract, "sources", []any{}).([]any)
	has := func(list []any, name string) bool {
		for _, item := range list {
			if pyrepr.Equal(item, name) {
				return true
			}
		}
		return false
	}
	requested := "cli"
	if pyrepr.Truthy(args["source"]) {
		requested = pyrepr.Str(args["source"])
	} else if oneshot && has(sources, "oneshot") {
		requested = "oneshot"
	}
	source := agent.CreateSurface(requested)
	if !has(sources, source) {
		return nil, &gateway.GatewayClientError{Msg: "Gateway does not support source " + pyrepr.Repr(source)}
	}
	parameters, _ := gatewayGetOr(contract, "parameters", []any{}).([]any)
	policy := gatewaycli.RequestedPolicy(args)
	policy.Delete("source")
	if createIfMissing {
		policy.Set("title", title)
	}
	env := gatewaycli.EnvLaunchFlags()
	for _, key := range env.Keys {
		policy.Set(key, env.Vals[key])
	}
	clientEnv, err := gatewaycli.ClientEnvPolicy(ctx, parameters)
	if err != nil {
		return nil, err
	}
	for _, key := range sortedMapKeys(clientEnv) {
		policy.Set(key, clientEnv[key])
	}
	cwd, err := gatewayCallerCwd(args)
	if err != nil {
		return nil, err
	}
	switch {
	case has(parameters, "cwd"):
		policy.Set("cwd", cwd)
	case pyrepr.Truthy(args["in_dir"]):
		return nil, &gateway.GatewayClientError{Msg: "Gateway does not support --in / caller cwd; update the gateway"}
	default:
		fmt.Fprintln(stderrWriter(), "Warning: this gateway cannot preserve caller cwd; it uses its configured execution directory.")
	}
	var missing []string
	for _, key := range policy.Keys {
		if !has(parameters, key) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &gateway.GatewayClientError{Msg: "Gateway does not support creation options: " + strings.Join(missing, ", ")}
	}
	params := map[string]any{"request_id": uuidHex(), "source": source}
	for _, key := range policy.Keys {
		params[key] = policy.Vals[key]
	}
	raw, err := client.RPCTimeout(ctx, "session.create", params, 0)
	if err != nil {
		return nil, err
	}
	snapshot, err := gatewayAnswerMap(raw)
	if err != nil {
		return nil, err
	}
	info, _ := snapshot["info"].(map[string]any)
	if warning := info["config_warning"]; pyrepr.Truthy(warning) && !oneshotPrompt {
		fmt.Fprintln(stderrWriter(), "Warning: "+pyrepr.Str(warning))
	}
	return snapshot, nil
}

// Whether standard input and output are both terminals
func gatewayStdioTTY() bool {
	return xterm.IsTerminal(os.Stdin.Fd()) && xterm.IsTerminal(os.Stdout.Fd())
}

// Attaches to the owner, resumes or creates the session and runs the chat view on it
func runGatewayChat(ctx context.Context, args map[string]any, emitter *StreamJSONEmitter,
	connect func(ctx context.Context) (gatewayChatConn, error)) (int, error) {
	query := ""
	if pyrepr.Truthy(args["query"]) {
		query = pyrepr.Str(args["query"])
	} else if pyrepr.Truthy(args["q"]) {
		query = pyrepr.Str(args["q"])
	}
	oneshotText, oneshotPrompt := args["oneshot"].(string)
	if oneshotPrompt {
		query = oneshotText
	}
	quiet := pyrepr.Truthy(args["quiet"]) || oneshotText != "" || emitter != nil
	oneshot := oneshotText != "" || pyrepr.Truthy(args["oneshot_exit"]) || quiet || (query != "" && !gatewayStdioTTY())
	client, err := connect(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	rawDescription, err := client.RPCTimeout(ctx, "runtime.describe", map[string]any{}, 0)
	if err != nil {
		return 0, err
	}
	description, err := gatewayAnswerMap(rawDescription)
	if err != nil {
		return 0, err
	}
	title, titled := gatewayContinueTitle(args)
	createIfMissing := titled && title != "" && pyrepr.Truthy(args["create_if_missing"])
	latest := gatewayWantsLatest(args)
	var snapshot map[string]any
	switch {
	case latest:
		if snapshot, err = gatewayResumeLatest(ctx, client, args); err == nil {
			err = gatewaycli.CheckResumePolicy(ctx, args, snapshot)
		}
	case pyrepr.Truthy(args["resume"]) || (titled && title != "" && !createIfMissing):
		name := title
		if pyrepr.Truthy(args["resume"]) {
			name = pyrepr.Str(args["resume"])
		}
		if snapshot, err = gatewayResumeNamed(ctx, client, args, name); err == nil {
			err = gatewaycli.CheckResumePolicy(ctx, args, snapshot)
		}
	default:
		snapshot, err = gatewayCreateSession(ctx, client, args, description, oneshot, createIfMissing, title, oneshotPrompt)
	}
	if err != nil {
		return 0, err
	}
	sessionID := pyrepr.Str(requireKey(snapshot, "stored_session_id"))
	if !oneshotPrompt {
		fmt.Fprintln(stderrWriter(), "Session: "+sessionID)
	}
	if !oneshot {
		WriteBreadcrumb(ctx, sessionID, "")
	}
	if emitter != nil {
		emitter.BindSession(sessionID)
	}
	usageFile := ""
	if pyrepr.Truthy(args["usage_file"]) {
		usageFile = pyrepr.Str(args["usage_file"])
	}
	view := newGatewayChatView(client, snapshot, quiet, emitter, usageFile)
	if pyrepr.Truthy(args["api_key"]) {
		view.launchAPIKey = pyrepr.Str(args["api_key"])
	}
	view.unattended = oneshotPrompt
	view.resumeFooter = oneshot && !quiet
	if (pyrepr.Truthy(args["resume"]) || (titled && title != "") || latest) && !quiet {
		messages, _ := gatewayGetOr(snapshot, "messages", []any{}).([]any)
		for _, raw := range messages {
			row, _ := raw.(map[string]any)
			content, isStr := row["content"].(string)
			if (pyrepr.Equal(row["role"], "user") || pyrepr.Equal(row["role"], "assistant")) && isStr && !gatewayModelOnlyRow(row) {
				fmt.Fprintln(console().Out, pyrepr.Str(row["role"])+": "+content)
			}
		}
	}
	return view.Run(ctx, query, oneshot)
}

// Prints a refused resume as the usage error every classic path exits 2 with, nil for any other failure
func ExitOnResumeRefusal(err error) error {
	msg, refused := gatewayRefusalText(err)
	if refused && (strings.HasPrefix(msg, "Resume retains gateway session policy; creation overrides are unsupported on resume.") ||
		strings.HasPrefix(msg, gateway.ResumeKeyMismatch)) {
		fmt.Fprintln(stderrWriter(), "Error: "+msg)
		return &pm.SystemExit{Code: 2}
	}
	return nil
}

// Records this chat client in the process ledger and warns once when another install shares the home
func gatewayRegisterTerminalProcess(ctx context.Context) {
	tools.RegisterSelf(ctx, "cli", "", nil)
	if warning := profiles.SharedProfileWarning(ctx, home.Dir(ctx), ""); warning != "" {
		fmt.Fprintln(stderrWriter(), "Warning: "+warning)
	}
}

// Whether the error is the OSError, TimeoutError or websocket failure of a lost owner connection
func gatewayTransportError(err error) bool {
	var pathErr *os.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	var netErr net.Error
	var closeErr *websocket.CloseError
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) || errors.As(err, &errno) ||
		errors.As(err, &netErr) || errors.As(err, &closeErr) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, websocket.ErrBadHandshake) ||
		errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, websocket.ErrReadLimit) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Runs fn with stdout carried to stderr, the redirect_stdout of setup guidance under a machine readable stdout
func gatewayStdoutToStderr(fn func() (bool, error)) (bool, error) {
	stderr := stderrWriter()
	base := console()
	redirected := &cliui.Console{In: base.In, Out: stderr, Getenv: base.Getenv, StdinTTY: base.StdinTTY, StdoutTTY: base.StdoutTTY,
		TermSize: base.TermSize, MaskedSecretPrompt: base.MaskedSecretPrompt, OpenScreen: base.OpenScreen}
	restore := SetStreams(redirected, stderr)
	previous := os.Stdout
	if f, ok := stderr.(*os.File); ok {
		os.Stdout = f
	}
	defer func() {
		os.Stdout = previous
		restore()
	}()
	return fn()
}

// Reads the query file the way main did, undecodable bytes replaced and a dash reading stdin
func gatewayReadQueryFile(ctx context.Context, path string) (string, error) {
	if path == "-" {
		type read struct {
			data []byte
			err  error
		}
		done := make(chan read, 1)
		go func() {
			data, err := io.ReadAll(os.Stdin)
			done <- read{data, err}
		}()
		select {
		case r := <-done:
			if r.err != nil {
				return "", r.err
			}
			return pystr.TranslateNewlines(pystr.DecodeUTF8Replace(r.data)), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return pystr.TranslateNewlines(pystr.DecodeUTF8SigReplace(data)), nil
}

// Launches a classic or one shot chat through the canonical gateway, ensure being the frontend provider check
func LaunchFromArgs(ctx context.Context, args map[string]any, ensure func() (bool, error)) int {
	return launchFromArgs(ctx, args, ensure, connectGatewayChat)
}

// Launches from keyword options, the namespace being the options themselves
func LaunchFromKwargs(ctx context.Context, options map[string]any, ensure func() (bool, error)) int {
	return LaunchFromArgs(ctx, options, ensure)
}

// The launch with its owner connector, every failed start closing the stream json protocol with a result
func launchFromArgs(ctx context.Context, args map[string]any, ensure func() (bool, error),
	connect func(ctx context.Context) (gatewayChatConn, error)) int {
	var emitter *StreamJSONEmitter
	if format, ok := args["output_format"]; ok && pyrepr.Equal(format, "stream-json") {
		model := ""
		if pyrepr.Truthy(args["model"]) {
			model = pyrepr.Str(args["model"])
		}
		emitter = NewStreamJSONEmitter(model, "")
	}
	failed := func(message string, code int) int {
		fmt.Fprintln(stderrWriter(), "Error: "+message)
		if emitter != nil {
			return emitter.EmitResult(map[string]any{"failed": true, "error": message}, "", code)
		}
		return code
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupts := make(chan os.Signal, 1)
	interrupted := make(chan struct{})
	code, err := func() (int, error) {
		if err := gatewayValidateOptions(args); err != nil {
			return 0, err
		}
		if pyrepr.Truthy(args["list_tools"]) || pyrepr.Truthy(args["list_toolsets"]) {
			return printToolListing(ctx, args)
		}
		gatewayRegisterTerminalProcess(ctx)
		if emitter == nil {
			configured, err := ensure()
			var exit *pm.SystemExit
			if errors.As(err, &exit) {
				return exit.Code, nil
			}
			if err != nil {
				return 0, err
			}
			if !configured {
				return 0, nil
			}
		} else {
			configured, err := gatewayStdoutToStderr(ensure)
			var exit *pm.SystemExit
			if errors.As(err, &exit) {
				configured, err = false, nil
			}
			if err != nil {
				return 0, err
			}
			if !configured {
				return failed("credentials or agent init failed", 1), nil
			}
		}
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
		go func() {
			select {
			case <-interrupts:
				close(interrupted)
				cancel()
			case <-runCtx.Done():
			}
		}()
		if queryFile, ok := args["query_file"]; ok && pyrepr.Truthy(queryFile) {
			path := pyrepr.Str(queryFile)
			query, err := gatewayReadQueryFile(runCtx, path)
			if err != nil {
				if runCtx.Err() != nil {
					return 0, runCtx.Err()
				}
				return failed(fmt.Sprintf("cannot read --query-file %s: %s", path, pypath.OSErrorText(err)), 2), nil
			}
			args["query"] = query
			if pystr.Strip(query) == "" {
				return failed(fmt.Sprintf("--query-file %s is empty", path), 2), nil
			}
		}
		if !pyrepr.Truthy(args["query"]) && !pyrepr.Truthy(args["q"]) && !pyrepr.Truthy(args["oneshot"]) && !xterm.IsTerminal(os.Stdin.Fd()) {
			return 0, &gateway.GatewayClientError{Msg: "Noninteractive chat requires --query or --oneshot"}
		}
		return runGatewayChat(runCtx, args, emitter, connect)
	}()
	select {
	case <-interrupted:
		fmt.Fprintln(stderrWriter(), "Detached; accepted work continues at the gateway.")
		if emitter != nil {
			return emitter.EmitResult(map[string]any{"interrupted": true}, "", 130)
		}
		return 130
	default:
	}
	if err == nil {
		return code
	}
	if msg, refused := gatewayRefusalText(err); refused {
		exitCode := 1
		if strings.Contains(msg, "Unsupported") || strings.Contains(msg, "unsupported") {
			exitCode = 2
		}
		return failed(msg, exitCode)
	}
	if gatewayTransportError(err) {
		return failed("Gateway connection/read failed; no local fallback", 1)
	}
	panic(err)
}
