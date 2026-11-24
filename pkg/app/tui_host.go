package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Launch variables that must not leak into a gateway owner the bootstrap starts
var tuiBootstrapScrubbedEnv = []string{"ICEBATH_MODEL", "ICEBATH_INFERENCE_MODEL", "ICEBATH_TUI_PROVIDER",
	"ICEBATH_INFERENCE_PROVIDER", "ICEBATH_TUI_TOOLSETS", "ICEBATH_TUI_SKILLS",
	"ICEBATH_CWD", "TERMINAL_CWD", "ICEBATH_YOLO", "ICEBATH_ACCEPT_HOOKS",
	"ICEBATH_TUI_CHECKPOINTS", "ICEBATH_TUI_PASS_SESSION_ID", "ICEBATH_KANBAN_BOARD",
	"ICEBATH_TUI_CLIENT_ENV", "ICEBATH_TUI_BASE_URL", "ICEBATH_TUI_API_KEY", "ICEBATH_TUI_REASONING"}

// Host work the terminal UI hands to the launcher, owner attach, snapshot restore and worktree settling, with the window settings of the home
func terminalUIHost(ctx context.Context) tui.Host {
	return tui.Host{Attach: tuiAttachOwner, RestoreSnapshot: tuiRestoreSnapshot, SettleWorktrees: settleTUIWorktrees, ReplaceOwner: tuiReplaceOwner,
		Prefs: terminalUIPrefs(ctx)}
}

// Settings of the window resolved for the home of ctx, which the terminal UI cannot read itself
func terminalUIPrefs(ctx context.Context) *tui.Prefs {
	var sequence *bool
	if on, set := settings.DisplayTuiClipboardSequence.Lookup(ctx); set {
		sequence = &on
	}
	return &tui.Prefs{
		ClipboardSequence: sequence,
		Theme:             settings.Theme.Get(ctx),
		ReducedMotion:     settings.DisplayTuiReducedMotion.Get(ctx),
		ScrollSpeed:       settings.DisplayTuiScrollSpeed.Get(ctx),
		TermuxLayout:      settings.DisplayTuiTermuxMode.Get(ctx),
		RequestTimeout:    settings.DisplayTuiRequestTimeout.Get(ctx),
		StartupTimeout:    settings.DisplayTuiStartupTimeout.Get(ctx),
		NoConfirm:         !settings.ConfirmDestructive.Get(ctx),
		CPUProfile:        settings.DiagnosticsCPUProfile.Get(ctx),
		FrameRate:         settings.DiagnosticsFrameRate.Get(ctx),
		FrameLog:          settings.DiagnosticsFrameLog.Get(ctx),
		FrameLogFloor:     settings.DiagnosticsFrameLogThreshold.Get(ctx),
		FrameLogFile:      settings.DiagnosticsFrameLogFile.Get(ctx),
		HeapDumpDir:       settings.DiagnosticsHeapdumpDir.Get(ctx),
		AutoHeapProfile:   settings.DiagnosticsAutoHeapProfile.Get(ctx),
		HeapDumpMaxBytes:  int64(settings.DiagnosticsHeapdumpMaxBytes.Get(ctx)),
		HeapDumpCooldown:  settings.DiagnosticsHeapdumpCooldown.Get(ctx),
		HeapDumpOnStart:   settings.DiagnosticsHeapdumpOnStart.Get(ctx),
	}
}

// Stops the home's background service of another build, busy or not, so the window's reconnect starts one of this build
func tuiReplaceOwner(ctx context.Context) error {
	h, err := tuiRealHome(ctx)
	if err != nil {
		return err
	}
	if refusal := gateway.ReplaceIncompatibleOwner(ctx, h, pythread.Monotonic()+20); refusal != "" {
		return errors.New(cmp.Or(replaceOwnerRefusals[refusal], refusal))
	}
	return nil
}

// Why the background service of another build did not stop, in plain words, by refusal code
var replaceOwnerRefusals = map[string]string{
	"owner_newer":        "it runs a newer icebath",
	"foreign_install":    "another icebath install started it",
	"owner_unverified":   "it could not be identified",
	"owner_did_not_exit": "it did not stop in time",
	"owner_unreachable":  "it did not answer",
}

