package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	maxChangedPathsInNudge         = 8
	maxStatusSummaryChars          = 1200
	maxUnattributedCommandChars    = 200
	defaultVerifyOnStopMaxAttempts = 2
	unattributedRunWarning         = " Your last check run (`%s`) did not count: the shell could not tell the " +
		"check's own exit status apart from the commands around it. Run the check directly; " +
		"if you pipe it, keep the check as the first command in the pipeline. The terminal " +
		"already truncates long output and spills the rest to a file."
)

var (
	nonCodeVerifyExtensions = map[string]bool{".md": true, ".markdown": true, ".mdx": true, ".rst": true, ".txt": true, ".text": true,
		".adoc": true, ".asciidoc": true, ".org": true, ".log": true, ".csv": true, ".tsv": true}
	nonCodeVerifyFilenames = map[string]bool{"license": true, "licence": true, "notice": true, "authors": true,
		"contributors": true, "changelog": true, "codeowners": true}
	verifyFalsyTokens  = map[string]bool{"0": true, "false": true, "no": true, "off": true}
	verifyTruthyTokens = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
)

// Readonly profile config the verify on stop switch reads, swapped by tests
var verifyOnStopConfig = func(ctx context.Context) map[string]any {
	h := home.Dir(ctx)
	tree, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return map[string]any{}
	}
	return tree
}

// True when a changed path is documentation or prose with nothing to verify
func isNonCodeVerifyPath(raw string) bool {
	suffix := pystr.Lower(pypath.PathSuffix(raw))
	return nonCodeVerifyExtensions[suffix] || (suffix == "" && nonCodeVerifyFilenames[pystr.Lower(pypath.PathName(raw))])
}

// Edit then verify before finish switch from the env var, then agent.verify_on_stop, default off
func verifyOnStopEnabled(ctx context.Context, env *tools.Env, cfg map[string]any) bool {
	if v, ok := os.LookupEnv("ICEBATH_VERIFY_ON_STOP"); ok {
		return !verifyFalsyTokens[pystr.Lower(pystr.Strip(v))]
	}
	if cfg == nil {
		cfg = verifyOnStopConfig(ctx)
	}
	agentCfg, _ := cfg["agent"].(map[string]any)
	cfgVal := agentCfg["verify_on_stop"]
	if b, ok := cfgVal.(bool); ok {
		return b
	}
	token := ""
	if s, ok := cfgVal.(string); ok {
		token = pystr.Lower(pystr.Strip(s))
	}
	if token == "auto" {
		return !tools.SessionIsMessagingSurface(env)
	}
	return verifyTruthyTokens[token]
}

// Distinct resolved directories of the edited paths in order, a file's parent for files
func verifyCandidateCwds(paths []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range paths {
		if raw == "" {
			continue
		}
		p, err := pypath.PathExpanduser(raw)
		if err != nil {
			continue
		}
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			p = pypath.PathParent(p)
		}
		resolved, err := pypath.Resolve(p)
		if err != nil {
			continue
		}
		if !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
	}
	return out
}

// Status of a verification record, unverified when absent
func verifyStatusName(status map[string]any) string {
	if v := status["status"]; pyTruthy(v) {
		return pyrepr.Str(v)
	}
	return "unverified"
}

// Status and facts of the first edited workspace needing proof, else the first recognized workspace
func verificationSnapshot(ctx context.Context, env *tools.Env, sessionID string, changedPaths []string) (map[string]any, map[string]any, error) {
	var firstStatus, firstFacts map[string]any
	for _, cwd := range verifyCandidateCwds(changedPaths) {
		facts := tools.ProjectFactsFor(cwd)
		if len(facts) == 0 {
			continue
		}
		status, err := verificationStatus(ctx, env, sessionID, cwd)
		if err != nil {
			return nil, nil, err
		}
		if firstFacts == nil {
			firstStatus, firstFacts = status, facts
		}
		if verifyStatusName(status) != "passed" {
			return status, facts, nil
		}
	}
	return firstStatus, firstFacts, nil
}

// Bulleted changed paths, capped with a count of the rest
func formatChangedPaths(paths []string) string {
	lines := []string{}
	for _, p := range paths[:min(len(paths), maxChangedPathsInNudge)] {
		lines = append(lines, "- `"+p+"`")
	}
	if len(paths) > maxChangedPathsInNudge {
		lines = append(lines, fmt.Sprintf("- ... and %d more", len(paths)-maxChangedPathsInNudge))
	}
	return strings.Join(lines, "\n")
}

// Static recipe detector the nudge consults, swapped by tests that break it
var verifyDetectRecipe = verify.DetectRecipe

