package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/launch"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentlaunch "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/launch"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Source of the sessions a one-shot run makes, and of those a query run makes
const (
	oneshotSource = "oneshot"
	querySource   = "cli"
)

// Exit codes of a run: interrupted; refused or a one-shot run whose turn failed; a turn failed, or a one-shot run answered with nothing;
// left waiting on input
const (
	exitInterrupted = 130
	exitFailed      = 2
	exitEmpty       = 1
	exitDetached    = 3
)

// Longest tool output a stream-json event carries
const streamOutputCap = 5000

// Ends the run with the exit code, after its output was written
type exit struct{ code int }

// Says nothing: what the run had to say it printed
func (e exit) Error() string { return "" }
func (e exit) ExitCode() int { return e.code }

type commands struct {
	// Flags of a launch the features add, such as -w, and the work they do around the run
	launch *launch.Registry
	// Sessions of the launch, which end as their hooks know it once the run does
	asked *agentlaunch.Asked
}

func Register(registry *cli.Registry, launches *launch.Registry, signins *providercli.Registry, asked *agentlaunch.Asked) {
	registry.Register(commands{launch: launches, asked: asked})
	signins.Register(chooser{})
}

// The profile's provider of new sessions, which auth logout clears with the last login of the provider it names
type chooser struct{}

func (chooser) SignIns() []credentials.SignIn { return nil }

func (chooser) Choice() string { return agentconfig.Provider }

func (c commands) Commands(conn *connect.Client) []*cobra.Command {
	return []*cobra.Command{c.chat(conn)}
}

// What a chat run is asked to do
type options struct {
	query, queryFile, oneshot, usageFile, format, image string
	model, provider, reasoning, resume, continued, in   string
	source                                              string
	quiet, verbose, yolo, create, listTools, listSets   bool
	// What the run asks of its session over the profile's settings
	skills                                           []string
	maxTurns                                         int
	runBudget                                        float64
	safeMode, ignoreConfig, ignoreRules, passSession bool
}

