package tui

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the screens of the system hub
const systemContext = "tui.system"

// Keys of the system screens beyond the shared hub keys
func registerSystemKeys(km *keys.Map) {
	if keys.ContextDocs[systemContext] == "" {
		if err := keys.AddContext(systemContext, "system screen, its log files"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.system.level", Context: systemContext, Label: "level", Doc: "show every line, only warnings and errors, or only errors", Default: []string{"l"}})
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "system", order: 60, title: txt.HubsTitleSystem, short: txt.HubsShortSystem, slash: "system",
		usage: "[service|health|logs|storage|packages|updates|security|…]", open: func(a *app, arg string) tea.Cmd { return a.openSystem(arg) }, keys: registerSystemKeys,
		suggest: func(string) []string { return systemSectionIDs }})
}

// Names /system takes to open one section
var systemSectionIDs = []string{"service", "health", "logs", "storage", "packages", "updates", "security", "lsp", "computer-use", "proxy", "migrations"}

// System screen: its hub, the command tree of this build and what it measured
type systemHub struct {
	a *app
	h *cards.Hub
	// Commands of this build by path, read once the screen opens
	tree map[string]tuigateway.CLICommand
	// Log files of the home and the parts of the home with their sizes, the second measured off the update loop
	logs     []systemLog
	parts    []systemPart
	measured bool
	// Last outcome of the update check and of the health check, empty until one ran
	update, health string
}

// One section of the system screen
type systemSection struct {
	id     string
	title  string
	covers string
	state  func() string
	open   func() tea.Cmd
	// Command paths the section runs, the section left out when this build has none of them
	paths []string
}

// Opens the system screen, at the section named when one is
func (a *app) openSystem(arg string) tea.Cmd {
	s := &systemHub{a: a}
	sections := s.sections()
	shown := func() []systemSection {
		return slices.DeleteFunc(slices.Clone(sections), func(sec systemSection) bool {
			return s.tree != nil && len(sec.paths) > 0 && !slices.ContainsFunc(sec.paths, func(p string) bool { _, ok := s.tree[p]; return ok })
		})
	}
	var rows []systemSection
	list := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSystem), Glyph: a.toolkit.Glyphs.Server, Key: "tui:system", Context: systemContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.SystemColSection), Max: 26}, {Title: tr(txt.SystemColState), Max: 36}, {Title: tr(txt.HubsColWhatItCovers), Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows = shown()
			out := make([][]string, len(rows))
			for i, sec := range rows {
				state := ""
				if sec.state != nil {
					state = sec.state()
				}
				out[i] = []string{sec.title, state, sec.covers}
			}
			return out
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(rows) {
				return nil
			}
			return []string{rows[i].title, rows[i].covers}
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, {Action: "hub.refresh"}}
		},
		Quick: func(_ *cards.List, i int) tea.Cmd { return rows[i].open() },
		Free:  map[string]bool{"hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return rows[i].open() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.measure() },
		},
	})
	s.h = a.newHub("tui:system", list)
	// A hub with a close step resets whether it is shown, which the log reader polls on
	s.h.OnClose(func() tea.Cmd { return nil })
	s.readLogs()
	load := hubCall(a, s.h, tr(txt.SystemReading), tuigateway.CommandsTree, tuigateway.CommandsTreeParams{}, func(r *tuigateway.CommandsTreeResult) tea.Cmd {
		s.tree = map[string]tuigateway.CLICommand{}
		for _, c := range r.Commands {
			s.tree[c.Path] = c
		}
		list.Refresh()
		words := strings.Fields(strings.ToLower(arg))
		if len(words) == 0 {
			return nil
		}
		for _, sec := range shown() {
			if sec.id != words[0] && !strings.HasPrefix(sec.id, words[0]) {
				continue
			}
			if sec.id == "updates" && len(words) > 1 && words[1] == "check" {
				return tea.Batch(sec.open(), s.checkUpdates())
			}
			return sec.open()
		}
		return s.h.Fail(tr(txt.SystemUnknownSection, words[0], strings.Join(systemSectionIDs, ", ")))
	})
	return tea.Batch(a.wm.ShowOverlay(s.h), load, s.measure())
}