// Whether icebath verify has a saved manifest or a detected recipe with a start command here
func workspaceHasRunnableRecipe(root any) bool {
	if !pyTruthy(root) {
		return false
	}
	rootPath := pyrepr.Str(root)
	if info, err := os.Stat(verify.ManifestPath(rootPath)); err == nil && info.Mode().IsRegular() {
		return true
	}
	recipe, err := verifyDetectRecipe(rootPath)
	return err == nil && recipe != nil && recipe.Start != nil && *recipe.Start != ""
}

// Status line with the last command and its elided output when evidence exists
func verifyStatusDetail(status map[string]any) string {
	name := verifyStatusName(status)
	evidence, _ := status["evidence"].(map[string]any)
	if len(evidence) == 0 {
		return name
	}
	command := evidence["canonical_command"]
	if !pyTruthy(command) {
		command = evidence["command"]
	}
	summary := ""
	if v := evidence["output_summary"]; pyTruthy(v) {
		summary = pystr.Strip(pyrepr.Str(v))
	}
	parts := []string{name}
	if pyTruthy(command) {
		parts = append(parts, "last command `"+pyrepr.Str(command)+"`")
	}
	if summary != "" {
		parts = append(parts, "last output:\n"+tools.Elide(summary, maxStatusSummaryChars))
	}
	return strings.Join(parts, "\n")
}

// Synthetic follow up when edited code lacks fresh verification, empty when none is due
func buildVerifyOnStopNudge(ctx context.Context, env *tools.Env, sessionID string, changedPaths []string, attempts, maxAttempts int) (string, error) {
	unique := map[string]bool{}
	for _, p := range changedPaths {
		if p != "" && !isNonCodeVerifyPath(p) {
			unique[p] = true
		}
	}
	paths := make([]string, 0, len(unique))
	for p := range unique {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) == 0 || attempts >= maxAttempts {
		return "", nil
	}
	status, facts, err := verificationSnapshot(ctx, env, sessionID, paths)
	if err != nil || facts == nil {
		return "", err
	}
	if verifyStatusName(status) == "passed" {
		return "", nil
	}
	evidence, _ := status["evidence"].(map[string]any)
	runWarning := ""
	if cmd := lastUnattributedRun(status["session_id"], status["root"], evidence["created_at"]); cmd != "" {
		runWarning = fmt.Sprintf(unattributedRunWarning, tools.Elide(cmd, maxUnattributedCommandChars))
	}
	verifyCommands := []string{}
	if cmds, ok := facts["verifyCommands"].([]string); ok {
		for _, c := range cmds {
			if c = pystr.Strip(c); c != "" {
				verifyCommands = append(verifyCommands, c)
			}
		}
	}
	hasRecipe := workspaceHasRunnableRecipe(facts["root"])
	addendum := ""
	if guidance := verify.CodingVerifyGuidance(nil); guidance != "" {
		addendum = "\n\n" + guidance
	}

	var instruction string
	switch {
	case len(verifyCommands) > 0:
		quoted := []string{}
		for _, c := range verifyCommands[:min(len(verifyCommands), 3)] {
			quoted = append(quoted, "`"+c+"`")
		}
		more := ""
		if len(verifyCommands) > 3 {
			more = ", ..."
		}
		instruction = "Run the relevant verification command now (" + strings.Join(quoted, ", ") + more +
			"), read any failure, repair the code, and summarize what passed."
		if hasRecipe {
			instruction += " For a full check including a runtime boot (build + test + " +
				"start + readiness), prefer `icebath verify --json` — a passing " +
				"run records verification evidence for this workspace."
		}
	case hasRecipe:
		instruction = "No canonical test/lint/build command was detected, but the " +
			"project has a runnable verification recipe. Run `icebath verify " +
			"--json` (detect -> build -> test -> boot -> readiness poll); a " +
			"passing run records verification evidence for this workspace. " +
			"Read any failure, repair the code, and summarize what passed."
	default:
		tempDir, err := pypath.Gettempdir()
		if err != nil {
			return "", err
		}
		if tempDir, err = pypath.Realpath(tempDir, pypath.Lax); err != nil {
			return "", err
		}
		instruction = "No canonical test/lint/build command was detected. Create a focused " +
			"temporary verification script under `" + tempDir + "` using an OS-safe " +
			"`tempfile` path with a `icebath-verify-` filename prefix, run it " +
			"against the changed behavior, clean it up when possible, and " +
			"summarize it explicitly as ad-hoc verification rather than suite " +
			"green."
	}
	return "[System: You edited code in this turn, but the workspace does not have " +
		"fresh passing verification evidence yet.\n\n" +
		"Verification status: " + verifyStatusDetail(status) + "\n\n" +
		"Changed paths:\n" + formatChangedPaths(paths) + "\n\n" +
		instruction + runWarning + " If verification is not possible, " +
		"explain the concrete blocker instead of claiming the work is fully verified." +
		addendum + "]", nil
}