func (c commands) chat(conn *connect.Client) *cobra.Command {
	var o options
	launchFlags := map[string]*bool{}
	launchValues := map[string]*[]string{}
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Answer one query and print it, or list the tools a chat would have",
		Long: "Answer one query in a new session, or one resumed, and print the reply. -z prints only the final reply and runs commands without " +
			"asking; -q prints the reply as it streams. A query can also come from --query-file or piped stdin.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			given := launch.Given{}
			for name, values := range launch.From(cmd.Context()) {
				given[name] = values
			}
			for name, on := range launchFlags {
				if *on {
					given = given.With(name)
				}
			}
			for name, values := range launchValues {
				if len(*values) > 0 {
					given = given.With(name, *values...)
				}
			}
			// The same asks given ahead of the command count as the command's own
			o.safeMode = o.safeMode || given.Has("--safe-mode") || config.Yes(os.Getenv("ICEBATH_SAFE_MODE"))
			o.ignoreConfig = o.ignoreConfig || given.Has("--ignore-user-config") || config.Yes(os.Getenv("ICEBATH_IGNORE_USER_CONFIG"))
			o.ignoreRules = o.ignoreRules || given.Has("--ignore-rules")
			o.passSession = o.passSession || given.Has("--pass-session-id")
			o.skills = append(given.Values("-s"), o.skills...)
			// What the run leaves out of the profile, the work around it leaves out too, such as consent to hooks of a config it does not read
			if o.safeMode && !given.Has("--safe-mode") {
				given = given.With("--safe-mode")
			}
			if o.ignoreConfig && !given.Has("--ignore-user-config") {
				given = given.With("--ignore-user-config")
			}
			return c.around(cmd, conn, o, given)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&o.query, "query", "q", "", "the query to answer")
	flags.StringVar(&o.queryFile, "query-file", "", "read the query from a file, - for stdin")
	flags.StringVarP(&o.oneshot, "oneshot", "z", "", "answer the prompt and print only the final reply, approvals bypassed")
	flags.StringVar(&o.usageFile, "usage-file", "", "write a JSON report of the tokens the run took, even when it fails")
	flags.StringVar(&o.format, "format", "text", "output format: text or stream-json (newline-delimited JSON events)")
	flags.BoolVarP(&o.quiet, "quiet", "Q", false, "print only the reply: no query line, tool lines or streaming")
	flags.BoolVarP(&o.verbose, "verbose", "v", false, "also print tool results and the agent's notices")
	flags.StringVar(&o.image, "image", "", "attach an image file to the query")
	flags.StringVarP(&o.model, "model", "m", "", "model for this run")
	flags.StringVar(&o.provider, "provider", "", "provider for this run, as named in the profile")
	flags.StringVar(&o.reasoning, "reasoning", "", "reasoning effort for this run")
	flags.StringVarP(&o.resume, "resume", "r", "", "resume the session the id, its start or a title names; latest for the newest of this workspace")
	flags.StringVarP(&o.continued, "continue", "c", "", "resume the newest session of this workspace, or the one titled NAME")
	flags.Lookup("continue").NoOptDefVal = " "
	flags.BoolVar(&o.create, "create-if-missing", false, "with -c NAME, start a session of that title when none has it")
	flags.StringVar(&o.in, "in", "", "start or resume in this folder, whose workspace session lookups are scoped to")
	flags.StringVar(&o.source, "source", "", "source tag of the session; tool keeps it out of session lists")
	flags.BoolVar(&o.yolo, "yolo", false, "run every command without asking")
	flags.BoolVar(&o.listTools, "list-tools", false, "list the tools a chat would have, then exit")
	flags.BoolVar(&o.listSets, "list-toolsets", false, "list the toolsets a chat would have, then exit")
	flags.StringArrayVarP(&o.skills, "skills", "s", nil, "Preload one or more skills for the session (repeat flag or comma-separate)")
	flags.IntVar(&o.maxTurns, "max-turns", 0, "Maximum tool-calling iterations per conversation turn, 0 for no limit (default: agent.max_turns in config)")
	flags.Float64Var(&o.runBudget, "run-budget", 0, "Optional wall-clock budget in seconds for each conversation run. At 80% elapsed the agent gets a "+
		"one-time wrap-up notice. Unset = off. Also configurable as agent.run_budget_seconds in config.yaml. Intended for one-shot/eval invocations "+
		"with a hard ceiling.")
	flags.BoolVar(&o.passSession, "pass-session-id", false, "Include the session ID in the agent's system prompt")
	flags.BoolVar(&o.ignoreConfig, "ignore-user-config", false, "Ignore the profile's config.yaml and fall back to built-in defaults (credentials in "+
		".env are still loaded). Useful for isolated CI runs, reproduction, and third-party integrations.")
	flags.BoolVar(&o.ignoreRules, "ignore-rules", false, "Skip auto-injection of AGENTS.md, SOUL.md, .cursorrules, memory, and preloaded skills. "+
		"Combine with --ignore-user-config for a fully isolated run.")
	flags.BoolVar(&o.safeMode, "safe-mode", false, "Troubleshooting mode: disable ALL customizations — user config, AGENTS.md/memory injection, "+
		"and MCP servers (implies --ignore-user-config and --ignore-rules). Use to isolate whether a problem comes from your setup or from "+
		"Icebath itself.")
	for _, flag := range c.launch.Flags() {
		long, short := "", ""
		for _, name := range flag.Names {
			switch {
			case strings.HasPrefix(name, "--"):
				long = cmp.Or(long, strings.TrimPrefix(name, "--"))
			case strings.HasPrefix(name, "-"):
				short = cmp.Or(short, strings.TrimPrefix(name, "-"))
			}
		}
		switch {
		case long == "" || flags.Lookup(long) != nil:
		case flag.Value != "":
			launchValues[flag.Names[0]] = flags.StringArrayP(long, short, nil, flag.Help)
		default:
			launchFlags[flag.Names[0]] = flags.BoolP(long, short, false, flag.Help)
		}
	}
	return cmd
}

// Runs the chat inside the work the features do around a launch, such as the worktree of -w, its lines kept off the standard output of a
// run that prints only its reply or JSON there
func (c commands) around(cmd *cobra.Command, conn *connect.Client, o options, given launch.Given) error {
	if given.Has("-w") && o.in != "" {
		// The worktree is made of the repository --in names, and the session works in it
		dir, err := folder(o.in)
		if err != nil {
			return err
		}
		if err := os.Chdir(dir); err != nil {
			return fmt.Errorf("cannot enter --in directory %s: %w", o.in, err)
		}
		o.in = ""
	}
	cmd.SetOut(cmd.OutOrStdout())
	if o.format == "stream-json" || o.oneshot != "" {
		stdout := os.Stdout
		os.Stdout = os.Stderr
		defer func() { os.Stdout = stdout }()
	}
	return c.launch.Around(cmd.Context(), conn, given, func(ctx context.Context) error {
		return run(ctx, conn, cmd, o, given, c.asked)
	})
}