// Sections of the system screen in the order it lists them
func (s *systemHub) sections() []systemSection {
	a := s.a
	return []systemSection{
		{id: "service", title: tr(txt.SystemServiceTitle), covers: tr(txt.SystemServiceCovers), state: s.serviceState, open: s.openService,
			paths: []string{"gateway status", "gateway install", "pause"}},
		{id: "health", title: tr(txt.SystemHealthTitle), covers: tr(txt.SystemHealthCovers), state: func() string { return s.health }, open: s.openHealth,
			paths: []string{"doctor", "status", "dump"}},
		{id: "logs", title: tr(txt.SystemLogsTitle), covers: tr(txt.SystemLogsCovers), state: s.logsState, open: s.openLogs},
		{id: "storage", title: tr(txt.SystemStorageTitle), covers: tr(txt.SystemStorageCovers), state: s.storageState, open: s.openStorage},
		{id: "packages", title: tr(txt.SystemPackagesTitle), covers: tr(txt.SystemPackagesCovers), open: s.openPackages, paths: []string{"pm status"}},
		{id: "updates", title: tr(txt.SystemUpdatesTitle), covers: tr(txt.SystemUpdatesCovers), state: func() string { return s.update }, open: s.openUpdates,
			paths: []string{"update"}},
		{id: "security", title: tr(txt.SystemSecurityTitle), covers: tr(txt.SystemSecurityCovers), open: s.openSecurity, paths: []string{"security audit", "egress status"}},
		{id: "lsp", title: tr(txt.SystemLspTitle), covers: tr(txt.SystemLspCovers), open: s.openLSP, paths: []string{"lsp status"}},
		{id: "computer-use", title: tr(txt.SystemComputerTitle), covers: tr(txt.SystemComputerCovers), open: s.openComputerUse, paths: []string{"computer-use status"}},
		{id: "proxy", title: tr(txt.SystemProxyTitle), covers: tr(txt.SystemProxyCovers), open: s.openProxy, paths: []string{"proxy status"}},
		{id: "migrations", title: tr(txt.SystemMigrationsTitle), covers: tr(txt.SystemMigrationsCovers), open: s.openMigrations,
			paths: []string{"migrate xai", "codex-runtime migrate"}},
		{id: "after-exit", title: tr(txt.HubsTitleAfterExit), covers: tr(txt.HubsShortAfterExit), state: afterExitState, open: a.openAfterExit},
	}
}

// Command of this build that runs the words, the longest path the words start with, false for none or one people never run
func (s *systemHub) lookup(argv []string) (tuigateway.CLICommand, bool) {
	var words []string
	for _, w := range argv {
		if strings.HasPrefix(w, "-") {
			break
		}
		words = append(words, w)
	}
	for n := len(words); n > 0; n-- {
		if c, ok := s.tree[strings.Join(words[:n], " ")]; ok {
			return c, c.Class != tuigateway.CLIRunInternal
		}
	}
	return tuigateway.CLICommand{}, false
}

// Whether a command of class after exit runs inside because the words carry a flag that makes it read only
func readOnlyInside(c tuigateway.CLICommand, argv []string) bool {
	return slices.ContainsFunc(argv, func(w string) bool {
		name, _, _ := strings.Cut(w, "=")
		return strings.HasPrefix(name, "--") && slices.Contains(c.InsideWith, name[2:])
	})
}

// Action that runs icebath with the words, in a page of the hub or queued for after exit by the class of its command, nothing when this
// build lacks the command
func (s *systemHub) command(label, does string, argv []string, confirm string) []hubAction {
	c, ok := s.lookup(argv)
	if !ok {
		return nil
	}
	return []hubAction{{label: label, does: does, confirm: confirm, run: func() tea.Cmd { return s.run(label, argv, c, nil) }}}
}

// Runs icebath with the words, queuing them for after exit when they cannot run while the window is open, then hands the run on
func (s *systemHub) run(title string, argv []string, c tuigateway.CLICommand, then func(run *cliRun) tea.Cmd) tea.Cmd {
	line := "icebath " + joinCommandLine(argv)
	if c.Class == tuigateway.CLIRunAfterExit && !readOnlyInside(c, argv) {
		reason := c.ClassReason
		if err := queueAfterExit(argv, reason); err != nil {
			return s.h.Fail(tr(txt.AfterExitQueueFailed, err.Error()))
		}
		return s.h.Done(tr(txt.AfterExitQueued, line))
	}
	if then == nil {
		then = func(*cliRun) tea.Cmd { return nil }
	}
	return s.a.hubRunCLI(s.h, title, argv, then)
}

// Action that runs an interactive icebath command in a terminal window of its own, the hub making way for it
func (s *systemHub) interactive(label, does string, argv []string) []hubAction {
	if _, ok := s.lookup(argv); !ok {
		return nil
	}
	return []hubAction{{label: label, does: does, run: func() tea.Cmd {
		exe, err := os.Executable()
		if err != nil {
			return s.h.Fail(err.Error())
		}
		return tea.Batch(wm.RequestDismissOf(s.h), s.a.runInTerminal(append([]string{exe}, argv...), func(cards.TerminalEnd) tea.Cmd { return nil }))
	}}}
}

// Action that asks for one value and runs icebath with the words and the value
func (s *systemHub) asking(label, does, question string, argv []string) []hubAction {
	c, ok := s.lookup(argv)
	if !ok {
		return nil
	}
	return []hubAction{{label: label, does: does, run: func() tea.Cmd {
		return s.a.hubAsk(s.h, label, question, "", false, func(value string) tea.Cmd {
			return s.run(label, append(slices.Clone(argv), value), c, nil)
		})
	}}}
}

// Actions screen of a section: its lines on top and its actions, each joined only when this build has its command
func (s *systemHub) section(title string, detail func() []string, actions ...func() []hubAction) tea.Cmd {
	return s.a.hubActions(s.h, title, detail, func() []hubAction {
		var out []hubAction
		for _, act := range actions {
			out = append(out, act()...)
		}
		return out
	})
}

// Action builder of a command, built each time the screen lists its actions so a command this build lacks stays out
func (s *systemHub) cmd(label, does txt.Key, argv []string, confirm string) func() []hubAction {
	return func() []hubAction { return s.command(tr(label), tr(does), argv, confirm) }
}

