package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
)

// True when a changed path is documentation or prose with nothing to verify
func isNonCodeVerifyPath(raw string) bool {
	suffix := strings.ToLower(pathutil.Suffix(raw))
	return nonCodeVerifyExtensions[suffix] || (suffix == "" && nonCodeVerifyFilenames[strings.ToLower(pathutil.Name(raw))])
}

// Edit then verify before finish switch from agent.verify_on_stop, auto meaning on outside chat apps
func verifyOnStopEnabled(ctx context.Context, env *tools.Env) bool {
	switch settings.AgentVerifyOnStop.Get(ctx) {
	case "on":
		return true
	case "auto":
		return !tools.SessionIsMessagingSurface(env)
	}
	return false
}

// Distinct resolved directories of the edited paths in order, a file's parent for files
func verifyCandidateCwds(paths []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range paths {
		if raw == "" {
			continue
		}
		p, err := pathutil.ExpandUserPath(raw)
		if err != nil {
			continue
		}
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			p = filepath.Dir(filepath.Clean(p))
		}
		resolved, err := pathutil.Resolve(p)
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
	if v := status["status"]; anyutil.IsTrue(v) {
		return anyutil.String(v)
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
	if !anyutil.IsTrue(root) {
		return false
	}
	rootPath := anyutil.String(root)
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
	if !anyutil.IsTrue(command) {
		command = evidence["command"]
	}
	summary := ""
	if v := evidence["output_summary"]; anyutil.IsTrue(v) {
		summary = strings.TrimSpace(anyutil.String(v))
	}
	parts := []string{name}
	if anyutil.IsTrue(command) {
		parts = append(parts, "last command `"+anyutil.String(command)+"`")
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
			if c = strings.TrimSpace(c); c != "" {
				verifyCommands = append(verifyCommands, c)
			}
		}
	}
	hasRecipe := workspaceHasRunnableRecipe(facts["root"])
	addendum := ""
	if guidance := verify.CodingVerifyGuidance(ctx); guidance != "" {
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
		tempDir, err := pathutil.TempDir()
		if err != nil {
			return "", err
		}
		if tempDir, err = pathutil.Resolve(tempDir); err != nil {
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
