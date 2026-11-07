package googlemeet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

var (
	cliIn io.Reader = os.Stdin
	// Handler meet subcommands run with their parsed namespace, swapped by tests
	meetDispatch = meetCommand
	pmSyncVenv   = pm.SyncVenv
	pmEnsure     = pm.Ensure
	lookPath     = exec.LookPath
	// Runs a confirmed package manager command on the terminal, returning its exit code
	installRun = func(argv []string) (int, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	// Output of system_profiler SPAudioDataType decoded with replacement
	audioDevices = func() (string, error) {
		cmd := exec.Command("system_profiler", "SPAudioDataType")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		return strings.ToValidUTF8(string(out), "�"), err
	}
)

// Saved Google sign in state of the profile
func authStatePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "workspace", "meetings", "auth.json")
}

var meetSubcommandHelp = [][2]string{
	{"setup", "Preflight: playwright, chromium, auth"},
	{"install", "Install prerequisites (Python dependencies, Chromium, platform audio tools)"},
	{"auth", "Sign in to Google and save session state"},
	{"join", "Join a Meet URL"},
	{"status", "Print current Meet bot state"},
	{"transcript", "Print the scraped transcript"},
	{"say", "Speak text in an active realtime meeting"},
	{"stop", "Leave the current meeting"},
	{"node", "Manage remote meet node hosts (run/list/approve/remove/status/ping)"},
}

// Builds the meet subcommand tree under the cobra command the host passes
func registerMeetCLI(parser any) {
	root, ok := parser.(*cobra.Command)
	if !ok {
		panic(&AttributeError{fmt.Sprintf("'%s' object has no attribute 'add_subparsers'", pyTypeName(parser))})
	}
	run := func(cmd *cobra.Command, extra map[string]any) error {
		ns := map[string]any{"meet_command": cmd.Name()}
		maps.Copy(ns, extra)
		return exitErr(meetDispatch(ns))
	}
	sub := map[string]*cobra.Command{}
	for _, nh := range meetSubcommandHelp {
		c := &cobra.Command{Use: nh[0], Short: nh[1], Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return run(cmd, nil) }}
		sub[nh[0]] = c
		root.AddCommand(c)
	}
	var realtime, yes, headed bool
	sub["install"].Flags().BoolVar(&realtime, "realtime", false, "Also install realtime audio tools (pulseaudio-utils on Linux, BlackHole+ffmpeg on macOS). Uses sudo/brew, prompts before invoking either.")
	sub["install"].Flags().BoolVarP(&yes, "yes", "y", false, "Answer yes to all prompts (use with care; will run sudo apt-get or brew without asking).")
	sub["install"].RunE = func(cmd *cobra.Command, _ []string) error {
		return run(cmd, map[string]any{"realtime": realtime, "yes": yes})
	}
	var guestName, duration, mode, joinNode, sayNode string
	join := sub["join"]
	join.Use, join.Args = "join url", cobra.ExactArgs(1)
	join.Flags().StringVar(&guestName, "guest-name", "Icebath Agent", "")
	join.Flags().StringVar(&duration, "duration", "", "e.g. 30m, 2h, 90s")
	join.Flags().BoolVar(&headed, "headed", false, "show browser")
	join.Flags().StringVar(&mode, "mode", "transcribe", "transcribe (default, listen-only) or realtime (speak via OpenAI Realtime)")
	join.Flags().StringVar(&joinNode, "node", "", "remote node name, or 'auto' to use the sole registered node")
	optional := func(cmd *cobra.Command, flag, value string) any {
		if cmd.Flags().Changed(flag) {
			return value
		}
		return nil
	}
	join.RunE = func(cmd *cobra.Command, a []string) error {
		if mode != "transcribe" && mode != "realtime" {
			return fmt.Errorf("argument --mode: invalid choice: %s (choose from 'transcribe', 'realtime')", pyrepr.StrRepr(mode))
		}
		return run(cmd, map[string]any{"url": a[0], "guest_name": guestName, "duration": optional(cmd, "duration", duration),
			"headed": headed, "mode": mode, "node": optional(cmd, "node", joinNode)})
	}
	var last int
	sub["transcript"].Flags().IntVar(&last, "last", 0, "")
	sub["transcript"].RunE = func(cmd *cobra.Command, _ []string) error {
		ns := map[string]any{"last": nil}
		if cmd.Flags().Changed("last") {
			ns["last"] = last
		}
		return run(cmd, ns)
	}
	say := sub["say"]
	say.Use, say.Args = "say text", cobra.ExactArgs(1)
	say.Flags().StringVar(&sayNode, "node", "", "")
	say.RunE = func(cmd *cobra.Command, a []string) error {
		return run(cmd, map[string]any{"text": a[0], "node": optional(cmd, "node", sayNode)})
	}
	registerNodeCLI(sub["node"])
	root.RunE = func(*cobra.Command, []string) error {
		return exitErr(meetDispatch(map[string]any{"meet_command": nil}))
	}
}