// Verdict line the TUI shows when no owner can be attached, each detail line kept a verdict
func tuiBootstrapFailureMessage(receipt gateway.ObservedEndpoint) string {
	reason := receipt.ReasonCode
	if reason == "" {
		reason = "not ready"
	}
	head := fmt.Sprintf("gateway %s: %s", receipt.State, reason)
	if receipt.Detail == "" {
		return head
	}
	if receipt.ReasonCode == "runtime_exited" {
		return head + "\n" + receipt.Detail
	}
	out := []string{head}
	for _, line := range strings.Split(strings.ReplaceAll(receipt.Detail, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, fmt.Sprintf("gateway %s: %s", receipt.State, line))
		}
	}
	return strings.Join(out, "\n")
}

// Answer of the control socket to a session ticket request
type ticketAnswer struct {
	OK     bool `json:"ok"`
	ID     int  `json:"id"`
	Result struct {
		InstanceID string  `json:"instance_id"`
		ProfileID  string  `json:"profile_id"`
		Ticket     *string `json:"ticket"`
	} `json:"result"`
}

// Real path of the profile home the terminal UI runs on
func tuiRealHome(ctx context.Context) (string, error) {
	abs, err := filepath.Abs(home.Dir(ctx))
	if err != nil {
		return "", err
	}
	return realHome(abs)
}

