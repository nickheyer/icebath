package tools

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/lsp"
)

// Profile level env of a session, the session values left out as the LSP loop never sees them
func lspProfileEnv(env *Env) *Env {
	if env == nil {
		return &Env{}
	}
	return &Env{Home: env.Home, Config: env.Config, Secrets: env.Secrets, Platform: env.Platform, Log: env.Log}
}

// Profile home a session's LSP service is keyed by
func lspHomeKey(env *Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Process()
}

// Current lsp config section of a session, empty when absent or malformed
func lspConfigSection(env *Env) map[string]any {
	if m, ok := cfgTree(env)["lsp"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Difflib opcodes of two line lists without autojunk
func lspOpcodes(a, b []string) []lsp.Opcode {
	ops := newSequenceMatcher[string](nil, a, b, false).getOpcodes()
	out := make([]lsp.Opcode, len(ops))
	for i, op := range ops {
		out[i] = lsp.Opcode{Tag: op.tag, I1: op.i1, I2: op.i2, J1: op.j1, J2: op.j2}
	}
	return out
}

// LSPRuntime wires the LSP layer to a session's profile, managed packages, child env and process ledger
func LSPRuntime(env *Env) *lsp.Runtime {
	profile := lspProfileEnv(env)
	return &lsp.Runtime{
		Home:             lspHomeKey(env),
		Log:              envLog(env),
		Config:           func() map[string]any { return lspConfigSection(profile) },
		EnvFor:           pmEnvFor,
		EnsureEnv:        pmEnsureEnv,
		InstalledBinary:  pmInstalledBinary,
		PythonTool:       pmPythonTool,
		EnsurePythonTool: pmEnsurePythonTool,
		SubprocessEnv:    func() map[string]string { return IcebathSubprocessEnv(profile, false, nil) },
		DelegatedChildEnv: func(child map[string]string) map[string]string {
			return delegatedChildSubprocessEnv(profile, child)
		},
		OwnProcessGroups: OwnProcessGroups,
		RegisterChild: func(ctx context.Context, pid int, purpose string) bool {
			return RegisterChild(WithEnvHome(ctx, env), pid, purpose, "")
		},
		ReleaseProcessGroupOnceEmpty: ReleaseProcessGroupOnceEmpty,
		Opcodes:                      lspOpcodes,
		DecodeJSON:                   lspDecodeJSON,
	}
}

// json.loads of an LSP body, Python's constants, integers and decode errors included
func lspDecodeJSON(text string) (any, error) {
	class, msg, jerr := pyJSONLoadsCheck(text)
	if jerr != nil {
		return nil, &lsp.JSONDecodeError{Msg: jerr.Error()}
	}
	if class != "" {
		return nil, lsp.PythonError(class, msg)
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	return lspJSONValue(v), nil
}

// Decoded value in the plain map, int64 and float64 shapes the LSP layer reads
func lspJSONValue(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		m := make(map[string]any, len(t.Keys))
		for _, k := range t.Keys {
			m[k] = lspJSONValue(t.Vals[k])
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = lspJSONValue(e)
		}
		return out
	case int64:
		return t
	case *big.Int:
		return json.Number(t.String())
	}
	return v
}

// Worktrees the operator pointed the session at, the launch dir and the surface set workspace, none for scheduled work
func lspOperatorWorkspaceRoots(env *Env) map[string]bool {
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		return map[string]bool{}
	}
	anchors := []func() (string, error){os.Getwd}
	if !sessionFlag(env, "cron", "ICEBATH_CRON_SESSION") {
		anchors = append(anchors, func() (string, error) { return ResolveAgentCwd(env) })
	}
	return lsp.OperatorWorkspaceRoots(anchors)
}

// Active LSP service of the session's profile, nil when disabled or its config cannot build one
func lspServiceFor(env *Env) lspService { return lspServiceLookup(env) }

// Session service lookup the write path consults, swapped by tests that stand in a fake service
var lspServiceLookup = func(env *Env) lspService {
	svc, err := LSPServiceFor(env)
	if err != nil || svc == nil {
		return nil
	}
	return &lspSessionService{svc: svc, env: env}
}

// LSPServiceFor returns the active LSP service of the session's profile, built on first use, nil when disabled
func LSPServiceFor(env *Env) (*lsp.Service, error) {
	return lsp.ServiceFor(lspHomeKey(env), func() (*lsp.Service, error) {
		return lsp.ServiceFromConfig(LSPRuntime(env), cfgTree(env)["lsp"], lspOperatorWorkspaceRoots(env))
	})
}

// LSPCommandContext is what icebath lsp reads from the session's profile
func LSPCommandContext(env *Env) lsp.CommandContext {
	return lsp.CommandContext{
		Runtime: LSPRuntime(env),
		Config:  func() map[string]any { return lspConfigSection(env) },
		Service: func() (*lsp.Service, error) { return LSPServiceFor(env) },
	}
}

// Service seen from one session, its operator roots noted on every gate it passes
type lspSessionService struct {
	svc *lsp.Service
	env *Env
}

func (w *lspSessionService) HandlesExtension(ext string) (bool, error) {
	return w.svc.HandlesExtension(ext), nil
}

func (w *lspSessionService) EnabledFor(path string) (bool, error) {
	return w.svc.EnabledFor(path, lspOperatorWorkspaceRoots(w.env)), nil
}

func (w *lspSessionService) StartEditCheck(path string, preText, postText *string, owner string, carry []lspNote) (lspEditFuture, error) {
	notes := make([]lsp.Note, len(carry))
	for i, n := range carry {
		notes[i] = lsp.Note{Text: n.Text, BaselineText: n.BaselineText}
	}
	fut, err := w.svc.StartEditCheck(path, preText, postText, owner, notes, lspOperatorWorkspaceRoots(w.env))
	if err != nil || fut == nil {
		return nil, err
	}
	return lspFuture{fut}, nil
}

// Running edit check as the write path consumes it
type lspFuture struct{ fut *lsp.EditFuture }

// Write path form of a check result
func lspCheckResult(r *lsp.CheckResult) *lspEditCheckResult {
	if r == nil {
		return nil
	}
	carry := make([]lspNote, len(r.Carry))
	for i, n := range r.Carry {
		carry[i] = lspNote{Text: n.Text, BaselineText: n.BaselineText}
	}
	return &lspEditCheckResult{Diagnostics: r.Diagnostics, HasVerdict: r.HasVerdict, BaselineText: r.BaselineText,
		Superseded: r.Superseded, Carry: carry}
}

func (f lspFuture) Wait(timeout time.Duration) (*lspEditCheckResult, bool, error) {
	res, ready, err := f.fut.Wait(timeout)
	return lspCheckResult(res), ready, err
}

func (f lspFuture) OnDone(fn func(res *lspEditCheckResult, cancelled bool, err error)) {
	f.fut.OnDone(func(res *lsp.CheckResult, cancelled bool, err error) { fn(lspCheckResult(res), cancelled, err) })
}

// Whether a built in server claims the extension
func lspStaticRegistryHandles(ext string) bool { return lsp.StaticRegistryHandles(ext) }

// lsp.trusted_workspaces as directories, a malformed value trusting nothing extra
func lspParseTrustedWorkspaces(raw any) []string { return lsp.ParseTrustedWorkspaces(nil, raw) }

// Whether a root lies in a trusted directory or an operator worktree
func lspIsTrustedWorkspace(root string, trusted []string, operatorRoots map[string]bool) bool {
	return lsp.IsTrustedWorkspace(root, trusted, operatorRoots)
}

// Time a write may still wait for its verdict inline, zero start meaning unknown
func lspInlineWait(started time.Time) time.Duration { return lsp.InlineWait(started) }

// Diagnostics block of a file, raising like the reporter does on malformed entries
func lspReportForFile(path string, diagnostics []map[string]any) string {
	block, err := lsp.ReportForFile(path, diagnostics, tomllibIsPrintable)
	if err != nil {
		panic(err)
	}
	return block
}

// Diagnostics summary capped with the elision marker
func lspTruncate(s string) string { return elide(s, lsp.MaxTotalChars) }