// Registered meet handler over the parsed namespace, the exit code carried as an error
func meetHandler(args any) error {
	ns, ok := args.(map[string]any)
	if !ok {
		return &AttributeError{fmt.Sprintf("'%s' object has no attribute 'meet_command'", pyTypeName(args))}
	}
	return exitErr(meetCommand(ns))
}

// Runs the meet subcommand named in the namespace, returning the exit code
func meetCommand(ns map[string]any) int {
	sub := ns["meet_command"]
	if !pyrepr.Truthy(sub) {
		fmt.Fprintln(cliOut, "usage: icebath meet {setup,auth,join,status,transcript,say,stop,node}")
		return 2
	}
	ctx := context.Background()
	name, _ := sub.(string)
	switch name {
	case "setup":
		return cmdSetup(ctx)
	case "install":
		return cmdInstall(ctx, pyrepr.Truthy(ns["realtime"]), pyrepr.Truthy(ns["yes"]))
	case "auth":
		return cmdAuth(ctx)
	case "join":
		var duration *string
		if d, ok := ns["duration"].(string); ok {
			duration = &d
		}
		return cmdJoin(ctx, nsStr(ns, "url"), nsStr(ns, "guest_name"), duration, pyrepr.Truthy(ns["headed"]), nsStr(ns, "mode"), ns["node"])
	case "status":
		res, err := pmStatus(ctx)
		if err != nil {
			return uncaught(err)
		}
		return printResult(res)
	case "transcript":
		var last *int
		if n, ok := ns["last"].(int); ok {
			last = &n
		}
		return cmdTranscript(ctx, last)
	case "say":
		return cmdSay(ctx, nsStr(ns, "text"), ns["node"])
	case "stop":
		res, err := pmStop(ctx, "icebath meet stop")
		if err != nil {
			return uncaught(err)
		}
		return printResult(res)
	case "node":
		return nodeCommand(ns)
	}
	fmt.Fprintf(cliOut, "unknown subcommand: %s\n", pyrepr.Str(sub))
	return 2
}

// Reports an error escaping a subcommand on stderr with exit code one
func uncaught(err error) int {
	fmt.Fprintln(cliErr, err)
	return 1
}

// Line typed after the prompt, io.EOF when input ends before any character
func readLine(prompt string) (string, error) {
	fmt.Fprint(cliOut, prompt)
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := cliIn.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, buf[0])
			continue
		}
		if err != nil {
			if len(line) > 0 {
				return string(line), nil
			}
			return "", io.EOF
		}
	}
}

// Prints the preflight of platform, bot driver, Chromium and saved Google auth
func cmdSetup(ctx context.Context) int {
	fmt.Fprintln(cliOut, "google_meet preflight\n---------------------")
	system := pyplatform.System()
	systemOK := system == "Linux" || system == "Darwin"
	state := "unsupported"
	if systemOK {
		state = "ok"
	}
	fmt.Fprintf(cliOut, "  platform       : %s  [%s]\n", system, state)
	pwOK := meetDriverAvailable()
	if pwOK {
		fmt.Fprintln(cliOut, "  playwright     : installed")
	} else {
		fmt.Fprintln(cliOut, "  playwright     : NOT installed — run: icebath meet install")
	}
	chromiumOK, chromiumMsg := false, "unknown"
	if pwOK {
		exe := browser.ChromiumExecutable(botEnv(), true)
		if exe != "" {
			_, err := os.Stat(exe)
			chromiumOK = err == nil
		}
		chromiumMsg = "not installed — run: icebath meet install"
		if chromiumOK {
			chromiumMsg = "ok (" + exe + ")"
		}
	}
	fmt.Fprintf(cliOut, "  chromium       : %s\n", chromiumMsg)
	authPath := authStatePath(ctx)
	if st, err := os.Stat(authPath); err == nil && st.Mode().IsRegular() {
		fmt.Fprintf(cliOut, "  google auth    : ok (%s)\n", authPath)
	} else {
		fmt.Fprintln(cliOut, "  google auth    : not saved — run: icebath meet auth")
	}
	fmt.Fprintln(cliOut)
	if systemOK && pwOK && chromiumOK {
		fmt.Fprintln(cliOut, "ready. Join a meeting:  icebath meet join https://meet.google.com/abc-defg-hij")
		return 0
	}
	fmt.Fprintln(cliOut, "not ready yet — fix the items above.")
	return 1
}

