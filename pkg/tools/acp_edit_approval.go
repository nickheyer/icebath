package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One proposed file edit an ACP client can be shown, with every file it actually touches
type EditProposal struct {
	ToolName  string
	Path      string
	OldText   *string
	NewText   string
	Arguments map[string]any
	Paths     []string
}

// Asks the editor whether a proposed edit may run
type EditApprovalRequester func(ctx context.Context, env *Env, proposal *EditProposal) (bool, error)

type editApprovalRequesterKey struct{}

// Binds an ACP edit approval requester for the calls made under ctx, nil leaving edits unguarded
func WithEditApprovalRequester(ctx context.Context, requester EditApprovalRequester) context.Context {
	return context.WithValue(ctx, editApprovalRequesterKey{}, requester)
}

// Requester bound to ctx, nil when none is
func boundEditApprovalRequester(ctx context.Context) EditApprovalRequester {
	if ctx == nil {
		return nil
	}
	requester, _ := ctx.Value(editApprovalRequesterKey{}).(EditApprovalRequester)
	return requester
}

const (
	AutoApproveAsk       = "ask"
	AutoApproveWorkspace = "workspace_session"
	AutoApproveSession   = "session"
)

var (
	sensitiveAutoApproveNames = map[string]bool{".env": true, ".env.local": true, ".env.production": true, "id_rsa": true, "id_ed25519": true}
	v4aFileRe                 = regexp.MustCompile(`(?m)^\*\*\*\s+(?:Update|Add|Delete)\s+File:\s*(.+)$`)
	v4aMoveRe                 = regexp.MustCompile(`(?m)^\*\*\*\s+Move\s+File:\s*(.+?)\s*->\s*(.+)$`)
)

// Text of a file decoded as utf-8-sig with replacement, nil when absent and an error for a non file path
func readEditTextIfExists(path string) (*string, error) {
	path = expandUser(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Cannot edit non-file path: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ToValidUTF8(strings.TrimPrefix(string(data), "\xef\xbb\xbf"), "\uFFFD")
	return &text, nil
}

// Task resolved path argument of an edit, refused when missing
func requiredEditPath(env *Env, arguments map[string]any) (string, error) {
	path := ""
	if raw := arguments["path"]; pyrepr.Truthy(raw) {
		path = pyrepr.Str(raw)
	}
	if path == "" {
		return "", errors.New("path required")
	}
	return resolvePathForTask(env, path)
}

// Shallow copy of tool arguments
func copiedArguments(arguments map[string]any) map[string]any {
	out := make(map[string]any, len(arguments))
	for k, v := range arguments {
		out[k] = v
	}
	return out
}

// Proposal of a write_file call
func proposalForWriteFile(env *Env, arguments map[string]any) (*EditProposal, error) {
	path, err := requiredEditPath(env, arguments)
	if err != nil {
		return nil, err
	}
	content, has := arguments["content"]
	if !has || content == nil {
		return nil, errors.New("content required")
	}
	old, err := readEditTextIfExists(path)
	if err != nil {
		return nil, err
	}
	return &EditProposal{ToolName: "write_file", Path: path, OldText: old, NewText: pyrepr.Str(content), Arguments: copiedArguments(arguments)}, nil
}

// Proposal of a replace mode patch call, refused when the old text has no match
func proposalForPatchReplace(env *Env, arguments map[string]any) (*EditProposal, error) {
	path, err := requiredEditPath(env, arguments)
	if err != nil {
		return nil, err
	}
	oldString, newString := arguments["old_string"], arguments["new_string"]
	if oldString == nil || newString == nil {
		return nil, errors.New("old_string and new_string required")
	}
	old, err := readEditTextIfExists(path)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, fmt.Errorf("Failed to read file: %s", path)
	}
	newText, matches, _, problem := FuzzyFindAndReplace(*old, pyrepr.Str(oldString), pyrepr.Str(newString), pyrepr.Truthy(arguments["replace_all"]), nil)
	if problem != "" || matches == 0 {
		if problem == "" {
			problem = "Could not find match for old_string in " + path
		}
		return nil, errors.New(problem)
	}
	return &EditProposal{ToolName: "patch", Path: path, OldText: old, NewText: newText, Arguments: copiedArguments(arguments)}, nil
}