// Real path of a home, a missing tail kept as written under its resolved parent
func realHome(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	resolved, err := realHome(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

// Ensures or discovers the gateway owner and mints the private interactive grant the terminal UI attaches with
func tuiAttachOwner(ctx context.Context, start, recover bool) (tui.Grant, error) {
	h, err := tuiRealHome(ctx)
	if err != nil {
		return tui.Grant{}, err
	}
	for _, key := range tuiBootstrapScrubbedEnv {
		_ = os.Unsetenv(key)
	}
	var receipt gateway.ObservedEndpoint
	if start {
		if receipt, err = gateway.EnsureGatewayRuntime(ctx, h, 30, true); err != nil {
			return tui.Grant{}, err
		}
	} else {
		receipt = gateway.DiscoverGatewayEndpoint(ctx, h, 5*time.Second)
	}
	if !start && recover && !gateway.Attachable(receipt) {
		if receipt, err = gateway.EnsureGatewayRuntime(ctx, h, 30, true); err != nil {
			return tui.Grant{}, err
		}
	}
	replaced := ""
	if gateway.Attachable(receipt) {
		if receipt, replaced, err = replaceOtherBuildOwner(ctx, h, receipt); err != nil {
			return tui.Grant{}, err
		}
	}
	if !gateway.Attachable(receipt) {
		return tui.Grant{}, errors.New(tuiBootstrapFailureMessage(receipt))
	}
	endpoint := receipt.Endpoint
	request, err := json.Marshal(map[string]any{"protocol": 1, "id": 1, "verb": "session-ticket",
		"params": map[string]any{"profile_id": endpoint.ProfileID, "instance_id": endpoint.InstanceID, "purpose": "interactive"}})
	if err != nil {
		return tui.Grant{}, err
	}
	raw, err := gateway.TUIBootstrapControlQuery(gateway.ControlHomeFor(h, endpoint), append(request, '\n'), 5*time.Second)
	if err != nil {
		return tui.Grant{}, err
	}
	var answer ticketAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return tui.Grant{}, err
	}
	granted := answer.OK && answer.ID == 1 && answer.Result.Ticket != nil &&
		answer.Result.InstanceID == endpoint.InstanceID && answer.Result.ProfileID == endpoint.ProfileID
	if !granted {
		return tui.Grant{}, errors.New("gateway private bootstrap rejected")
	}
	url, protocols := gateway.GatewayWSTarget(endpoint, *answer.Result.Ticket)
	return tui.Grant{URL: url, Protocols: protocols, ProfileID: endpoint.ProfileID, InstanceID: endpoint.InstanceID, Replaced: replaced}, nil
}

// Gateway builds this process replaced once and saw come back, kept from then on
var reproducedBuilds sync.Map

// Whether the owner reports no attached client and no running work, false when it cannot tell
func ownerIdle(h string, endpoint *gateway.GatewayEndpoint) bool {
	work := gateway.QueryGatewayControl(gateway.ControlHomeFor(h, endpoint), "active-work", nil, 5*time.Second)
	if work == nil {
		return false
	}
	agents, _ := work.Get("active_agents")
	clients, _ := work.Get("clients")
	return agents == int64(0) && clients == int64(0)
}

// Owner of another build or contract replaced by one of this build while idle and alone, with the build it replaced
func replaceOtherBuildOwner(ctx context.Context, h string, receipt gateway.ObservedEndpoint) (gateway.ObservedEndpoint, string, error) {
	other := receipt.Endpoint
	if tuigateway.SameBuild(other.BuildID) {
		return receipt, "", nil
	}
	if _, reproduced := reproducedBuilds.Load(other.BuildID); reproduced || !ownerIdle(h, other) {
		return receipt, "", nil
	}
	refusal := gateway.ReplaceIncompatibleOwner(ctx, h, pythread.Monotonic()+20)
	fresh, err := gateway.EnsureGatewayRuntime(ctx, h, 30, true)
	if err != nil || !gateway.Attachable(fresh) {
		return fresh, "", err
	}
	if refusal != "" || !tuigateway.SameBuild(fresh.Endpoint.BuildID) {
		reproducedBuilds.Store(fresh.Endpoint.BuildID, true)
		return fresh, "", nil
	}
	if other.BuildID == "" {
		return fresh, "unknown", nil
	}
	return fresh, other.BuildID, nil
}

// Whether the state database of the home holds the session id
func sessionInState(h, sid string) (bool, error) {
	db, err := state.OpenHome(h, state.Options{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer state.CloseAllUnder(h)
	id, err := db.ResolveSessionID(sid)
	return id == sid, err
}

// Restores a quick snapshot, with handoff the profile handed off the owner after release dropped the UI socket
func tuiRestoreSnapshot(ctx context.Context, id, session string, handoff bool, release func()) tui.SnapshotRestore {
	snap := gatewaycli.SnapshotCommand{Home: home.Dir(ctx), T: func(key string, kwargs map[string]any) string {
		return i18n.T(ctx, "cli.commands."+key, "", kwargs)
	}}
	report := func(restored bool, err error, onGateway bool) []string {
		lines, reportErr := snap.RestoreReport(id, restored, err, onGateway)
		if reportErr != nil {
			return []string{"/snapshot: " + reportErr.Error()}
		}
		return lines
	}
	held := false
	var targetErr error
	if handoff {
		dir, err := backup.QuickSnapshotDir(id, snap.Home)
		held, targetErr = dir != "", err
	}
	if targetErr != nil || !held {
		restored, err := false, targetErr
		if err == nil {
			restored, err = backup.RestoreQuickSnapshot(ctx, id, snap.Home)
		}
		return tui.SnapshotRestore{Lines: report(restored, err, handoff), SessionKept: true}
	}
	res, err := gatewaycli.RestoreSnapshotOffGateway(ctx, snap.Home, id, manualGatewayIdentity, func(h string) error {
		release()
		state.CloseAllUnder(h)
		return nil
	})
	out := tui.SnapshotRestore{Lines: report(res.Restored, err, true), Released: res.Detached, SessionKept: true}
	if line := res.ReserveLine(); line != "" {
		out.Lines = append(out.Lines, line)
	}
	if res.Restored && session != "" {
		kept, probeErr := sessionInState(snap.Home, session)
		switch {
		case probeErr != nil:
			out.Lines = append(out.Lines, "/snapshot: "+probeErr.Error())
		case !kept:
			out.SessionKept = false
			out.Lines = append(out.Lines, "This session is not part of the restored state.")
		}
	}
	return out
}

// Settles the trees /worktree new made once the terminal UI is gone, newest first, asking about the active one
func settleTUIWorktrees(ctx context.Context, trees []tui.Worktree, active string) {
	echo := func(text string) { fmt.Fprintln(os.Stdout, text) }
	for i := len(trees) - 1; i >= 0; i-- {
		tree := map[string]string{"path": trees[i].Path, "branch": trees[i].Branch, "repo_root": trees[i].RepoRoot}
		choice := ""
		if tree["path"] == active {
			if _, err := os.Stat(tree["path"]); err == nil {
				info := map[string]any{"path": tree["path"], "branch": tree["branch"], "repo_root": tree["repo_root"]}
				tools.AskExitChoice(ctx, info)
				choice, _ = info["exit_choice"].(string)
			}
		}
		tuiserver.SettleWorktree(ctx, tree, choice, echo)
	}
}