// One line on the background service: connected or not, the live sessions and the emergency stop
func (s *systemHub) serviceState() string {
	a := s.a
	if a.store.ui.offline() {
		return tr(txt.SystemServiceOffline)
	}
	state := tr(txt.SystemServiceRunning, len(a.shell.live))
	if emergencyStopped(a) {
		state += " · " + tr(txt.SystemServicePaused)
	}
	return state
}

// Whether the emergency stop sentinel holds new work in this home or in the root home
func emergencyStopped(a *app) bool {
	for _, dir := range []string{home.Dir(a.ctx), home.Root()} {
		if _, err := os.Stat(filepath.Join(dir, "ESTOP")); err == nil {
			return true
		}
	}
	return false
}

// Background service: what it is, its state, restart and reconnect, the emergency stop, and the system service it may be installed as
func (s *systemHub) openService() tea.Cmd {
	detail := func() []string {
		return []string{tr(txt.SystemServiceAbout), "", s.serviceState(), tr(txt.SystemServiceInstallAbout)}
	}
	restart := func() []hubAction {
		if s.a.host.ReplaceOwner == nil {
			return s.command(tr(txt.SystemServiceRestart), tr(txt.SystemServiceRestartDoes), []string{"gateway", "restart", "--yes", "--detach"}, tr(txt.SystemServiceRestartConfirm))
		}
		return []hubAction{{label: tr(txt.SystemServiceRestart), does: tr(txt.SystemServiceRestartDoes), confirm: tr(txt.SystemServiceRestartConfirm), run: s.restartService}}
	}
	pause := func() []hubAction {
		if emergencyStopped(s.a) {
			return s.command(tr(txt.SystemServiceResume), tr(txt.SystemServiceResumeDoes), []string{"resume"}, "")
		}
		if _, ok := s.lookup([]string{"pause"}); !ok {
			return nil
		}
		return []hubAction{{label: tr(txt.SystemServicePause), does: tr(txt.SystemServicePauseDoes), run: func() tea.Cmd {
			return s.a.hubForm(s.h, cards.FormSpec{Title: tr(txt.SystemServicePause), Intro: []string{tr(txt.SystemServicePauseIntro)}, Submit: tr(txt.SystemServicePause),
				Fields: []cards.Field{{ID: "reason", Label: tr(txt.SystemServicePauseReason), Help: tr(txt.SystemServicePauseReasonHelp)}}},
				func(_ *cards.Form, values map[string]string) tea.Cmd {
					argv := []string{"pause"}
					if reason := strings.TrimSpace(values["reason"]); reason != "" {
						argv = append(argv, "--reason", reason)
					}
					c, _ := s.lookup(argv)
					return s.run(tr(txt.SystemServicePause), argv, c, nil)
				})
		}}}
	}
	return s.section(tr(txt.SystemServiceTitle), detail,
		s.cmd(txt.SystemServiceStatus, txt.SystemServiceStatusDoes, []string{"gateway", "status"}, ""),
		restart, pause,
		s.cmd(txt.SystemServiceList, txt.SystemServiceListDoes, []string{"gateway", "list"}, ""),
		s.cmd(txt.SystemServiceInstall, txt.SystemServiceInstallDoes, []string{"gateway", "install"}, tr(txt.SystemServiceInstallConfirm)),
		s.cmd(txt.SystemServiceUninstall, txt.SystemServiceUninstallDoes, []string{"gateway", "uninstall"}, tr(txt.SystemServiceUninstallConfirm)),
		s.cmd(txt.SystemServiceStop, txt.SystemServiceStopDoes, []string{"gateway", "stop"}, tr(txt.SystemServiceStopConfirm)),
	)
}

// Stops the background service off the update loop; this window then reconnects and starts it again, as every other window does
func (s *systemHub) restartService() tea.Cmd {
	s.h.Busy(tr(txt.SystemServiceRestarting))
	replace, ctx := s.a.host.ReplaceOwner, s.a.ctx
	return func() tea.Msg {
		err := replace(ctx)
		return asyncDoneMsg{run: func() tea.Cmd {
			if err != nil {
				return s.h.Fail(tr(txt.SystemServiceRestartFailed, err.Error()))
			}
			return s.h.Done(tr(txt.SystemServiceRestarted))
		}}
	}
}