func run(ctx context.Context, conn *connect.Client, cmd *cobra.Command, o options, given launch.Given, asked *agentlaunch.Asked) error {
	if o.listTools || o.listSets {
		return list(ctx, conn, cmd.OutOrStdout(), o.listSets)
	}
	if o.format != "text" && o.format != "stream-json" {
		return exit{code: exitFailed}.with(cmd, "Error: --format must be text or stream-json")
	}
	if (o.safeMode || o.ignoreConfig) && o.model == "" && o.resume == "" && o.continued == "" {
		return exit{code: exitEmpty}.with(cmd, "Error: --safe-mode / --ignore-user-config (or ICEBATH_SAFE_MODE / ICEBATH_IGNORE_USER_CONFIG) read no "+
			"profile default: pass --model explicitly.\n  Example: icebath chat --safe-mode --provider openrouter --model anthropic/claude-sonnet-4 -q \"hello\"")
	}
	if o.safeMode {
		// Sessions this process serves itself leave out every customization, MCP servers it would start included
		os.Setenv("ICEBATH_SAFE_MODE", "1")
		os.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
		os.Setenv("ICEBATH_IGNORE_RULES", "1")
	} else if o.ignoreConfig {
		os.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
	}
	query, err := o.text(cmd)
	if err != nil {
		return err
	}
	if o.format == "stream-json" {
		o.quiet = true
	}
	dir, err := folder(o.in)
	if err != nil {
		return err
	}
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	agents := agentv1connect.NewAgentServiceClient(conn)
	session, err := o.session(ctx, sessions, dir)
	if err != nil {
		return err
	}
	asked.Hold(session.GetId())
	r := &runner{o: o, agents: agents, sessions: sessions, id: session.GetId(), out: cmd.OutOrStdout(), errs: cmd.ErrOrStderr(), started: time.Now(),
		launch: o.launch(cmd, given), asked: asked}
	return r.run(ctx, conn, query)
}

// What the run asks of its session over the profile's settings, the flags given alone
func (o *options) launch(cmd *cobra.Command, given launch.Given) *agentv1.Launch {
	l := &agentv1.Launch{Skills: o.skills}
	if cmd.Flags().Changed("max-turns") {
		l.MaxTurns = new(int32(o.maxTurns))
	}
	if o.runBudget != 0 {
		l.RunBudget = new(o.runBudget)
	}
	flags := []struct {
		on  bool
		set **bool
	}{{o.passSession, &l.PassSessionId}, {o.ignoreRules, &l.IgnoreRules}, {o.ignoreConfig, &l.IgnoreUserConfig}, {o.safeMode, &l.SafeMode},
		{given.Has("--checkpoints"), &l.Checkpoints}}
	for _, flag := range flags {
		if flag.on {
			*flag.set = new(true)
		}
	}
	return l
}

// Query of the run: -z, -q, --query-file or piped stdin, refused when two are given or the file is empty
func (o *options) text(cmd *cobra.Command) (string, error) {
	query := cmp.Or(o.oneshot, o.query)
	if o.oneshot != "" && o.query != "" {
		return "", exit{code: exitFailed}.with(cmd, "Error: -z/--oneshot and -q/--query are mutually exclusive")
	}
	if o.queryFile != "" {
		if query != "" {
			return "", exit{code: exitFailed}.with(cmd, "Error: -q/--query and --query-file are mutually exclusive")
		}
		var raw []byte
		var err error
		if o.queryFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(o.queryFile)
		}
		if err != nil {
			return "", exit{code: exitFailed}.with(cmd, fmt.Sprintf("Error: cannot read --query-file %s: %v", o.queryFile, err))
		}
		query = strings.TrimPrefix(strings.ToValidUTF8(string(raw), "\uFFFD"), "\ufeff")
		if strings.TrimSpace(query) == "" {
			return "", exit{code: exitFailed}.with(cmd, fmt.Sprintf("Error: --query-file %s is empty", o.queryFile))
		}
	}
	if query == "" {
		if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return "", err
			}
			query = string(raw)
		}
	}
	if strings.TrimSpace(query) == "" && o.image == "" {
		return "", exit{code: exitFailed}.with(cmd, "Error: give a query with -q, -z, --query-file or on stdin")
	}
	if o.image != "" {
		path, err := filepath.Abs(o.image)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", exit{code: exitFailed}.with(cmd, fmt.Sprintf("Error: cannot read --image %s: %v", o.image, err))
		}
		query = strings.TrimSpace(query + "\n\n" + "Look at the image " + path)
	}
	return query, nil
}

// Prints the message on stderr and ends the run with the code
func (e exit) with(cmd *cobra.Command, message string) error {
	fmt.Fprintln(cmd.ErrOrStderr(), message)
	return e
}

// Folder the session works in: the one --in names, else this one
func folder(in string) (string, error) {
	if in == "" {
		return os.Getwd()
	}
	dir, err := filepath.Abs(in)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("--in %s is not a folder", in)
	}
	return dir, nil
}

