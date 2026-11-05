package agent

import (
	"context"
	"os"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// EmptyResponseExplanation is the English source of the no reply after retries text every surface shows
const EmptyResponseExplanation = "{model} didn't produce a reply this time, even after retries. " +
	"Send `continue` to try again, or switch models with /model."

// Localized empty response explanation with the model name or the model filled in
func EmptyResponseExplanationFor(ctx context.Context, model string) string {
	if model == "" {
		model = i18n.T(ctx, "explainer.shared.the_model", "", nil)
	}
	return i18n.T(ctx, "explainer.empty_response", "", map[string]any{"model": model})
}

var exitReasonExplanations = map[string]string{
	"empty_response_exhausted":          "explainer.empty_response",
	"all_retries_exhausted_no_response": "explainer.exit.all_retries_exhausted_no_response",
	"partial_stream_recovery":           "explainer.exit.partial_stream_recovery",
	"fallback_prior_turn_content":       "explainer.exit.fallback_prior_turn_content",
	"redirect_restart_limit_exceeded":   "explainer.exit.redirect_restart_limit_exceeded",
	"rebuilt_restart_limit_exceeded":    "explainer.exit.rebuilt_restart_limit_exceeded",
	"budget_exhausted":                  "explainer.exit.budget_exhausted",
	"ollama_runtime_context_too_small":  "explainer.exit.ollama_runtime_context_too_small",
	"pending_tool_result":               "explainer.exit.pending_tool_result",
}

var exitReasonPrefixExplanations = []string{
	"interrupted_during_api_call", "max_iterations_reached", "error_near_max_iterations", "repeated_outer_errors",
}

var persistenceCauses = map[string]bool{
	"compression": true, "compression_closed": true, "turn_lease": true, "session_row_missing": true, "locked": true,
	"replaced": true, "deleted_wal": true, "corrupt": true, "fts_index": true, "disk": true,
}

func persistenceExplanationKey(cause string) string {
	if persistenceCauses[cause] {
		return "explainer.persistence." + cause
	}
	return "explainer.persistence.default"
}

// FileStat is the mtime in nanoseconds and size of a file mutation target
type FileStat struct {
	MtimeNS int64
	Size    int64
}

// FileMutationFailure is the first failure of a write_file or patch call on one model spelled path
type FileMutationFailure struct {
	Tool         string
	ErrorPreview string
	Identity     string
	HasStat      bool
	Stat         *FileStat
}

// FileMutationFailures keeps failed file mutations by model spelled path in insertion order
type FileMutationFailures struct {
	order  []string
	byPath map[string]*FileMutationFailure
}

// Empty per turn failed mutation record
func NewFileMutationFailures() *FileMutationFailures {
	return &FileMutationFailures{byPath: map[string]*FileMutationFailure{}}
}

// Number of failed paths
func (f *FileMutationFailures) Len() int { return len(f.order) }

// Failed paths in insertion order
func (f *FileMutationFailures) Paths() []string { return append([]string(nil), f.order...) }

// Failure recorded for a path
func (f *FileMutationFailures) Get(path string) (*FileMutationFailure, bool) {
	info, ok := f.byPath[path]
	return info, ok
}

// Stores a failure for a path unless one is already recorded
func (f *FileMutationFailures) SetDefault(path string, info *FileMutationFailure) {
	if _, ok := f.byPath[path]; ok {
		return
	}
	f.order = append(f.order, path)
	f.byPath[path] = info
}

// Removes the failure recorded for a path
func (f *FileMutationFailures) Pop(path string) {
	if _, ok := f.byPath[path]; !ok {
		return
	}
	delete(f.byPath, path)
	for i, p := range f.order {
		if p == path {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

// Per agent turn explainer state, the failed mutation record is nil outside run_conversation
type turnExplainersState struct {
	turnFailedFileMutations             *FileMutationFailures
	turnFileMutationPaths               map[string]bool
	fileMutationVerifierEnabledCache    *bool
	turnCompletionExplainerEnabledCache *bool
}

// One key per on disk target, the task resolved absolute path case folded on case insensitive hosts
func fileMutationIdentity(env *tools.Env, path, taskID string) (string, error) {
	if taskID == "" {
		taskID = "default"
	}
	resolved, err := tools.ResolvePathForTaskID(env, taskID, path)
	if err != nil {
		if resolved, err = pypath.Abspath(pypath.Expanduser(path)); err != nil {
			return "", err
		}
	}
	return pypath.Normcase(pypath.Normpath(resolved)), nil
}

// Modification time and size of the target, nil when it does not exist or cannot be read
func fileStatSignature(identity string) *FileStat {
	st, err := os.Stat(identity)
	if err != nil {
		return nil
	}
	return &FileStat{MtimeNS: st.ModTime().UnixNano(), Size: st.Size()}
}

// Reads display.<key> defaulting on, with the env var overriding every call and the config answer cached
func displayFlagEnabled(ctx context.Context, envVar, configKey string, cache **bool) bool {
	if env, ok := os.LookupEnv(envVar); ok {
		switch pystr.Lower(pystr.Strip(env)) {
		case "0", "false", "no", "off":
			return false
		}
		return true
	}
	if *cache != nil {
		return **cache
	}
	cfg, err := config.Load(home.Dir(ctx), os.LookupEnv)
	if err != nil {
		cfg = config.Tree{}
	}
	enabled := true
	display, ok := cfg["display"].(map[string]any)
	if tree, isTree := cfg["display"].(config.Tree); isTree {
		display, ok = tree, true
	}
	if ok {
		if v, present := display[configKey]; present {
			enabled = pyrepr.Truthy(v)
		}
	}
	*cache = &enabled
	return enabled
}

// Records a write_file or patch outcome for the turn end verifier, a later success on the same target clears it
func (a *AIAgent) recordFileMutationResult(toolName string, args map[string]any, result any, isError bool, taskID string) error {
	if !tools.FileMutatingToolNames[toolName] {
		return nil
	}
	failures := a.turnFailedFileMutations
	if failures == nil {
		return nil
	}
	targets := tools.ExtractFileMutationTargets(toolName, args)
	if len(targets) == 0 {
		return nil
	}
	landed := tools.FileMutationResultLanded(toolName, result)
	var landedPaths []string
	if landed {
		landedPaths = tools.ExtractLandedFileMutationPaths(toolName, args, result)
		if a.turnFileMutationPaths != nil {
			for _, p := range landedPaths {
				a.turnFileMutationPaths[p] = true
			}
		}
		taskKey := taskID
		if taskKey == "" {
			taskKey = "default"
		}
		if mgr := a.checkpointMgr; mgr != nil && mgr.Enabled && tools.ContainerBackendForTask(a.Env, taskKey) == "" {
			for _, p := range landedPaths {
				mgr.RecordAgentWrite(p)
			}
		}
	}
	if isError && !landed {
		preview := tools.ExtractErrorPreview(result, 180)
		for _, path := range targets {
			identity, err := fileMutationIdentity(a.Env, path, taskID)
			if err != nil {
				return err
			}
			failures.SetDefault(path, &FileMutationFailure{
				Tool: toolName, ErrorPreview: preview, Identity: identity, HasStat: true, Stat: fileStatSignature(identity),
			})
		}
		return nil
	}
	cleared := map[string]bool{}
	source := targets
	if landed {
		source = landedPaths
	}
	for _, p := range source {
		identity, err := fileMutationIdentity(a.Env, p, taskID)
		if err != nil {
			return err
		}
		cleared[identity] = true
	}
	for _, path := range failures.Paths() {
		info, _ := failures.Get(path)
		identity := info.Identity
		if identity == "" {
			var err error
			if identity, err = fileMutationIdentity(a.Env, path, taskID); err != nil {
				return err
			}
		}
		if cleared[identity] {
			failures.Pop(path)
		}
	}
	return nil
}

// Drops failures whose target changed on disk since the failed call, keeping entries without a snapshot
func fileMutationsStillFailed(failed *FileMutationFailures) *FileMutationFailures {
	out := NewFileMutationFailures()
	for _, path := range failed.Paths() {
		info, _ := failed.Get(path)
		if !info.HasStat || sameFileStat(fileStatSignature(info.Identity), info.Stat) {
			out.SetDefault(path, info)
		}
	}
	return out
}

func sameFileStat(a, b *FileStat) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Reads display.file_mutation_verifier with ICEBATH_FILE_MUTATION_VERIFIER overriding
func (a *AIAgent) fileMutationVerifierEnabled(ctx context.Context) bool {
	return displayFlagEnabled(ctx, "ICEBATH_FILE_MUTATION_VERIFIER", "file_mutation_verifier", &a.fileMutationVerifierEnabledCache)
}

// Reads display.turn_completion_explainer with ICEBATH_TURN_COMPLETION_EXPLAINER overriding
func (a *AIAgent) turnCompletionExplainerEnabled(ctx context.Context) bool {
	return displayFlagEnabled(ctx, "ICEBATH_TURN_COMPLETION_EXPLAINER", "turn_completion_explainer", &a.turnCompletionExplainerEnabledCache)
}

var footerPathRE = regexp2.MustCompile(
	`(?<![/:\p{L}\p{N}_.`+"`"+`])(?:~/|/|[A-Za-z]:[/\\])(?:[\p{L}\p{N}_.\-]+[/\\])*[\p{L}\p{N}_.\-]+\.[\p{L}\p{N}_]+`, regexp2.None)

// Backticks bare file paths so the gateway attachment extractor never auto attaches them
func neutralizeFooterPaths(text string) string {
	if text == "" {
		return text
	}
	out, err := footerPathRE.ReplaceFunc(text, func(m regexp2.Match) string { return "`" + m.String() + "`" }, -1, -1)
	if err != nil {
		return text
	}
	return out
}

// User facing footer listing up to ten failed mutation paths with their first error, empty when nothing failed
func formatFileMutationFailureFooter(ctx context.Context, failed *FileMutationFailures) string {
	if failed == nil || failed.Len() == 0 {
		return ""
	}
	lines := []string{i18n.T(ctx, "explainer.file_mutation.header", "", map[string]any{"count": failed.Len()})}
	paths := failed.Paths()
	shown := paths[:min(len(paths), 10)]
	for _, path := range shown {
		info, _ := failed.Get(path)
		preview := pystr.Strip(info.ErrorPreview)
		tool := info.Tool
		if tool == "" {
			tool = "patch"
		}
		if preview == "" {
			preview = i18n.T(ctx, "explainer.file_mutation.failed", "", nil)
		}
		lines = append(lines, i18n.T(ctx, "explainer.file_mutation.entry", "", map[string]any{"path": path, "tool": tool, "preview": preview}))
	}
	if remaining := len(paths) - len(shown); remaining > 0 {
		lines = append(lines, i18n.T(ctx, "explainer.shared.and_more", "", map[string]any{"count": remaining}))
	}
	return neutralizeFooterPaths(strings.Join(lines, "\n"))
}

// User facing explanation for an abnormal turn ending, empty for normal and unknown reasons
func formatTurnCompletionExplanation(ctx context.Context, turnExitReason any, persistenceCause, dbPath, model string) string {
	if !pyrepr.Truthy(turnExitReason) {
		return ""
	}
	reason := pyrepr.Str(turnExitReason)
	if strings.HasPrefix(reason, "text_response") {
		return ""
	}
	key, ok := exitReasonExplanations[reason]
	if !ok {
		for _, prefix := range exitReasonPrefixExplanations {
			if strings.HasPrefix(reason, prefix) {
				key, ok = "explainer.exit."+prefix, true
				break
			}
		}
	}
	body := ""
	switch {
	case ok:
		if model == "" {
			model = i18n.T(ctx, "explainer.shared.the_model", "", nil)
		}
		body = i18n.T(ctx, key, "", map[string]any{"model": model})
	case reason == "session_persistence_failed":
		h := home.Dir(ctx)
		fill := map[string]any{
			"home": home.Display(h), "profile_arg": provider.ProfileCLISelector(h),
			"recovery_docs": state.StorageRecoveryDocsURL, "db_path": "", "backups_dir": "",
		}
		if persistenceCause == "corrupt" || persistenceCause == "fts_index" {
			if dbPath == "" {
				dbPath = home.StatePath(h)
			}
			fill["db_path"] = dbPath
			fill["backups_dir"] = pypath.Join(home.RootOf(h), "backups")
		}
		body = i18n.T(ctx, persistenceExplanationKey(persistenceCause), "", fill)
	}
	if body == "" {
		return ""
	}
	return i18n.T(ctx, "explainer.no_reply_prefix", "", nil) + body
}