// Health check: the checks, the fix, the component status and what to paste when asking for help
func (s *systemHub) openHealth() tea.Cmd {
	detail := func() []string {
		out := []string{tr(txt.SystemHealthAbout)}
		if s.health != "" {
			out = append(out, "", s.health)
		}
		return out
	}
	doctor := func(label, does string, argv []string, confirm string) func() []hubAction {
		return func() []hubAction {
			c, ok := s.lookup(argv)
			if !ok {
				return nil
			}
			return []hubAction{{label: label, does: does, confirm: confirm, run: func() tea.Cmd {
				return s.run(label, argv, c, func(run *cliRun) tea.Cmd {
					s.health = tr(txt.SystemHealthPassed)
					if run.err != nil || run.code != 0 {
						s.health = tr(txt.SystemHealthProblems)
					}
					return nil
				})
			}}}
		}
	}
	return s.section(tr(txt.SystemHealthTitle), detail,
		doctor(tr(txt.SystemHealthCheck), tr(txt.SystemHealthCheckDoes), []string{"doctor"}, ""),
		doctor(tr(txt.SystemHealthFix), tr(txt.SystemHealthFixDoes), []string{"doctor", "--fix"}, tr(txt.SystemHealthFixConfirm)),
		doctor(tr(txt.SystemHealthLive), tr(txt.SystemHealthLiveDoes), []string{"doctor", "--live"}, tr(txt.SystemHealthLiveConfirm)),
		s.cmd(txt.SystemHealthStatus, txt.SystemHealthStatusDoes, []string{"status", "--full"}, ""),
		s.cmd(txt.SystemHealthDeep, txt.SystemHealthDeepDoes, []string{"status", "--deep"}, ""),
		s.cmd(txt.SystemHealthConfig, txt.SystemHealthConfigDoes, []string{"config", "check"}, ""),
		s.cmd(txt.SystemHealthPromptSize, txt.SystemHealthPromptSizeDoes, []string{"prompt-size"}, ""),
		s.cmd(txt.SystemHealthMonitoring, txt.SystemHealthMonitoringDoes, []string{"monitoring", "status"}, ""),
		s.cmd(txt.SystemHealthDump, txt.SystemHealthDumpDoes, []string{"dump"}, ""),
		s.cmd(txt.SystemHealthReport, txt.SystemHealthReportDoes, []string{"debug", "share", "--local"}, ""),
		s.cmd(txt.SystemHealthShare, txt.SystemHealthShareDoes, []string{"debug", "share", "--yes"}, tr(txt.SystemHealthShareConfirm)),
		func() []hubAction {
			return s.asking(tr(txt.SystemHealthUnshare), tr(txt.SystemHealthUnshareDoes), tr(txt.SystemHealthUnshareAsk), []string{"debug", "delete"})
		},
	)
}

// Packages icebath installs for its tools: the read only checks inside, every change after exit
func (s *systemHub) openPackages() tea.Cmd {
	detail := func() []string { return []string{tr(txt.SystemPackagesAbout)} }
	return s.section(tr(txt.SystemPackagesTitle), detail,
		s.cmd(txt.SystemPackagesStatus, txt.SystemPackagesStatusDoes, []string{"pm", "status"}, ""),
		s.cmd(txt.SystemPackagesDoctor, txt.SystemPackagesDoctorDoes, []string{"pm", "doctor"}, ""),
		s.cmd(txt.SystemPackagesClean, txt.SystemPackagesCleanDoes, []string{"pm", "doctor", "--repair"}, tr(txt.SystemPackagesCleanConfirm)),
		s.cmd(txt.SystemPackagesEnv, txt.SystemPackagesEnvDoes, []string{"pm", "env"}, ""),
		s.cmd(txt.SystemPackagesCheck, txt.SystemPackagesCheckDoes, []string{"pm", "update", "--check"}, ""),
		s.cmd(txt.SystemPackagesUpdate, txt.SystemPackagesUpdateDoes, []string{"pm", "update"}, ""),
		s.cmd(txt.SystemPackagesInstall, txt.SystemPackagesInstallDoes, []string{"pm", "install"}, ""),
		s.cmd(txt.SystemPackagesRepair, txt.SystemPackagesRepairDoes, []string{"pm", "repair"}, ""),
		s.cmd(txt.SystemPackagesGc, txt.SystemPackagesGcDoes, []string{"pm", "gc"}, ""),
	)
}

// Updates: the read only check and plan inside, the update itself after exit with the offer to quit now
func (s *systemHub) openUpdates() tea.Cmd {
	detail := func() []string {
		out := []string{tr(txt.SystemUpdatesAbout)}
		if s.update != "" {
			out = append(out, "", s.update)
		}
		return out
	}
	check := func() []hubAction {
		if _, ok := s.lookup([]string{"update", "--check"}); !ok {
			return nil
		}
		return []hubAction{{label: tr(txt.SystemUpdatesCheck), does: tr(txt.SystemUpdatesCheckDoes), run: s.checkUpdates}}
	}
	quit := func() []hubAction {
		if !updateQueued() {
			return nil
		}
		return []hubAction{{label: tr(txt.SystemUpdatesQuit), does: tr(txt.SystemUpdatesQuitDoes), run: func() tea.Cmd { return s.a.die(0) }}}
	}
	queue := func() []hubAction {
		if updateQueued() {
			return nil
		}
		return s.command(tr(txt.SystemUpdatesQueue), tr(txt.SystemUpdatesQueueDoes), []string{"update"}, "")
	}
	return s.section(tr(txt.SystemUpdatesTitle), detail, quit, check,
		s.cmd(txt.SystemUpdatesPlan, txt.SystemUpdatesPlanDoes, []string{"update", "--plan"}, ""),
		queue)
}

// Runs the read only update check in a page, its first line kept as the state of the updates section
func (s *systemHub) checkUpdates() tea.Cmd {
	argv := []string{"update", "--check"}
	c, ok := s.lookup(argv)
	if !ok {
		return nil
	}
	return s.run(tr(txt.SystemUpdatesCheck), argv, c, func(run *cliRun) tea.Cmd {
		s.update = firstOutputLine(run)
		return nil
	})
}