// Session of the run: the one resumed, or a new one in the folder
func (o *options) session(ctx context.Context, sessions sessionsv1connect.SessionServiceClient, dir string) (*sessionsv1.Session, error) {
	source := querySource
	if o.oneshot != "" {
		source = oneshotSource
	}
	source = cmp.Or(o.source, source)
	ref := o.resume
	title := strings.TrimSpace(o.continued)
	switch {
	case ref == "latest" || o.continued != "" && title == "":
		listed, err := sessions.List(ctx, &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{Limit: 1, Workspace: dir}})
		if err != nil {
			return nil, err
		}
		if len(listed.GetSessions()) == 0 {
			return nil, errors.New("No session of this workspace to continue.")
		}
		ref = listed.GetSessions()[0].GetId()
	case title != "":
		ref = title
	}
	if ref != "" {
		found, err := sessions.Resolve(ctx, &sessionsv1.ResolveRequest{Ref: ref, Tip: true})
		if err == nil {
			return found.GetSession(), nil
		}
		if !o.create || title == "" {
			return nil, err
		}
	}
	made, err := sessions.Create(ctx, &sessionsv1.CreateRequest{Directory: dir, Source: source, Title: title})
	if err != nil {
		return nil, err
	}
	return made.GetSession(), nil
}

// Lists the tools a chat would have, by name, or their toolsets
func list(ctx context.Context, conn *connect.Client, out io.Writer, sets bool) error {
	listed, err := toolsv1connect.NewToolServiceClient(conn).List(ctx, &toolsv1.ListRequest{})
	if err != nil {
		return err
	}
	byset := map[string][]string{}
	for _, tool := range listed.GetTools() {
		if tool.GetAvailable() && !tool.GetDisabled() {
			byset[tool.GetToolset()] = append(byset[tool.GetToolset()], tool.GetName())
		}
	}
	names := make([]string, 0, len(byset))
	for name := range byset {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, set := range names {
		if sets {
			fmt.Fprintf(out, "%s (%d tools)\n", set, len(byset[set]))
			continue
		}
		sort.Strings(byset[set])
		for _, tool := range byset[set] {
			fmt.Fprintf(out, "%s\t%s\n", tool, set)
		}
	}
	return nil
}

// One run: the session watched, the query sent, its reply printed as the format and quietness say, until the turn ends
type runner struct {
	o        options
	agents   agentv1connect.AgentServiceClient
	sessions sessionsv1connect.SessionServiceClient
	id       string
	out      io.Writer
	errs     io.Writer
	started  time.Time
	// Messages of the session before the query, and those of the run
	before  int
	reply   string
	failure string
	stopped bool
	tools   map[string]time.Time
	model   string
	// What the run asks of its session, sent with the query
	launch *agentv1.Launch
	// Profile of the session, once known; the run was ended by a stop signal without its turn's end
	profile atomic.Pointer[string]
	cut     atomic.Bool
	// Why the run left its turn waiting on input no one gives here, empty while it did not
	detached string
	// Text of the reply was streamed since the last message
	streamed bool
	// What the session's model calls took before the run, which the run's own report leaves out, and the session it is of
	spent   *sessionsv1.Usage
	spentOn string
	// Sessions of the launch, which end as their hooks know it once the run does
	asked *agentlaunch.Asked
}

func (r *runner) run(parent context.Context, conn *connect.Client, query string) error {
	// A stop signal stops the turn, whose end the run waits for the signal grace at most; a second one ends the process
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	go r.interrupt(parent, ctx, cancel, conn)
	err := r.ask(parent, ctx, conn, query)
	if err != nil && !r.cut.Load() {
		return err
	}
	return r.finish(ctx)
}

// Sends the query once the session is watched, and follows the turn it starts to its end
func (r *runner) ask(parent, ctx context.Context, conn *connect.Client, query string) error {
	stream, err := r.agents.Watch(ctx, &agentv1.WatchRequest{SessionId: r.id, Client: "cli-" + fmt.Sprint(os.Getpid())})
	if err != nil {
		return err
	}
	defer stream.Close()
	// The watch is in place once its first event, the session's state, came: nothing is sent before, so every event of the turn reaches
	// the run
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	r.model = first.GetState().GetModel()
	got, err := r.sessions.Get(ctx, &sessionsv1.GetRequest{Id: r.id})
	if err != nil {
		return err
	}
	r.before, r.spent, r.spentOn = len(got.GetMessages()), got.GetSession().GetUsage(), r.id
	r.profile.Store(new(got.GetSession().GetProfile()))
	// Settings of this launch alone, as /model sets them for the session
	if switches := r.switches(); switches != "" {
		if _, err := r.agents.Send(ctx, &agentv1.SendRequest{SessionId: r.id, Text: switches}); err != nil {
			return err
		}
	}
	env, host := shell(ctx, conn, got.GetSession().GetProfile())
	if r.o.format == "stream-json" {
		r.emit(map[string]any{"type": "system", "subtype": "init", "model": cmp.Or(r.o.model, got.GetSession().GetModel(), r.model), "session_id": r.id})
	} else if !r.o.quiet && r.o.oneshot == "" {
		fmt.Fprintln(r.out, "Query: "+query)
	}
	if parent.Err() != nil {
		// Stopped before the query went
		r.cut.Store(true)
		return parent.Err()
	}
	if _, err := r.agents.Send(ctx, &agentv1.SendRequest{SessionId: r.id, Text: query, ClientEnv: env, ClientHost: host,
		Yolo: r.o.yolo || r.o.oneshot != "", Launch: r.launch, Finite: true}); err != nil {
		return err
	}
	return r.follow(stream)
}

// Stops the turn as the first stop signal comes, and ends the run without it once the signal grace passes and the turn still runs
func (r *runner) interrupt(parent, ctx context.Context, cancel context.CancelFunc, conn *connect.Client) {
	select {
	case <-ctx.Done():
		return
	case <-parent.Done():
	}
	r.agents.Stop(ctx, &agentv1.StopRequest{SessionId: r.id})
	grace := time.Duration(0)
	profile := ""
	if p := r.profile.Load(); p != nil {
		profile = *p
	}
	if setting, err := configv1connect.NewConfigServiceClient(conn).Get(ctx, &configv1.GetRequest{Key: launchconfig.SignalGrace, Profile: profile}); err == nil {
		var value any
		yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &value)
		grace, _ = config.Length(value, time.Second)
	}
	select {
	case <-ctx.Done():
	case <-time.After(grace):
		r.cut.Store(true)
		cancel()
	}
}

