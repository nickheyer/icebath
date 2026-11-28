// Package tools keeps the evidence that the code sessions change was checked: the checks their commands run, the files their writes land
// in, and the nudge that sends the agent back to check before it finishes a turn that changed code
package tools

import (
	"cmp"
	"context"
	"log/slog"
	"path/filepath"

	verifyconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/config"
	"github.com/nickheyer/icebath/internal/tools"
)

type registrant struct {
	tools  *tools.Registry
	ledger *Ledger
}

// Registers the ledger with the tools, telling the person through tell when it sets a ledger file aside; the ledger, which the verify
// command records its runs in
func Register(registry *tools.Registry, tell func(text string)) *Ledger {
	r := &registrant{tools: registry, ledger: newLedger(tell)}
	registry.Register(r)
	return r.ledger
}

func (r *registrant) Tools() []tools.Def { return nil }

func (r *registrant) Close() error { return r.ledger.Close() }

// A foreground command finished: recorded when it checks the session's workspace
func (r *registrant) Verified(env *tools.Env, run tools.Run) *tools.Evidence {
	if env == nil || !Enabled(env.Config, env.Platform) {
		return nil
	}
	e, err := r.ledger.recordRun(context.Background(), env.Config, run)
	if err != nil {
		slog.Debug("verification evidence recording failed", "error", err)
		return nil
	}
	if e == nil {
		return nil
	}
	return &tools.Evidence{Status: e.Status, Kind: e.Kind, Scope: e.Scope, Canonical: e.Canonical}
}

// The turn is about to end having changed files: the nudge when they lack fresh passing evidence
func (r *registrant) Unverified(env *tools.Env, session string, changed []string) string {
	if env == nil || !Enabled(env.Config, env.Platform) {
		return ""
	}
	nudge, err := r.ledger.nudge(context.Background(), env.Config, session, changed)
	if err != nil {
		slog.Debug("verification stop-loop check failed", "error", err)
		return ""
	}
	return nudge
}

// Most times a turn may be sent back by the hooks that check its work, never below zero
func (r *registrant) Nudges(env *tools.Env) int {
	if env == nil || env.Config == nil {
		return 0
	}
	return max(env.Config.GetInt(verifyconfig.MaxNudges), 0)
}

// A call returned: the files a write landed in make the evidence of their workspace stale
func (r *registrant) Finished(call *tools.Call, result string) {
	env := call.Env
	if env == nil || !Enabled(env.Config, env.Platform) {
		return
	}
	def, ok := r.tools.Def(call.Name)
	if !ok {
		return
	}
	var paths, dirs []string
	for _, path := range tools.Landed(def, call, result) {
		if !filepath.IsAbs(path) && env.Cwd != "" {
			path = filepath.Join(env.Cwd, path)
		}
		paths = append(paths, resolve(path))
		dirs = append(dirs, filepath.Dir(resolve(path)))
	}
	if len(paths) == 0 {
		return
	}
	dir := ""
	for _, parent := range dirs {
		if _, ok := tools.ProjectOf(parent); ok {
			dir = parent
			break
		}
	}
	dir = cmp.Or(dir, cmp.Or(env.Cwd, dirs[0]))
	if err := r.ledger.edited(context.Background(), env.Config, call.SessionID, dir, paths); err != nil {
		slog.Debug("verification stale marker failed", "error", err)
	}
}

// The agent checks the changes it makes to files, so the options of that check sit with the group of the file tools
func (r *registrant) Groups() []tools.Group {
	return []tools.Group{{Title: "Files", Sections: []string{verifyconfig.Key}}}
}