// Whether this window holds an update to run after it exits
func updateQueued() bool {
	queued, err := afterExitQueued()
	return err == nil && slices.ContainsFunc(queued, func(c afterExitCommand) bool { return len(c.Argv) > 0 && c.Argv[0] == "update" })
}

// First line a command printed, for the state line of its section
func firstOutputLine(run *cliRun) string {
	for _, line := range run.lines {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if run.err != nil {
		return run.err.Error()
	}
	return ""
}

// Security: the audit of installed parts against known vulnerabilities and the optional network guard
func (s *systemHub) openSecurity() tea.Cmd {
	detail := func() []string { return []string{tr(txt.SystemSecurityAbout), "", tr(txt.SystemSecurityEgressAbout)} }
	return s.section(tr(txt.SystemSecurityTitle), detail,
		s.cmd(txt.SystemSecurityAudit, txt.SystemSecurityAuditDoes, []string{"security", "audit"}, ""),
		s.cmd(txt.SystemSecurityEgressStatus, txt.SystemSecurityEgressStatusDoes, []string{"egress", "status"}, ""),
		func() []hubAction {
			return s.interactive(tr(txt.SystemSecurityEgressSetup), tr(txt.SystemSecurityEgressSetupDoes), []string{"egress", "setup"})
		},
		s.cmd(txt.SystemSecurityEgressStart, txt.SystemSecurityEgressStartDoes, []string{"egress", "start"}, ""),
		s.cmd(txt.SystemSecurityEgressRestart, txt.SystemSecurityEgressRestartDoes, []string{"egress", "restart"}, ""),
		s.cmd(txt.SystemSecurityEgressReload, txt.SystemSecurityEgressReloadDoes, []string{"egress", "reload"}, ""),
		s.cmd(txt.SystemSecurityEgressStop, txt.SystemSecurityEgressStopDoes, []string{"egress", "stop"}, tr(txt.SystemSecurityEgressStopConfirm)),
		s.cmd(txt.SystemSecurityEgressInstall, txt.SystemSecurityEgressInstallDoes, []string{"egress", "install"}, ""),
		s.cmd(txt.SystemSecurityEgressConfig, txt.SystemSecurityEgressConfigDoes, []string{"egress", "config"}, ""),
		s.cmd(txt.SystemSecurityEgressDisable, txt.SystemSecurityEgressDisableDoes, []string{"egress", "disable"}, tr(txt.SystemSecurityEgressDisableConfirm)),
	)
}

// Language servers that check the files the agent writes
func (s *systemHub) openLSP() tea.Cmd {
	detail := func() []string { return []string{tr(txt.SystemLspAbout)} }
	return s.section(tr(txt.SystemLspTitle), detail,
		s.cmd(txt.SystemLspStatus, txt.SystemLspStatusDoes, []string{"lsp", "status"}, ""),
		s.cmd(txt.SystemLspList, txt.SystemLspListDoes, []string{"lsp", "list"}, ""),
		func() []hubAction {
			return s.asking(tr(txt.SystemLspInstall), tr(txt.SystemLspInstallDoes), tr(txt.SystemLspServerAsk), []string{"lsp", "install"})
		},
		s.cmd(txt.SystemLspInstallAll, txt.SystemLspInstallAllDoes, []string{"lsp", "install-all"}, tr(txt.SystemLspInstallAllConfirm)),
		func() []hubAction {
			return s.asking(tr(txt.SystemLspWhich), tr(txt.SystemLspWhichDoes), tr(txt.SystemLspServerAsk), []string{"lsp", "which"})
		},
		s.cmd(txt.SystemLspRestart, txt.SystemLspRestartDoes, []string{"lsp", "restart"}, ""),
	)
}

// Computer use: the driver the agent controls a screen through, its checks and the permissions it needs
func (s *systemHub) openComputerUse() tea.Cmd {
	detail := func() []string { return []string{tr(txt.SystemComputerAbout)} }
	return s.section(tr(txt.SystemComputerTitle), detail,
		s.cmd(txt.SystemComputerStatus, txt.SystemComputerStatusDoes, []string{"computer-use", "status"}, ""),
		s.cmd(txt.SystemComputerDoctor, txt.SystemComputerDoctorDoes, []string{"computer-use", "doctor"}, ""),
		s.cmd(txt.SystemComputerPermissions, txt.SystemComputerPermissionsDoes, []string{"computer-use", "permissions", "status"}, ""),
		s.cmd(txt.SystemComputerGrant, txt.SystemComputerGrantDoes, []string{"computer-use", "permissions", "grant"}, ""),
		s.cmd(txt.SystemComputerInstall, txt.SystemComputerInstallDoes, []string{"computer-use", "install"}, tr(txt.SystemComputerInstallConfirm)),
	)
}

// Local proxy that lends a subscription login to other apps
func (s *systemHub) openProxy() tea.Cmd {
	detail := func() []string { return []string{tr(txt.SystemProxyAbout)} }
	return s.section(tr(txt.SystemProxyTitle), detail,
		s.cmd(txt.SystemProxyStatus, txt.SystemProxyStatusDoes, []string{"proxy", "status"}, ""),
		s.cmd(txt.SystemProxyProviders, txt.SystemProxyProvidersDoes, []string{"proxy", "providers"}, ""),
		func() []hubAction {
			return s.interactive(tr(txt.SystemProxyStart), tr(txt.SystemProxyStartDoes), []string{"proxy", "start"})
		},
	)
}

// Rewrites of config files: the xAI models that retire and the block icebath keeps in the codex config
func (s *systemHub) openMigrations() tea.Cmd {
	detail := func() []string { return []string{tr(txt.SystemMigrationsAbout)} }
	return s.section(tr(txt.SystemMigrationsTitle), detail,
		s.cmd(txt.SystemMigrationsXaiCheck, txt.SystemMigrationsXaiCheckDoes, []string{"migrate", "xai"}, ""),
		s.cmd(txt.SystemMigrationsXai, txt.SystemMigrationsXaiDoes, []string{"migrate", "xai", "--apply"}, tr(txt.SystemMigrationsXaiConfirm)),
		s.cmd(txt.SystemMigrationsCodexCheck, txt.SystemMigrationsCodexCheckDoes, []string{"codex-runtime", "migrate", "--dry-run"}, ""),
		s.cmd(txt.SystemMigrationsCodex, txt.SystemMigrationsCodexDoes, []string{"codex-runtime", "migrate"}, tr(txt.SystemMigrationsCodexConfirm)),
	)
}

// State line of the after exit section, how many commands wait
func afterExitState() string {
	queued, err := afterExitQueued()
	if err != nil || len(queued) == 0 {
		return ""
	}
	return tr(txt.SystemAfterExitWaiting, len(queued))
}

// One log file of the home
type systemLog struct {
	name, path string
	size       int64
	modified   float64
}

// Reads the log files of the profile home and of the root home's own logs
func (s *systemHub) readLogs() {
	s.logs = nil
	dirs := []string{filepath.Join(home.Dir(s.a.ctx), "logs")}
	if root := filepath.Join(home.Root(), "logs"); root != dirs[0] {
		dirs = append(dirs, root)
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			path := filepath.Join(dir, e.Name())
			if err != nil || !info.Mode().IsRegular() || seen[path] {
				continue
			}
			seen[path] = true
			s.logs = append(s.logs, systemLog{name: e.Name(), path: path, size: info.Size(), modified: float64(info.ModTime().Unix())})
		}
	}
	slices.SortStableFunc(s.logs, func(x, y systemLog) int { return int(y.modified - x.modified) })
}