// The /model command that sets the launch's model, provider and reasoning for the session, none when the launch sets none
func (r *runner) switches() string {
	if r.o.model == "" && r.o.provider == "" && r.o.reasoning == "" {
		return ""
	}
	parts := []string{"/model"}
	if r.o.model != "" {
		parts = append(parts, r.o.model)
	}
	if r.o.provider != "" {
		parts = append(parts, "--provider", r.o.provider)
	}
	if r.o.reasoning != "" {
		parts = append(parts, "--reasoning", r.o.reasoning)
	}
	return strings.Join(append(parts, "--session"), " ")
}

// Reads the session's events until the turn the query started has ended, printing as they come
func (r *runner) follow(stream agentv1connect.AgentServiceWatchClientStream) error {
	started := false
	seen := 0
	for {
		event, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("The connection to icebath ended before the reply did.")
			}
			return err
		}
		switch e := event.GetEvent().(type) {
		case *agentv1.WatchResponse_State:
			r.model = e.State.GetModel()
		case *agentv1.WatchResponse_Busy:
			if e.Busy {
				started = true
			} else if started {
				return nil
			}
		case *agentv1.WatchResponse_Delta:
			if !started {
				continue
			}
			r.streamed = r.streamed || e.Delta.GetText() != ""
			if r.o.format == "stream-json" && e.Delta.GetText() != "" {
				r.emit(map[string]any{"type": "text", "text": e.Delta.GetText()})
			} else if !r.o.quiet && r.o.oneshot == "" {
				fmt.Fprint(r.out, e.Delta.GetText())
			}
		case *agentv1.WatchResponse_Notice:
			if !started {
				continue
			}
			switch text := e.Notice; {
			case text == "Stopped.":
				r.stopped = true
			case strings.HasPrefix(text, "**Error:** "):
				r.failure = strings.TrimPrefix(text, "**Error:** ")
			case r.o.verbose:
				fmt.Fprintln(r.errs, text)
			}
		case *agentv1.WatchResponse_Message:
			if seen++; seen <= r.before || !started {
				continue
			}
			r.message(e.Message)
		case *agentv1.WatchResponse_Subagent:
			if started {
				r.subagent(e.Subagent)
			}
		case *agentv1.WatchResponse_Continued:
			// The conversation went on in a new session after its context was compressed, which the run follows
			if e.Continued != "" && e.Continued != r.id {
				r.id = e.Continued
				r.asked.Hold(r.id)
			}
		case *agentv1.WatchResponse_Approvals:
			// No one answers a prompt the run waits on: it leaves the turn waiting for an interactive window
			if started && len(e.Approvals.GetPending()) > 0 {
				r.detached = "Input required; detached without cancelling. Resume this session interactively."
				return nil
			}
		case *agentv1.WatchResponse_Questions:
			if started && len(e.Questions.GetQuestions()) > 0 {
				r.detached = "Input required; detached without cancelling. Resume this session interactively."
				return nil
			}
		}
	}
}