// Every file a V4A patch updates, adds, deletes or moves
func extractV4APatchPaths(body string) []string {
	var paths []string
	for _, m := range v4aFileRe.FindAllStringSubmatch(body, -1) {
		paths = append(paths, strings.TrimSpace(m[1]))
	}
	for _, m := range v4aMoveRe.FindAllStringSubmatch(body, -1) {
		paths = append(paths, strings.TrimSpace(m[1]), strings.TrimSpace(m[2]))
	}
	out := paths[:0]
	for _, p := range paths {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Proposal of a V4A patch call carrying the exact patch as its new text
func proposalForPatchV4A(env *Env, arguments map[string]any) (*EditProposal, error) {
	body, ok := arguments["patch"].(string)
	if !ok || body == "" {
		return nil, errors.New("patch content required")
	}
	var paths []string
	for _, raw := range extractV4APatchPaths(body) {
		path, err := requiredEditPath(env, map[string]any{"path": raw})
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, errors.New("no file paths found in V4A patch")
	}
	if len(paths) == 1 {
		old, err := readEditTextIfExists(paths[0])
		if err != nil {
			return nil, err
		}
		return &EditProposal{ToolName: "patch", Path: paths[0], OldText: old, NewText: body, Arguments: copiedArguments(arguments), Paths: paths}, nil
	}
	return &EditProposal{ToolName: "patch", Path: strings.Join(paths, ", "), NewText: body, Arguments: copiedArguments(arguments), Paths: paths}, nil
}

// Edit proposal of a supported file mutation call, nil for any other call
func BuildEditProposal(env *Env, toolName string, arguments map[string]any) (*EditProposal, error) {
	switch toolName {
	case "write_file":
		return proposalForWriteFile(env, arguments)
	case "patch":
		mode, has := arguments["mode"]
		if !has {
			mode = "replace"
		}
		switch mode {
		case "replace":
			return proposalForPatchReplace(env, arguments)
		case "patch":
			return proposalForPatchV4A(env, arguments)
		}
	}
	return nil, nil
}

// Whether a path names git or ssh state or a credential file name
func sensitiveAutoApprovePath(path string) bool {
	expanded := expandUser(path)
	for _, part := range strings.FieldsFunc(expanded, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if lower := strings.ToLower(part); lower == ".git" || lower == ".ssh" {
			return true
		}
	}
	return sensitiveAutoApproveNames[strings.ToLower(filepath.Base(expanded))]
}

// Absolute path with symlinks resolved as far as they exist
func resolveLoose(path string) string {
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(abs)
	if dir == "" || filepath.Clean(dir) == abs {
		return abs
	}
	return filepath.Join(resolveLoose(filepath.Clean(dir)), base)
}

// Whether path lies at or under root
func editPathUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Whether an ACP edit may bypass the prompt under the session policy, sensitive paths always asking
func ShouldAutoApproveEdit(proposal *EditProposal, policy, cwd string) bool {
	policy = strings.TrimSpace(policy)
	if policy == "" {
		policy = AutoApproveAsk
	}
	paths := proposal.Paths
	if len(paths) == 0 {
		paths = []string{proposal.Path}
	}
	if policy == AutoApproveAsk {
		return false
	}
	for _, p := range paths {
		if sensitiveAutoApprovePath(p) {
			return false
		}
	}
	resolved := make([]string, len(paths))
	for i, p := range paths {
		resolved[i] = resolveLoose(p)
	}
	if policy == AutoApproveSession {
		return true
	}
	if policy != AutoApproveWorkspace {
		return false
	}
	tmp := resolveLoose(os.TempDir())
	workspace := ""
	if cwd != "" {
		workspace = resolveLoose(cwd)
	}
	for _, p := range resolved {
		if !editPathUnder(p, tmp) && (workspace == "" || !editPathUnder(p, workspace)) {
			return false
		}
	}
	return true
}

// Tool error JSON of a refused edit
func editDenied(message string) string {
	text, _ := pyjson.Dumps(map[string]any{"error": message}, pyjson.EnsureASCII(false))
	return text
}

// Runs the bound ACP edit approval, a tool error JSON when the edit must be blocked and empty to continue
func MaybeRequireEditApproval(ctx context.Context, env *Env, toolName string, arguments map[string]any) (string, error) {
	requester := boundEditApprovalRequester(ctx)
	if requester == nil {
		return "", nil
	}
	proposal, err := BuildEditProposal(env, toolName, arguments)
	if err != nil {
		toolsLogger(env).Warn(fmt.Sprintf("Could not build ACP edit approval proposal for %s: %s", toolName, err))
		return editDenied(fmt.Sprintf("Edit approval denied: could not prepare diff (%s)", err)), nil
	}
	if proposal == nil {
		return "", nil
	}
	approved, err := requester(ctx, env, proposal)
	if err != nil {
		toolsLogger(env).Warn(fmt.Sprintf("ACP edit approval requester failed: %s", err))
		approved = false
	}
	if approved {
		return "", nil
	}
	return editDenied("Edit approval denied by ACP client; file was not modified."), nil
}