// One line on the log files: how many and how big
func (s *systemHub) logsState() string {
	var total int64
	for _, l := range s.logs {
		total += l.size
	}
	return tr(txt.SystemLogsState, len(s.logs), render.Bytes(total))
}

// Log files of the home, the newest first, enter following one
func (s *systemHub) openLogs() tea.Cmd {
	s.readLogs()
	list := cards.NewList(s.a.hubEnv(), cards.ListSpec{
		Title: tr(txt.SystemLogsTitle), Key: "tui:system:logs", Context: systemContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.SystemColFile), Max: 36}, {Title: tr(txt.SystemColSize), Right: true}, {Title: tr(txt.HubsColWhen), Right: true}, {Title: tr(txt.HubsColWhere), Flex: 1}},
		Yield:   []int{3},
		Rows: func() [][]string {
			out := make([][]string, len(s.logs))
			for i, l := range s.logs {
				out[i] = []string{l.name, render.Bytes(l.size), whenText(l.modified), filepath.Dir(l.path)}
			}
			return out
		},
		Empty: func() string { return tr(txt.SystemLogsEmpty) },
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return s.followLog(s.logs[i]) },
			"hub.refresh": func(l *cards.List, _ int) tea.Cmd { s.readLogs(); l.Refresh(); return nil },
		},
		Free: map[string]bool{"hub.refresh": true},
	})
	return s.h.Push(list)
}

// Least weight a log line needs to show, by the level the level key moved to
var logLevels = []struct {
	label txt.Key
	words []string
}{
	{txt.SystemLogsLevelAll, nil},
	{txt.SystemLogsLevelWarnings, []string{"WARNING", "WARN", "ERROR", "CRITICAL", "FATAL"}},
	{txt.SystemLogsLevelErrors, []string{"ERROR", "CRITICAL", "FATAL"}},
}

// How often an open log is read again
const logPollEvery = time.Second

// Bytes of a log's end a view reads, enough for the newest lines
const logTailBytes = 512 * 1024

// Lines of a log a view keeps
const logTailLines = 4000

// One parsed line of a log: its clock, level and the rest
type logLine struct {
	clock, level, text string
}

// Splits a log line into its clock, level and message, a line of another shape kept whole as the message
func parseLogLine(line string) logLine {
	f := strings.SplitN(line, " ", 4)
	if len(f) == 4 && len(f[0]) == 10 && strings.Count(f[0], "-") == 2 && strings.ToUpper(f[2]) == f[2] {
		clock, _, _ := strings.Cut(f[1], ",")
		return logLine{clock: clock, level: f[2], text: f[3]}
	}
	return logLine{text: line}
}

// Reads the end of a log, its newest lines last
func tailLog(path string) ([]logLine, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	start := max(info.Size()-logTailBytes, 0)
	buf := make([]byte, info.Size()-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	buf = buf[:n]
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	out := make([]logLine, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			out = append(out, parseLogLine(line))
		}
	}
	return out, info.Size(), nil
}