// Prints a subagent's step under the reply as its tree line: what it thinks and the tools it calls, and why it failed
func (r *runner) subagent(event *agentv1.Subagent) {
	if r.o.format == "stream-json" {
		r.emit(map[string]any{"type": "subagent", "event": event.GetKind(), "subagent_id": event.GetId(), "parent_id": event.GetParentId(),
			"depth": event.GetDepth(), "task_index": event.GetTaskIndex(), "task_count": event.GetTaskCount(), "goal": event.GetGoal(),
			"tool": event.GetTool(), "text": event.GetText(), "status": event.GetStatus(), "duration_seconds": event.GetDurationSeconds()})
		return
	}
	if r.o.quiet || r.o.oneshot != "" {
		return
	}
	prefix := ""
	if event.GetTaskCount() > 1 {
		prefix = fmt.Sprintf("[%d/%d] ", event.GetTaskIndex()+1, event.GetTaskCount())
	}
	line := ""
	switch event.GetKind() {
	case "thinking":
		if text := strings.TrimSpace(event.GetText()); text != "" {
			line = fmt.Sprintf("💭 %q", shortText(text, 55))
		}
	case "tool":
		line = "⚡ " + event.GetTool()
		if preview := strings.TrimSpace(event.GetPreview()); preview != "" {
			line += fmt.Sprintf("  %q", shortText(preview, 35))
		}
	case "complete":
		switch event.GetStatus() {
		case "failed", "error", "timeout":
			line = failureLine(event)
		}
	}
	if line != "" {
		fmt.Fprintln(r.errs, " "+prefix+"├─ "+line)
	}
}

func shortText(text string, n int) string {
	if runes := []rune(text); len(runes) > n {
		return string(runes[:n]) + "..."
	}
	return text
}

// Plain clause of the kind of failure that ended a subagent
var failureGloss = map[string]string{
	"timeout": "the AI model service did not respond in time", "network": "the AI model service did not respond in time",
	"rate_limit": "the AI model service was rate-limited (too many requests)", "upstream_rate_limit": "the AI model service was rate-limited (too many requests)",
	"overloaded": "the AI model service is overloaded right now", "server": "the AI model service returned an internal error",
	"server_error": "the AI model service returned an internal error", "billing": "the AI model service says the account's usage or credit limit is reached",
	"quota": "the AI model service says the account's usage or credit limit is reached", "auth": "the AI model service rejected the sign-in",
	"auth_permanent": "the AI model service rejected the sign-in", "upstream_blocked": "a firewall/CDN in front of the AI model service blocked the request",
	"model_not_found": "the model it uses was not found at the AI model service", "content_filter": "the AI model service's safety filter rejected the request",
	"content_policy_blocked":  "the AI model service's safety filter rejected the request",
	"provider_policy_blocked": "the AI model service refused the request because of a policy on the account",
	"context_overflow":        "its request grew too large for the model", "payload_too_large": "its request grew too large for the model",
}

// Line about a subagent that failed or timed out, with what the user can do next
func failureLine(event *agentv1.Subagent) string {
	goal := strings.ReplaceAll(strings.TrimSpace(event.GetGoal()), "\n", " ")
	if runes := []rune(goal); len(runes) > 60 {
		goal = string(runes[:57]) + "..."
	}
	part := ""
	if goal != "" {
		part = fmt.Sprintf(` — "%s"`, goal)
	}
	took := ""
	switch seconds := event.GetDurationSeconds(); {
	case seconds >= 120:
		took = fmt.Sprintf("%d min", int(math.RoundToEven(seconds/60)))
	case seconds > 0:
		took = fmt.Sprintf("%ds", int(math.RoundToEven(seconds)))
	}
	if event.GetStatus() == "timeout" {
		after := ""
		if took != "" {
			after = " after " + took
		}
		return fmt.Sprintf("⚠️ Subagent timed out%s%s without finishing. I will carry on without it; ask me to retry it, or raise "+
			"delegation.child_timeout_seconds in config.yaml if these tasks legitimately take longer.", part, after)
	}
	line := "⚠️ Subagent failed" + part
	if took != "" {
		line += " after " + took
	}
	reason := failureGloss[event.GetFailureReason()]
	if reason == "" {
		reason = strs.Clip(strings.Join(strings.Fields(cmp.Or(event.GetSummary(), event.GetText())), " "), 200)
	}
	if reason != "" {
		line += ": " + strings.TrimRight(reason, ".")
	}
	return line + ". Details: /agents, or ask me to retry with a smaller task."
}

