package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Launch variables that must not leak into a gateway owner the bootstrap starts
var tuiBootstrapScrubbedEnv = []string{"ICEBATH_MODEL", "ICEBATH_INFERENCE_MODEL", "ICEBATH_TUI_PROVIDER",
	"ICEBATH_INFERENCE_PROVIDER", "ICEBATH_TUI_TOOLSETS", "ICEBATH_TUI_SKILLS",
	"ICEBATH_CWD", "TERMINAL_CWD", "ICEBATH_YOLO", "ICEBATH_ACCEPT_HOOKS",
	"ICEBATH_TUI_CHECKPOINTS", "ICEBATH_TUI_PASS_SESSION_ID", "ICEBATH_KANBAN_BOARD",
	"ICEBATH_TUI_CLIENT_ENV", "ICEBATH_TUI_BASE_URL", "ICEBATH_TUI_API_KEY", "ICEBATH_TUI_REASONING"}

// Host work the terminal UI hands to the launcher, owner attach, snapshot restore and worktree settling
func terminalUIHost() tui.Host {
	return tui.Host{Attach: tuiAttachOwner, RestoreSnapshot: tuiRestoreSnapshot, SettleWorktrees: settleTUIWorktrees}
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
	abs, err := filepath.Abs(home.Dir(ctx))
	if err != nil {
		return tui.Grant{}, err
	}
	h, err := realHome(abs)
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
	return tui.Grant{URL: url, Protocols: protocols, ProfileID: endpoint.ProfileID, InstanceID: endpoint.InstanceID}, nil
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