// Follows one log: its newest lines last, read again every second while it is in view, / narrowing them and the level key the weight shown
func (s *systemHub) followLog(log systemLog) tea.Cmd {
	var lines, shown []logLine
	var size int64 = -1
	level, ticking := 0, false
	var list *cards.List
	t := s.a.toolkit
	read := func() tea.Cmd {
		next, n, err := tailLog(log.path)
		if err != nil {
			return s.h.Fail(err.Error())
		}
		if n != size {
			size, lines = n, next
			list.Refresh()
		}
		return nil
	}
	var tick func() tea.Cmd
	tick = func() tea.Cmd {
		return tea.Tick(logPollEvery, func(time.Time) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd {
				if !s.h.Shown() || s.h.Top() != wm.Card(list) {
					ticking = false
					return nil
				}
				return tea.Batch(read(), tick())
			}}
		})
	}
	list = cards.NewList(s.a.hubEnv(), cards.ListSpec{
		Title: log.name, Key: "tui:system:log", Context: systemContext, Shared: []string{hubContext}, Primary: "hub.use", Follow: true,
		Columns: []render.Column{{Title: tr(txt.SystemColTime)}, {Title: tr(txt.SystemColLevel)}, {Title: tr(txt.SystemColLine), Flex: 1}},
		Yield:   []int{0},
		Rows: func() [][]string {
			shown = shown[:0]
			for _, l := range lines {
				if words := logLevels[level].words; words == nil || slices.Contains(words, l.level) {
					shown = append(shown, l)
				}
			}
			out := make([][]string, len(shown))
			for i, l := range shown {
				lvl := l.level
				switch lvl {
				case "ERROR", "CRITICAL", "FATAL":
					lvl = render.Bad(lvl, t)
				case "WARNING", "WARN":
					lvl = render.Warm(lvl, t)
				case "DEBUG":
					lvl = render.Dim(lvl, t)
				}
				out[i] = []string{l.clock, lvl, l.text}
			}
			return out
		},
		Status: func() string { return tr(logLevels[level].label) + " · " + render.Bytes(max(size, 0)) },
		Empty:  func() string { return tr(txt.SystemLogsNoLines) },
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "tui.system.level"}, {Action: "hub.use", Label: tr(txt.SystemLogsShowLine)}}
		},
		Free: map[string]bool{"tui.system.level": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.system.level": func(l *cards.List, _ int) tea.Cmd {
				level = (level + 1) % len(logLevels)
				l.Refresh()
				l.Move(len(lines))
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { size = -1; return read() },
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(shown) {
					return nil
				}
				l := shown[i]
				return s.h.Push(cards.NewPage(s.a.hubEnv(), cards.PageSpec{Title: log.name, Key: "tui:system:line",
					Lines: func(int) []string { return []string{strings.TrimSpace(l.clock + " " + l.level), "", l.text} }}))
			},
		},
		OnFocus: func(*cards.List) tea.Cmd {
			if ticking {
				return read()
			}
			ticking = true
			return tea.Batch(read(), tick())
		},
	})
	return s.h.Push(list)
}

// One top level part of the home and the bytes it holds
type systemPart struct {
	name  string
	bytes int64
	dir   bool
}

// What the known parts of a home hold, in plain words
var homeParts = map[string]txt.Key{
	"state.db": txt.SystemStoragePartSessions, "state.db-wal": txt.SystemStoragePartSessions, "checkpoints": txt.SystemStoragePartCheckpoints,
	"logs": txt.SystemStoragePartLogs, "cache": txt.SystemStoragePartCache, "image_cache": txt.SystemStoragePartCache, "audio_cache": txt.SystemStoragePartCache,
	"skills": txt.SystemStoragePartSkills, "plugins": txt.SystemStoragePartPlugins, "profiles": txt.SystemStoragePartProfiles, "backups": txt.SystemStoragePartBackups,
	"memories": txt.SystemStoragePartMemory, "kanban.db": txt.SystemStoragePartKanban, "cron": txt.SystemStoragePartCron, "sessions": txt.SystemStoragePartSessionFiles,
	"pm": txt.SystemStoragePartPackages, "venv": txt.SystemStoragePartPackages, "environments": txt.SystemStoragePartPackages, "snapshots": txt.SystemStoragePartBackups,
}

// Measures the parts of the profile home off the update loop, the screen showing the sizes once they are in
func (s *systemHub) measure() tea.Cmd {
	dir := home.Dir(s.a.ctx)
	s.measured = false
	return func() tea.Msg {
		entries, err := os.ReadDir(dir)
		var parts []systemPart
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			part := systemPart{name: e.Name(), dir: e.IsDir()}
			if e.IsDir() {
				_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, walkErr error) error {
					if walkErr != nil || d.IsDir() {
						return nil
					}
					if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
						part.bytes += info.Size()
					}
					return nil
				})
			} else if info, err := e.Info(); err == nil {
				part.bytes = info.Size()
			}
			parts = append(parts, part)
		}
		slices.SortStableFunc(parts, func(x, y systemPart) int {
			switch {
			case x.bytes > y.bytes:
				return -1
			case x.bytes < y.bytes:
				return 1
			}
			return 0
		})
		return asyncDoneMsg{run: func() tea.Cmd {
			if err != nil {
				return s.h.Fail(err.Error())
			}
			s.parts, s.measured = parts, true
			return nil
		}}
	}
}