// Takes one message of the run in: a reply's text is the final one so far, a tool call and its result shown as the format says
func (r *runner) message(m *sessionsv1.Message) {
	switch m.GetRole() {
	case sessionsv1.Role_ROLE_ASSISTANT:
		if text := strings.TrimSpace(m.GetText()); text != "" {
			r.reply = text
		}
		// A reply that came whole, such as one a hook wrote in place of the model's, is shown as it was not streamed
		whole := !r.streamed && m.GetText() != ""
		r.streamed = false
		switch {
		case whole && r.o.format == "stream-json":
			r.emit(map[string]any{"type": "text", "text": m.GetText()})
		case whole && !r.o.quiet && r.o.oneshot == "":
			fmt.Fprintln(r.out, m.GetText())
		case !r.o.quiet && r.o.oneshot == "" && m.GetText() != "":
			fmt.Fprintln(r.out)
		}
		for _, call := range m.GetToolCalls() {
			if r.tools == nil {
				r.tools = map[string]time.Time{}
			}
			r.tools[call.GetId()] = time.Now()
			switch {
			case r.o.format == "stream-json":
				var input any
				json.Unmarshal([]byte(call.GetArguments()), &input)
				r.emit(map[string]any{"type": "tool_use", "name": call.GetName(), "tool_call_id": call.GetId(), "input": input})
			case !r.o.quiet && r.o.oneshot == "":
				fmt.Fprintf(r.errs, "  ┊ %s %s\n", call.GetName(), strs.Clip(call.GetArguments(), 120))
			}
		}
	case sessionsv1.Role_ROLE_TOOL:
		took := time.Duration(0)
		if at, ok := r.tools[m.GetToolCallId()]; ok {
			took = time.Since(at)
		}
		switch {
		case r.o.format == "stream-json":
			r.emit(map[string]any{"type": "tool_result", "name": m.GetToolName(), "tool_call_id": m.GetToolCallId(), "output": strs.Clip(m.GetText(), streamOutputCap),
				"duration_ms": took.Milliseconds(), "is_error": m.GetFailed()})
		case r.o.verbose:
			fmt.Fprintf(r.errs, "  ┊ %s → %s\n", m.GetToolName(), strs.Clip(m.GetText(), 400))
		}
	}
}

// Prints the outcome, writes the usage report and ends with the run's exit code
func (r *runner) finish(ctx context.Context) error {
	if r.detached != "" {
		fmt.Fprintln(r.errs, r.detached)
		if r.o.format == "stream-json" {
			r.emit(map[string]any{"type": "result", "session_id": r.id, "exit_code": exitDetached, "text": "", "error": r.detached,
				"duration_ms": time.Since(r.started).Milliseconds(), "tokens": map[string]any{"input": 0, "output": 0, "total": 0, "cache_read": 0, "cache_write": 0}})
		}
		return exit{code: exitDetached}
	}
	// A one-shot run fails apart from answering with nothing; a query run fails alike for both, a turn that ended with no reply ending well
	code := 0
	switch oneshot := r.o.oneshot != ""; {
	case r.interrupted():
		code = exitInterrupted
	case r.failure != "" && oneshot:
		code = exitFailed
	case r.failure != "" || r.reply == "" && oneshot:
		code = exitEmpty
	}
	got, err := r.sessions.Get(context.WithoutCancel(ctx), &sessionsv1.GetRequest{Id: r.id})
	var session *sessionsv1.Session
	if err == nil {
		session = got.GetSession()
	}
	switch {
	case r.o.format == "stream-json":
		usage := r.usage(session)
		result := map[string]any{"type": "result", "session_id": r.id, "exit_code": code, "text": r.reply, "duration_ms": time.Since(r.started).Milliseconds(),
			"tokens": map[string]any{"input": usage.GetInput() + usage.GetCacheRead() + usage.GetCacheWrite(), "output": usage.GetOutput(),
				"total": usage.GetInput() + usage.GetCacheRead() + usage.GetCacheWrite() + usage.GetOutput(), "cache_read": usage.GetCacheRead(),
				"cache_write": usage.GetCacheWrite()}}
		if r.failure != "" {
			result["error"] = r.failure
		}
		r.emit(result)
	case r.failure != "" && r.reply == "":
		fmt.Fprintln(r.errs, "Error: "+r.failure)
	case r.o.quiet || r.o.oneshot != "":
		if r.reply != "" {
			fmt.Fprintln(r.out, r.reply)
		}
	}
	if r.failure != "" && r.reply != "" {
		fmt.Fprintln(r.errs, "Error: "+r.failure)
	}
	if r.o.format != "stream-json" {
		switch {
		case r.interrupted() && r.reply == "":
			fmt.Fprintln(r.errs, "Turn interrupted; it was not retried. Resume this session to continue.")
		case r.o.oneshot != "" && code == exitEmpty:
			fmt.Fprintln(r.errs, "icebath -z: no final response was produced; treating the run as failed.")
		}
	}
	if r.o.oneshot == "" {
		fmt.Fprintf(r.errs, "\nsession_id: %s\n", r.id)
	}
	if !r.o.quiet && r.o.oneshot == "" {
		// How to go on with the session, the profile named unless it is the default
		flag := ""
		if profile := session.GetProfile(); profile != "" && profile != "default" && profile != "custom" {
			flag = " -p " + profile
		}
		fmt.Fprintf(r.out, "\nResume this session with:\n  icebath --resume %s%s\n", r.id, flag)
	}
	r.report(session, code)
	if code != 0 {
		return exit{code: code}
	}
	return nil
}

