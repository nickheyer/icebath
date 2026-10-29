package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

var version = "dev"

func init() {
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

type settings []string

func (k *settings) String() string     { return strings.Join(*k, ",") }
func (k *settings) Set(v string) error { *k = append(*k, v); return nil }

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if err := ext.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 2
	}
	fs := flag.NewFlagSet("icebath", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		profile  = fs.String("profile", "", "endpoint profile, built in ones are "+strings.Join(names(), ", "))
		model    = fs.String("model", "", "model id the profile serves")
		resume   = fs.String("resume", "", "session id to continue")
		last     = fs.Bool("continue", false, "continue the last session of this project")
		dir      = fs.String("dir", "", "working directory of the shell, the current directory when empty")
		root     = fs.String("root", "", "project root for hooks, rules and servers, the working directory when empty")
		mode     = fs.String("mode", "", "permission mode, default, acceptEdits, plan or bypassPermissions")
		config   = fs.String("config", "", "extra configuration file read after the user and project ones")
		print    = fs.Bool("print", false, "run without a terminal and write every event as a JSON line")
		headless = fs.Bool("headless", false, "run without a terminal and write the answer as text")
		yes      = fs.Bool("yes", false, "approve every tool call when running without a terminal")
		mission  = fs.String("mission", "", "objective to start a mission with")
		kind     = fs.String("kind", "", "kind the session runs as, linked ones are "+strings.Join(kindNames(), ", "))
		workflow = fs.String("workflow", "", "workflow to run with the prompt as its argument, linked ones are "+strings.Join(workflowNames(), ", "))
		list     = fs.Bool("sessions", false, "list the stored sessions of this project and exit")
		every    = fs.Bool("all", false, "with -sessions, list the sessions of every project")
		theme    = fs.String("theme", "", "palette, blackwall, light, contrast or plain")
		motion   = fs.Float64("motion", -1, "animation scale from 0 to 1")
		showVer  = fs.Bool("version", false, "print the version and exit")
		set      settings
	)
	fs.BoolVar(last, "c", false, "short for -continue")
	fs.Var(&set, "k", "setting override such as max_tokens=4096, repeatable")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: icebath [flags] [prompt]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVer {
		fmt.Println("icebath", version)
		return 0
	}
	cwd := *dir
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "icebath:", err)
			return 2
		}
		cwd = wd
	}
	cfg, err := harness.Discover(cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 2
	}
	if *config != "" {
		extra, err := harness.ReadConfigFile(*config)
		if err != nil {
			fmt.Fprintln(os.Stderr, "icebath:", err)
			return 2
		}
		cfg = harness.Merge(cfg, extra).WithEnv()
	}
	if *profile != "" {
		cfg.Profile = *profile
	}
	if *model != "" {
		cfg.Model = *model
	}
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *root != "" {
		cfg.Root = *root
	}
	if *theme != "" {
		cfg.UI.Theme = *theme
	}
	if *motion >= 0 {
		m := *motion
		cfg.UI.Motion = &m
	}
	for _, k := range set {
		key, value, err := harness.ParseSetting(k)
		if err != nil {
			fmt.Fprintln(os.Stderr, "icebath:", err)
			return 2
		}
		if err := cfg.Set(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "icebath:", err)
			return 2
		}
	}
	project := cfg.Root
	if project == "" {
		project = cwd
	}
	if project, err = filepath.Abs(project); err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 2
	}
	if *list {
		return listSessions(cfg, project, *every)
	}
	if *last {
		if *resume != "" {
			fmt.Fprintln(os.Stderr, "icebath: -continue and -resume each pick a session, pass one")
			return 2
		}
		if *resume, err = newestSession(cfg, project); err != nil {
			fmt.Fprintln(os.Stderr, "icebath:", err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	tty := term.IsTerminal(os.Stdout.Fd()) && term.IsTerminal(os.Stdin.Fd())
	if *print || *headless || !tty {
		var in *os.File
		if !term.IsTerminal(os.Stdin.Fd()) {
			in = os.Stdin
		}
		var input interface{ Read([]byte) (int, error) }
		if in != nil {
			input = in
		}
		return app.Headless(ctx, app.HeadlessOptions{Config: cfg, Resume: *resume, Dir: cwd, Prompt: prompt, Mission: *mission, Kind: *kind, Workflow: *workflow, Print: *print, Yes: *yes, Out: os.Stdout, Err: os.Stderr, In: input})
	}
	m, err := app.New(ctx, app.Options{Config: cfg, Resume: *resume, Dir: cwd, Version: version, Kind: *kind})
	if err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 2
	}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithFilter(m.Filter))
	m.Attach(p)
	if prompt != "" || *mission != "" || *workflow != "" {
		go func() {
			switch {
			case *workflow != "":
				p.Send(app.StartWorkflow(*workflow, prompt))
			case *mission != "":
				p.Send(app.StartMission(*mission))
			default:
				p.Send(app.StartPrompt(prompt))
			}
		}()
	}
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 1
	}
	return 0
}

func names() []string {
	var out []string
	for _, p := range profiles.Defaults() {
		out = append(out, p.Name)
	}
	return out
}

// Names of the kinds extensions linked in, the ones kind files add known only once a directory is chosen
func kindNames() []string {
	out := []string{harness.NoKind}
	for _, k := range harness.RegisteredKinds() {
		out = append(out, k.Name)
	}
	return out
}

func workflowNames() []string {
	var out []string
	for _, w := range harness.Workflows() {
		out = append(out, w.Name)
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// Id of the newest session of the project to continue
func newestSession(cfg harness.Config, root string) (string, error) {
	dir, err := cfg.SessionsPath()
	if err != nil {
		return "", err
	}
	return harness.NewestSession(context.Background(), &session.Store{Dir: dir}, root)
}

// Prints the stored sessions of the project newest first, or of every project, naming the project of each when every
func listSessions(cfg harness.Config, root string, every bool) int {
	dir, err := cfg.SessionsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 2
	}
	store := &session.Store{Dir: dir}
	infos, err := store.List(context.Background())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no sessions under", dir)
			return 0
		}
		fmt.Fprintln(os.Stderr, "icebath:", err)
		return 2
	}
	if !every {
		kept := infos[:0]
		for _, info := range infos {
			if p := info.Labels[session.ProjectLabel]; p == "" || p == root {
				kept = append(kept, info)
			}
		}
		infos = kept
	}
	if len(infos) == 0 {
		if every {
			fmt.Println("no sessions under", dir)
		} else {
			fmt.Println("no sessions of", root, "under", dir, "(pass -all for every project)")
		}
		return 0
	}
	for _, info := range infos {
		printInfo(info, every)
	}
	return 0
}

func printInfo(info session.Info, every bool) {
	title := info.Title
	if title == "" {
		title = "untitled"
	}
	if info.Error != "" {
		fmt.Printf("%s  %s\n", info.ID, "unreadable: "+info.Error)
		return
	}
	state := "      "
	if info.Busy {
		state = "open  "
	}
	fmt.Printf("%s  %s  %s%-28s  ", info.ID, info.Updated.Local().Format("2006-01-02 15:04"), state, render.Truncate(title, 28, theme.Blackwall()))
	if every {
		project := info.Labels[session.ProjectLabel]
		if project == "" {
			project = "-"
		}
		fmt.Printf("%-20s  ", render.Truncate(filepath.Base(project), 20, theme.Blackwall()))
	}
	fmt.Printf("%s  %d msgs  %s\n", info.Model, info.Messages, render.Tokens(info.Usage.Total()))
}
