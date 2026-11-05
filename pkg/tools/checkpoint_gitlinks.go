package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Paths of gitlinks, nested repositories stored as a commit reference, in ls-tree -r -z output
func checkpointGitlinkPaths(lsTreeZ string) []string {
	var out []string
	for _, record := range strings.Split(lsTreeZ, "\x00") {
		if !strings.HasPrefix(record, "160000 commit ") {
			continue
		}
		if _, path, ok := strings.Cut(record, "\t"); ok {
			out = append(out, path)
		}
	}
	return out
}

// Checkpoint message suffix naming the nested repositories whose files were not captured
func checkpointUncapturedNote(nestedRepos []string) string {
	shown := strings.Join(nestedRepos[:min(3, len(nestedRepos))], ", ")
	more := ""
	if len(nestedRepos) > 3 {
		more = fmt.Sprintf(" (+%d)", len(nestedRepos)-3)
	}
	return " [nested git repos not captured: " + shown + more + "]"
}

// Components of a relative path with empty and current directory parts dropped
func checkpointRelParts(p string) []string {
	seps := "/"
	if runtime.GOOS == "windows" {
		seps = `/\`
	}
	var parts []string
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if seg != "." {
			parts = append(parts, seg)
		}
	}
	return parts
}

// Components of the requested restore scope relative to the project root, as recorded rather than resolved
func checkpointRequestedParts(absDir, filePath string) []string {
	requested := filepath.Clean(absDir + string(filepath.Separator) + filePath)
	rel, ok := relativeTo(requested, absDir)
	if !ok {
		return checkpointRelParts(filePath)
	}
	return checkpointRelParts(rel)
}

// Reports whether one component list is a prefix of the other
func checkpointPartsIntersect(requested, repo []string) bool {
	n := min(len(requested), len(repo))
	for i := 0; i < n; i++ {
		if requested[i] != repo[i] {
			return false
		}
	}
	return true
}

// Outcome of resolving a restore selection against the nested repositories of a checkpoint
type checkpointSelection struct {
	Blocked     []string
	Selected    []string
	HasSelected bool
	Failure     map[string]any
}

// Narrows the blocking gitlinks to the requested scope and resolves other pathspecs in a disposable index
func checkpointResolveSelection(env *Env, store, absDir, commitHash, filePath string, nestedRepos []string) (sel checkpointSelection, err error) {
	sel = checkpointSelection{Blocked: nestedRepos}
	if filePath == "" {
		return sel, nil
	}
	requested := checkpointRequestedParts(absDir, filePath)
	var blocked []string
	for _, repo := range nestedRepos {
		if checkpointPartsIntersect(requested, checkpointRelParts(filepath.ToSlash(repo))) {
			blocked = append(blocked, repo)
		}
	}
	sel.Blocked = blocked
	if len(blocked) > 0 {
		return sel, nil
	}
	scratch, err := os.MkdirTemp(store, "restore-inspect-")
	if err != nil {
		return sel, err
	}
	defer func() {
		if cerr := rmtreeReadonly(scratch, false); cerr != nil && err == nil {
			err = cerr
		}
	}()
	inspectIndex := filepath.Join(scratch, "index")
	ok, _, errText := runCheckpointGit(env, []string{"read-tree", commitHash}, store, absDir, checkpointGitOpts{IndexFile: inspectIndex})
	selected := ""
	if ok {
		ok, selected, errText = runCheckpointGit(env, []string{"ls-files", "--stage", "--error-unmatch", "-z", "--", filePath},
			store, absDir, checkpointGitOpts{IndexFile: inspectIndex})
	}
	if !ok {
		sel.Failure = map[string]any{"success": false, "error": "Could not inspect restore selection: " + errText}
		return sel, nil
	}
	sel.Blocked = nil
	for _, record := range strings.Split(selected, "\x00") {
		_, path, hasTab := strings.Cut(record, "\t")
		if !hasTab {
			continue
		}
		if strings.HasPrefix(record, "160000 ") {
			sel.Blocked = append(sel.Blocked, path)
		}
		sel.Selected = append(sel.Selected, path)
	}
	sel.HasSelected = true
	if len(sel.Selected) == 0 {
		sel.Failure = map[string]any{"success": false, "error": "Restore selection matched no files in checkpoint: " + filePath}
	}
	return sel, nil
}

// Checks out exactly the inspected entries through a NUL separated literal pathspec file
func checkpointCheckoutSelected(env *Env, store, absDir, commitHash, indexFile string, selected []string) (ok bool, errText string, err error) {
	scratch, err := os.MkdirTemp(store, "restore-select-")
	if err != nil {
		return false, "", err
	}
	defer func() {
		if cerr := rmtreeReadonly(scratch, false); cerr != nil && err == nil {
			err = cerr
		}
	}()
	specFile := filepath.Join(scratch, "pathspec")
	var spec strings.Builder
	for _, p := range selected {
		spec.WriteString(p)
		spec.WriteByte(0)
	}
	if err := os.WriteFile(specFile, []byte(spec.String()), 0o666); err != nil {
		return false, "", err
	}
	ok, _, errText = runCheckpointGit(env, []string{"checkout", commitHash, "--pathspec-from-file=" + specFile, "--pathspec-file-nul"},
		store, absDir, checkpointGitOpts{
			Timeout:   checkpointGitTimeout() * 2,
			IndexFile: indexFile,
			ExtraEnv:  map[string]string{"GIT_LITERAL_PATHSPECS": "1"},
		})
	return ok, errText, nil
}