// Whether the run was stopped: its turn said so, or a stop signal ended the run without the turn's end
func (r *runner) interrupted() bool {
	return r.stopped || r.cut.Load()
}

// What the session's model calls took in this run: its usage now less what it held before the run, all of it for the session a compression
// continued the conversation in
func (r *runner) usage(session *sessionsv1.Session) *sessionsv1.Usage {
	now, before := session.GetUsage(), r.spent
	if before == nil || session.GetId() != r.spentOn {
		return now
	}
	return &sessionsv1.Usage{Input: now.GetInput() - before.GetInput(), Output: now.GetOutput() - before.GetOutput(),
		CacheRead: now.GetCacheRead() - before.GetCacheRead(), CacheWrite: now.GetCacheWrite() - before.GetCacheWrite(),
		Reasoning: now.GetReasoning() - before.GetReasoning(), ApiCalls: now.GetApiCalls() - before.GetApiCalls(),
		EstimatedCost: now.GetEstimatedCost() - before.GetEstimatedCost(), ActualCost: now.GetActualCost() - before.GetActualCost(),
		CostStatus: now.GetCostStatus(), CostSource: now.GetCostSource(), PricingVersion: now.GetPricingVersion()}
}

// Writes the usage file the run was asked for, its failure included; a report that can't be written leaves the outcome as it is
func (r *runner) report(session *sessionsv1.Session, code int) {
	if r.o.usageFile == "" {
		return
	}
	usage := r.usage(session)
	prompt := usage.GetInput() + usage.GetCacheRead() + usage.GetCacheWrite()
	report := map[string]any{"estimated_cost_usd": nil, "cost_status": cmp.Or(usage.GetCostStatus(), "unknown"), "cost_source": cmp.Or(usage.GetCostSource(), "none"),
		"input_tokens": prompt, "output_tokens": usage.GetOutput(), "cache_read_tokens": usage.GetCacheRead(), "cache_write_tokens": usage.GetCacheWrite(),
		"reasoning_tokens": usage.GetReasoning(), "total_tokens": prompt + usage.GetOutput(), "api_calls": usage.GetApiCalls(), "model": session.GetModel(),
		"provider": session.GetProvider(), "session_id": r.id, "completed": code == 0, "partial": false, "interrupted": r.interrupted(),
		"turn_exit_reason": "completed", "failed": r.failure != "", "service_tier": nil}
	switch {
	case r.interrupted():
		report["turn_exit_reason"] = "interrupted"
	case r.failure != "":
		report["turn_exit_reason"] = "error"
	case r.reply == "":
		report["turn_exit_reason"] = "empty_response"
	}
	if r.failure != "" {
		report["failure"] = r.failure
	}
	path := r.o.usageFile
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, rest)
		}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o777) != nil {
		return
	}
	os.WriteFile(path, append(data, '\n'), 0o666)
}

// Writes one stream-json event line with its time
func (r *runner) emit(event map[string]any) {
	event["timestamp"] = time.Now().UnixMilli()
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintln(r.out, string(data))
}

// Variables of this shell the session takes as its client's, with the name of this machine
func shell(ctx context.Context, conn *connect.Client, profile string) (map[string]string, string) {
	var extra []string
	settings := configv1connect.NewConfigServiceClient(conn)
	if setting, err := settings.Get(ctx, &configv1.GetRequest{Key: terminalconfig.ClientEnvPassthrough, Profile: profile}); err == nil {
		yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &extra)
	}
	host, _ := os.Hostname()
	return tools.ClientVariables(extra), host
}