// Confirms unless assumeYes then runs a package manager command, reporting a non zero exit
func installPkgs(assumeYes bool, prompt string, argv []string, failMsg string) error {
	ok := assumeYes
	if !ok {
		answer, err := readLine(prompt + " [y/N] ")
		ok = err == nil && (pystr.Lower(pystr.Strip(answer)) == "y" || pystr.Lower(pystr.Strip(answer)) == "yes")
	}
	if !ok {
		fmt.Fprintln(cliOut, "  skipped (you can run it manually later)")
		return nil
	}
	fmt.Fprintf(cliOut, "  $ %s\n", strings.Join(argv, " "))
	code, err := installRun(argv)
	if err != nil {
		return err
	}
	if code != 0 {
		fmt.Fprintln(cliOut, failMsg)
	}
	return nil
}

// Installs the google-meet extra and PM Chromium, plus platform audio tools with realtime
func cmdInstall(ctx context.Context, realtime, assumeYes bool) int {
	system := pyplatform.System()
	if system != "Linux" && system != "Darwin" {
		fmt.Fprintf(cliOut, "google_meet install: %s is not supported (linux/macos only)\n", system)
		return 1
	}
	fmt.Fprintln(cliOut, "google_meet install\n-------------------")
	fmt.Fprintln(cliOut, "\n[1/3] Python dependencies (google-meet extra)")
	if err := pmSyncVenv(ctx, pm.SyncVenvOptions{Extras: []string{"google-meet"}, Explicit: true}); err != nil {
		fmt.Fprintf(cliOut, "  dependency install failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(cliOut, "\n[2/3] PM-managed Chromium")
	if _, err := pmEnsure(ctx, "chromium", pm.EnsureOptions{Explicit: true}); err != nil {
		fmt.Fprintf(cliOut, "  Chromium install failed: %v\n", err)
		return 1
	}
	if !realtime {
		fmt.Fprintln(cliOut, "\n[3/3] skipped (pass --realtime to install audio tooling too)")
	} else if err := installAudioTools(system, assumeYes); err != nil {
		return uncaught(err)
	}
	fmt.Fprintln(cliOut, "\ndone. verify with: icebath meet setup")
	return 0
}

// Step three of install, pulseaudio-utils on Linux or BlackHole and ffmpeg through brew on macOS
func installAudioTools(system string, assumeYes bool) error {
	fmt.Fprintln(cliOut, "\n[3/3] realtime audio deps")
	have := func(name string) bool { _, err := lookPath(name); return err == nil }
	switch system {
	case "Linux":
		if have("paplay") && have("pactl") {
			fmt.Fprintln(cliOut, "  pulseaudio-utils already installed.")
			return nil
		}
		return installPkgs(assumeYes, "  install pulseaudio-utils? this runs `sudo apt-get install -y pulseaudio-utils`",
			[]string{"sudo", "apt-get", "install", "-y", "pulseaudio-utils"},
			"  apt install failed — install pulseaudio-utils manually")
	case "Darwin":
		out, err := audioDevices()
		haveBH := err == nil && strings.Contains(out, "BlackHole")
		var needs []string
		if !haveBH {
			needs = append(needs, "blackhole-2ch")
		}
		if !have("ffmpeg") {
			needs = append(needs, "ffmpeg")
		}
		switch {
		case len(needs) == 0:
			fmt.Fprintln(cliOut, "  BlackHole and ffmpeg already installed.")
		case !have("brew"):
			fmt.Fprintln(cliOut, "  missing: "+strings.Join(needs, ", ")+"\n"+
				"  install Homebrew first (https://brew.sh) or install the packages manually.")
		default:
			if err := installPkgs(assumeYes, fmt.Sprintf("  install via brew: %s?", strings.Join(needs, " ")),
				append([]string{"brew", "install"}, needs...), "  brew install failed — install them manually"); err != nil {
				return err
			}
		}
		fmt.Fprintln(cliOut, "\n  NOTE: macOS does not auto-route audio. Open\n    System Settings → Sound → "+
			"Input\n  and select 'BlackHole 2ch' before starting a realtime meeting.\n  "+
			"icebath will not switch your default input for you.")
	}
	return nil
}

// Opens headed Chromium for a Google sign in and saves the storage state
func cmdAuth(ctx context.Context) int {
	path := authStatePath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return uncaught(err)
	}
	fmt.Fprintf(cliOut, "opening Chromium — sign in to Google, then return here and press Enter.\n"+
		"saving storage state to: %s\n", path)
	if err := authSession(path); err != nil {
		fmt.Fprintf(cliOut, "auth failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(cliOut, "saved. you can now run: icebath meet join <url>")
	return 0
}

// Launches the sign in browser and saves its storage state to path, closing it on every path
var authSession = func(path string) error {
	b, err := launchMeetBrowser(botEnv(), true, nil)
	if err != nil {
		return err
	}
	defer b.close()
	bctx, err := b.newContext(contextArgs{Width: 1280, Height: 720})
	if err != nil {
		return err
	}
	page, err := bctx.newPage()
	if err != nil {
		return err
	}
	if err := page.gotoURL("https://accounts.google.com/", 30*time.Second); err != nil {
		return err
	}
	_, _ = readLine("press Enter after you've signed in ... ")
	return bctx.saveStorageState(path)
}

// Prints the result as indented JSON, exit code zero when ok
func printResult(res *pyjson.Object) int {
	out, err := pyjson.Dumps(res, pyjson.Indent(2))
	if err != nil {
		return uncaught(err)
	}
	fmt.Fprintln(cliOut, out)
	if pyrepr.Truthy(getOr(res, "ok", nil)) {
		return 0
	}
	return 1
}

// Runs call against the registered node and prints its result with the node name
func remoteCall(ctx context.Context, node any, op string, call func(*nodeClient) (*pyjson.Object, error)) int {
	client, name, err := resolveNode(ctx, node)
	if err != nil {
		return uncaught(err)
	}
	if client == nil {
		fmt.Fprintf(cliOut, "no registered node matches %s\n", pyrepr.Repr(node))
		return 1
	}
	res, err := call(client)
	if err != nil {
		fmt.Fprintf(cliOut, "remote %s failed: %v\n", op, err)
		return 1
	}
	out := pyjson.ObjectOf("node", name)
	out.Update(res)
	return printResult(out)
}

// Joins a safe Meet URL locally with any saved auth, or on the named node
func cmdJoin(ctx context.Context, url, guestName string, duration *string, headed bool, mode string, node any) int {
	if !isSafeMeetURL(url) {
		fmt.Fprintf(cliOut, "refusing: not a meet.google.com URL: %s\n", url)
		return 2
	}
	if pyrepr.Truthy(node) {
		return remoteCall(ctx, node, "start_bot", func(c *nodeClient) (*pyjson.Object, error) {
			return c.startBot(url, guestName, duration, headed, mode)
		})
	}
	o := newStartOptions()
	o.Headed, o.GuestName, o.Mode = headed, guestName, mode
	if duration != nil {
		o.Duration = *duration
	}
	if st, err := os.Stat(authStatePath(ctx)); err == nil && st.Mode().IsRegular() {
		o.AuthState = authStatePath(ctx)
	}
	res, err := pmStart(ctx, url, o)
	if err != nil {
		return uncaught(err)
	}
	return printResult(res)
}

// Queues non blank text locally or on the named node
func cmdSay(ctx context.Context, text string, node any) int {
	if pystr.Strip(text) == "" {
		fmt.Fprintln(cliOut, "refusing: empty text")
		return 2
	}
	if pyrepr.Truthy(node) {
		return remoteCall(ctx, node, "say", func(c *nodeClient) (*pyjson.Object, error) { return c.say(text) })
	}
	res, err := pmEnqueueSay(ctx, text)
	if err != nil {
		return uncaught(err)
	}
	return printResult(res)
}

// Prints the transcript lines, or the failure result when no meeting is active
func cmdTranscript(ctx context.Context, last *int) int {
	n := 0
	if last != nil {
		n = *last
	}
	res, err := pmTranscript(ctx, n)
	if err != nil {
		return uncaught(err)
	}
	if !pyrepr.Truthy(getOr(res, "ok", nil)) {
		return printResult(res)
	}
	lines, _ := getOr(res, "lines", []any{}).([]any)
	for _, ln := range lines {
		fmt.Fprintln(cliOut, pyrepr.Str(ln))
	}
	return 0
}