// One line on the home's size, measuring while it is not in
func (s *systemHub) storageState() string {
	if !s.measured {
		return tr(txt.SystemStorageMeasuring)
	}
	var total int64
	for _, p := range s.parts {
		total += p.bytes
	}
	return render.Bytes(total)
}

// Storage: the biggest parts of the home, then the maintenance of the session store and of the file snapshots
func (s *systemHub) openStorage() tea.Cmd {
	detail := func() []string {
		out := []string{tr(txt.SystemStorageAbout, home.Dir(s.a.ctx)), ""}
		if !s.measured {
			return append(out, tr(txt.SystemStorageMeasuring))
		}
		for _, p := range s.parts[:min(len(s.parts), 10)] {
			name := p.name
			if p.dir {
				name += string(filepath.Separator)
			}
			what := ""
			if key, ok := homeParts[p.name]; ok {
				what = tr(key)
			}
			out = append(out, "  "+padTo(render.Bytes(p.bytes), 9)+"  "+padTo(name, 22)+"  "+what)
		}
		return out
	}
	return s.section(tr(txt.SystemStorageTitle), detail,
		s.cmd(txt.SystemStorageStats, txt.SystemStorageStatsDoes, []string{"sessions", "stats"}, ""),
		s.cmd(txt.SystemStorageCheckpoints, txt.SystemStorageCheckpointsDoes, []string{"checkpoints", "status"}, ""),
		s.cmd(txt.SystemStoragePrune, txt.SystemStoragePruneDoes, []string{"checkpoints", "prune", "--force"}, tr(txt.SystemStoragePruneConfirm)),
		s.cmd(txt.SystemStorageClearLegacy, txt.SystemStorageClearLegacyDoes, []string{"checkpoints", "clear-legacy", "--force"}, tr(txt.SystemStorageClearLegacyConfirm)),
		s.cmd(txt.SystemStorageClear, txt.SystemStorageClearDoes, []string{"checkpoints", "clear", "--force"}, tr(txt.SystemStorageClearConfirm)),
		s.cmd(txt.SystemStorageOptimize, txt.SystemStorageOptimizeDoes, []string{"sessions", "optimize-storage", "--yes"}, ""),
		s.cmd(txt.SystemStorageRepair, txt.SystemStorageRepairDoes, []string{"sessions", "repair"}, ""),
		s.cmd(txt.SystemStorageCleanMarkers, txt.SystemStorageCleanMarkersDoes, []string{"sessions", "clean-markers"}, tr(txt.SystemStorageCleanMarkersConfirm)),
		s.cmd(txt.SystemStorageAdopt, txt.SystemStorageAdoptDoes, []string{"sessions", "adopt"}, ""),
		s.cmd(txt.SystemStorageRefresh, txt.SystemStorageRefreshDoes, []string{"sessions", "refresh-prompts"}, ""),
		s.cmd(txt.SystemStorageRetitleCheck, txt.SystemStorageRetitleCheckDoes, []string{"sessions", "retitle-skills"}, ""),
		s.cmd(txt.SystemStorageRetitle, txt.SystemStorageRetitleDoes, []string{"sessions", "retitle-skills", "--apply"}, tr(txt.SystemStorageRetitleConfirm)),
		s.cmd(txt.SystemStorageProfilesCheck, txt.SystemStorageProfilesCheckDoes, []string{"sessions", "repair-profiles"}, ""),
		s.cmd(txt.SystemStorageRoutingCheck, txt.SystemStorageRoutingCheckDoes, []string{"sessions", "repair-routing"}, ""),
		s.cmd(txt.SystemStorageRouting, txt.SystemStorageRoutingDoes, []string{"sessions", "repair-routing", "--apply"}, tr(txt.SystemStorageRoutingConfirm)),
		s.cmd(txt.SystemStoragePromptsCheck, txt.SystemStoragePromptsCheckDoes, []string{"sessions", "repair-prompts"}, ""),
		s.cmd(txt.SystemStoragePrompts, txt.SystemStoragePromptsDoes, []string{"sessions", "repair-prompts", "--apply"}, tr(txt.SystemStoragePromptsConfirm)),
		func() []hubAction {
			return s.asking(tr(txt.SystemStorageJournal), tr(txt.SystemStorageJournalDoes), tr(txt.SystemStorageJournalAsk), []string{"sessions", "set-journal-mode"})
		},
	)
}

// Text padded with spaces to n cells, longer text left whole
func padTo(text string, n int) string {
	if w := len([]rune(text)); w < n {
		return text + strings.Repeat(" ", n-w)
	}
	return text
}

// /update: the update check inside, or the update queued for after exit with the updates screen open to quit now
func (a *app) updateSlash(r *slashRun) tea.Cmd {
	if strings.EqualFold(trimJS(r.arg), "check") {
		return tea.Batch(r.ran(), a.openSystem("updates check"))
	}
	if !updateQueued() {
		if err := queueAfterExit([]string{"update"}, tr(txt.SystemUpdatesReason)); err != nil {
			r.sys(errorLine(tr(txt.AfterExitQueueFailed, err.Error())))
			return nil
		}
	}
	return tea.Batch(r.ran(), a.openSystem("updates"))
}
