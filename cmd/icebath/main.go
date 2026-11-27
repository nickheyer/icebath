package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	appapp "github.com/nickheyer/icebath/internal/app/app"
	"github.com/nickheyer/icebath/internal/approvals"
	approvalsapp "github.com/nickheyer/icebath/internal/approvals/app"
	approvalscli "github.com/nickheyer/icebath/internal/approvals/cli"
	approvalsserver "github.com/nickheyer/icebath/internal/approvals/server"
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/botdesktop"
	botdesktopapp "github.com/nickheyer/icebath/internal/botdesktop/app"
	botdesktopserver "github.com/nickheyer/icebath/internal/botdesktop/server"
	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/client"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	configcli "github.com/nickheyer/icebath/internal/config/cli"
	configserver "github.com/nickheyer/icebath/internal/config/server"
	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/delegation"
	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/launch"
	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/models"
	osvconfig "github.com/nickheyer/icebath/internal/osv/config"
	"github.com/nickheyer/icebath/internal/packages"
	packagescli "github.com/nickheyer/icebath/internal/packages/cli"
	packagesserver "github.com/nickheyer/icebath/internal/packages/server"
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults"
	"github.com/nickheyer/icebath/internal/profile"
	profileapp "github.com/nickheyer/icebath/internal/profile/app"
	profilecli "github.com/nickheyer/icebath/internal/profile/cli"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	profileserver "github.com/nickheyer/icebath/internal/profile/server"
	"github.com/nickheyer/icebath/internal/prompt"
	promptcli "github.com/nickheyer/icebath/internal/prompt/cli"
	promptserver "github.com/nickheyer/icebath/internal/prompt/server"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
	providerserver "github.com/nickheyer/icebath/internal/provider/server"
	"github.com/nickheyer/icebath/internal/secrets"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	secretscli "github.com/nickheyer/icebath/internal/secrets/cli"
	secretsserver "github.com/nickheyer/icebath/internal/secrets/server"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionscli "github.com/nickheyer/icebath/internal/sessions/cli"
	sessionsserver "github.com/nickheyer/icebath/internal/sessions/server"
	sessionsslash "github.com/nickheyer/icebath/internal/sessions/slash"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/slash"
	slashserver "github.com/nickheyer/icebath/internal/slash/server"
	"github.com/nickheyer/icebath/internal/speech"
	speechapp "github.com/nickheyer/icebath/internal/speech/app"
	speechserver "github.com/nickheyer/icebath/internal/speech/server"
	"github.com/nickheyer/icebath/internal/state"
	statecli "github.com/nickheyer/icebath/internal/state/cli"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalapp "github.com/nickheyer/icebath/internal/terminal/app"
	terminalserver "github.com/nickheyer/icebath/internal/terminal/server"
	"github.com/nickheyer/icebath/internal/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	toolsserver "github.com/nickheyer/icebath/internal/tools/server"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/internal/vault"
	vaultapp "github.com/nickheyer/icebath/internal/vault/app"
	vaultcli "github.com/nickheyer/icebath/internal/vault/cli"
	vaultserver "github.com/nickheyer/icebath/internal/vault/server"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/internal/worktrees"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop) // a second signal kills
	if err := run(ctx, os.Args[1:]); err != nil {
		// A command that printed its own failure ends with the status it names and no line of its own
		code := 1
		if status, ok := errors.AsType[interface {
			error
			ExitCode() int
		}](err); ok {
			code = status.ExitCode()
		}
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, "icebath:", err)
		}
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string) (err error) {
	// The folder of settings and profiles: ICEBATH_HOME, else the user's config folder's; a profile's folder named there picks that profile
	dir, pinned, err := profile.Home()
	if err != nil {
		return err
	}
	// -p/--profile picks the profile of this run, carried by ICEBATH_PROFILE into every process it starts
	picked, args, err := profilecli.Pick(args, os.Stderr)
	if err != nil {
		return err
	}
	if picked = cmp.Or(picked, pinned); picked != "" {
		dir = cmp.Or(profile.SudoHome(dir, picked), dir)
		os.Setenv(profileconfig.ActiveEnv, picked)
	} else if sticky := profile.Sticky(dir); sticky != "" && os.Getenv(profileconfig.ActiveEnv) == "" && os.Getenv(profileconfig.PinnedEnv) == "" {
		// Under sudo the profile used last may be one only the user who ran sudo has
		if sudo := profile.SudoHome(dir, sticky); sudo != "" {
			dir = sudo
			os.Setenv(profileconfig.ActiveEnv, sticky)
		}
	}
	os.Setenv(profile.HomeEnv, dir)
	// -z PROMPT answers one prompt and prints the reply, as chat -z does
	if len(args) > 0 && (args[0] == "-z" || args[0] == "--oneshot" || strings.HasPrefix(args[0], "--oneshot=") || strings.HasPrefix(args[0], "-z=")) {
		args = append([]string{"chat"}, args...)
	}
	// --yolo runs every session of this process without asking, as ICEBATH_YOLO_MODE does
	if i, end := slices.Index(args, "--yolo"), slices.Index(args, "--"); i >= 0 && (end < 0 || i < end) {
		args = slices.Delete(slices.Clone(args), i, i+1)
		os.Setenv("ICEBATH_YOLO_MODE", "1")
	}

	cfg := config.New(dir)
	// The malware check MCP servers and the skills hub share
	osvconfig.Register(cfg)
	pr := profile.New(cfg)
	st := state.New(dir)
	pr.Register(st)
	srv := server.New(cfg)
	a := app.New(cfg)
	c := cli.New()
	p := provider.New(cfg)
	// The .env writer refuses the variables that name the program a provider starts
	cfg.Launching(p.Registry.LaunchEnv)
	cn := providerapp.New()
	si := providercli.New()
	t := transport.New(cfg)
	t.Describe(&p.Registry)
	cr := credentials.New()
	cr.Keep(cfg)
	m := models.New(cfg, &p.Registry)
	// The agent's lifecycle hook points, which plugins and the user's shell hooks set callbacks on
	hk := hooks.New(cfg)
	tl := tools.New(cfg, hk)
	s := sessions.New(cfg, hk)
	sl := slash.New()
	pm := prompt.New()
	tv := toolsapp.New()
	w := web.New(cfg)
	ax := auxiliary.New(cfg, &p.Registry, m, hk)
	ax.Register(sessions.Titles{})
	ax.Register(profileserver.Describer{})
	ap := approvals.New(cfg, ax, hk)
	tm := terminal.New(cfg, hk)
	pk := packages.New(cfg)
	sk := skills.New(cfg, &tl.Registry, pk, hk)
	mc := mcp.New()
	cx := contexts.New(cfg, &tl.Registry)
	ln := launch.New(cfg)
	wt := worktrees.New(cfg, s)
	// The record of trees follows a renamed profile's folder
	pr.Register(worktrees.Profiles{Worktrees: wt})
	// The environment every profile reads its variables through: its .env and what its secret stores supply over this process's
	sc := secrets.New(cfg)
	scc := secretscli.New()
	ss := secretsapp.New()
	va := vault.New(cfg)
	pr.Register(sc)
	pr.Register(va)
	pr.Register(tm)
	pr.Register(s)
	s.OnRelease(tm.Release)
	s.OnRelease(sk.Release)
	s.OnRelease(va.Release)
	// The Chromium of this machine and the cloud browsers, and each profile's own screen that computer use and the browser drive
	br := browser.New(cfg, pk)
	bd := botdesktop.New(cfg, tm, br)
	pr.Register(botdesktop.Profiles{Desktop: bd})
	pr.Register(credentials.Profiles{Credentials: cr, Providers: &p.Registry, Config: cfg})
	// Backends that decide where each subagent of delegate_task runs
	dl := delegation.New(cfg)
	// Services that draw pictures and make videos for image_generate and video_generate
	gn := generation.New(cfg)
	// Voices, recognizers and wake engines that text_to_speech, transcription and voice mode run on
	sp := speech.New(cfg, pk, tm, &p.Registry, &cr.Registry, &t.Registry, ax, hk, &tl.Registry)
	// A new profile takes the speech and voice settings of the profile it was made from
	pr.Register(speech.Profiles{Config: cfg})
	defaults.Register(plugins.New(cfg, srv, a, c, pr, p, cn, si, t, cr, m, tl, s, sl, pm, tv, w, ax, tm, sk, mc, ap, pk, cx, ln, wt, sc, scc, ss, va, br, bd, dl, gn, hk, sp))
	statecli.Register(c, st)
	profileserver.Register(srv, pr, &ax.Registry, &p.Registry, &t.Registry, &cr.Registry)
	profileapp.Register(a)
	profilecli.Register(c)
	appapp.Register(a)
	configserver.Register(srv, cfg, &p.Registry, credentials.Lifecycle{Providers: &p.Registry})
	configapp.Register(a)
	configcli.Register(c)
	providerserver.Register(srv, cfg, p, t, cr, m)
	providerapp.Register(a, cn)
	providercli.Register(c, si)
	sessionsserver.Register(srv, cfg, s, pm, &ax.Registry, &p.Registry, &t.Registry, &cr.Registry)
	// Flags of the launch the features add, taken out of a launch of the app and handed to what they do around it
	var given launch.Given
	// The app, run in this process on its own store, keeps the store's upkeep going while it runs
	launch := func(ctx context.Context, conn *connect.Client) error {
		if st.Backend == "" {
			s.Start(ctx)
		}
		return ln.Around(ctx, conn, given, func(ctx context.Context) error { return a.Run(ctx, conn) })
	}
	sessionscli.Register(c, launch)
	sessionsslash.Register(sl, cfg, s)
	slashserver.Register(srv, sl)
	toolsserver.Register(srv, cfg, tl)
	toolsapp.Register(a)
	promptserver.Register(srv, pm)
	promptcli.Register(c)
	terminalserver.Register(srv, cfg, tm)
	terminalapp.Register(a)
	approvalsserver.Register(srv, ap, cfg, s, tm, &tl.Registry)
	approvalscli.Register(c)
	approvalsapp.Register(a)
	packagesserver.Register(srv, cfg, pk)
	packagescli.Register(c)
	secretsserver.Register(srv, cfg, sc)
	secretscli.Register(c, scc)
	secretsapp.Register(a, ss)
	vaultserver.Register(srv, cfg, va)
	vaultcli.Register(c)
	vaultapp.Register(a)
	botdesktopserver.Register(srv, cfg, bd)
	botdesktopapp.Register(a)
	speechserver.Register(srv, cfg, sp, pr)
	speechapp.Register(a)
	// A process wedged after a stop signal or in its cleanup is ended anyway, the commands it started killed first
	kill := func() { tm.KillForeground(true) }
	ln.Signaled(ctx, kill)
	defer func() {
		ln.Cleaning(kill)
		err = errors.Join(err, srv.Close(), tl.Close(), tm.Close(), s.Close(), va.Close(), hk.Close(), cfg.Close())
	}()
	if err := pr.Load(profilecli.Recovering(args), os.Stderr); err != nil {
		return err
	}
	// The launcher of a Bot Desktop runs this binary for one idle check of its screen between its sleeps
	if len(args) > 0 && args[0] == botdesktop.IdleEntry {
		return bd.Idle(ctx, args[1:], os.Stdout)
	}
	// A process that serves its own profiles carries the variables of the one in use, saying what its secret stores did
	if st.Backend == "" || (len(args) > 0 && args[0] == "server") {
		sc.Serve(os.Stderr)
	}

	if len(args) > 0 && args[0] == "server" {
		if len(args) > 1 {
			return errors.New("server takes no arguments")
		}
		terminal.RegisterSelf(cfg, "server")
		tm.Outlive()
		ap.Serve()
		s.Start(ctx)
		return srv.Serve(ctx)
	}

	terminal.RegisterSelf(cfg, "cli")
	if warning := terminal.SharedProfile(cfg); warning != "" {
		fmt.Fprintln(os.Stderr, "Warning: "+warning)
	}
	var conn *connect.Client
	if st.Backend != "" {
		conn = client.Remote(st.Backend)
	} else {
		conn = client.InProcess(srv.Services())
	}

	args, given = ln.Take(args)
	if launched, err := sessionscli.Launch(ctx, conn, dir, args, launch); launched {
		return err
	}
	c.Launch(ln.Flags(), given)
	return c.Run(profile.WithPicked(ctx, picked), conn, args)
}
