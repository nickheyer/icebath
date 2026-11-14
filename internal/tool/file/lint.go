package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	grammar "github.com/nickheyer/icebath/internal/syntax/python"
	"github.com/nickheyer/icebath/internal/tool"
)

const lintTimeout = 30 * time.Second

type Diagnostics interface {
	HandlesExtension(ctx context.Context, ext string, hostFiles bool) bool
	EnabledFor(ctx context.Context, path string) bool
	WorkspaceTrusted(ctx context.Context, cwd string) (bool, error)
	EditCheck(ctx context.Context, path string, pre *string, post string, started time.Time) (string, error)
}

type Lint struct {
	diagnostics Diagnostics
}

var (
	_ Linter            = (*Lint)(nil)
	_ WriteGuard        = (*Safety)(nil)
	_ tool.CallObserver = (*Tracker)(nil)
)

func NewLint(d Diagnostics) (*Lint, error) {
	if d == nil {
		return nil, &tool.UnfilledError{Name: "file.Lint.Diagnostics"}
	}
	return &Lint{diagnostics: d}, nil
}

func shellLinter(ext string) (string, bool) {
	switch ext {
	case ".js":
		return "node --check {file} 2>&1", true
	case ".ts":
		return "npx tsc --noEmit {file} 2>&1", true
	case ".go":
		return "go vet {file} 2>&1", true
	case ".rs":
		return "rustfmt --check {file} 2>&1", true
	}
	return "", false
}

func linterUnusable(base, output string) bool {
	var patterns []string
	switch base {
	case "npx":
		patterns = []string{
			"this is not the tsc command you are looking for",
			"could not determine executable to run",
			"not found in npm registry",
		}
	case "rustfmt":
		patterns = []string{"no input filename given", "error: not a workspace"}
	case "go":
		patterns = []string{"cannot find package", "go: cannot find main module"}
	}
	lower := strings.ToLower(output)
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func lintYAML(content string) (bool, string) {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
	}
}

func lintTOML(content string) (bool, string) {
	var doc map[string]any
	err := toml.Unmarshal([]byte(content), &doc)
	if err == nil {
		return true, ""
	}
	var decodeErr *toml.DecodeError
	if errors.As(err, &decodeErr) {
		row, column := decodeErr.Position()
		return false, fmt.Sprintf("%s (at line %d, column %d)", decodeErr.Error(), row, column)
	}
	return false, err.Error()
}

func lintJSON(content string) (bool, string) {
	if problem, bad := jsonSyntaxProblem(content); bad {
		return false, "invalid JSON: " + problem
	}
	return true, ""
}

func lintSource(content string) (bool, string) {
	_, err := grammar.Parse([]byte(content), "")
	if err == nil {
		return true, ""
	}
	var syntaxErr *grammar.SyntaxError
	if errors.As(err, &syntaxErr) {
		loc := ""
		if syntaxErr.Line != 0 {
			loc = fmt.Sprintf(" (line %d, column %d)", syntaxErr.Line, syntaxErr.Col)
		}
		return false, syntaxErr.Class.String() + ": " + syntaxErr.Msg + loc
	}
	var limitErr *grammar.LimitError
	if errors.As(err, &limitErr) {
		return false, limitErr.Class() + ": " + limitErr.Error()
	}
	return false, err.Error()
}

func inprocLinter(ext string) func(string) (bool, string) {
	switch ext {
	case ".py":
		return lintSource
	case ".json":
		return lintJSON
	case ".yaml", ".yml":
		return lintYAML
	case ".toml":
		return lintTOML
	}
	return nil
}

func failClosed(ext string) bool {
	switch ext {
	case ".json", ".yaml", ".yml", ".toml":
		return true
	}
	return false
}

func (l *Lint) SyntaxProblem(ext, content string) (string, bool) {
	if !failClosed(ext) {
		return "", false
	}
	ok, problem := inprocLinter(ext)(content)
	return problem, !ok
}

func (l *Lint) WantsBaseline(ctx context.Context, o *Operations, ext string) (bool, error) {
	if inprocLinter(ext) != nil {
		return true, nil
	}
	if ext == "" {
		return false, nil
	}
	return l.diagnostics.HandlesExtension(ctx, ext, o.env.HostFilesystem()), nil
}

func (l *Lint) willHandle(ctx context.Context, o *Operations, path string) bool {
	return o.env.HostFilesystem() && l.diagnostics.EnabledFor(ctx, path)
}

func hasAncestorTSConfig(o *Operations, path string) bool {
	if !o.env.HostFilesystem() {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	dir := filepath.Dir(abs)
	for {
		if fi, err := os.Stat(filepath.Join(dir, "tsconfig.json")); err == nil && fi.Mode().IsRegular() {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func (l *Lint) check(ctx context.Context, o *Operations, path string, content *string) (LintResult, error) {
	ext := strings.ToLower(o.paths().ext(path))
	if inproc := inprocLinter(ext); inproc != nil {
		if content == nil {
			res, err := o.cat(ctx, path)
			if err != nil {
				return LintResult{}, err
			}
			if res.exitCode != 0 {
				return LintResult{Success: true, Skipped: true, Message: "Failed to read " + path + " for lint"}, nil
			}
			content = &res.stdout
		}
		ok, problem := inproc(*content)
		return LintResult{Success: ok, Output: problem}, nil
	}
	command, known := shellLinter(ext)
	if !known {
		return LintResult{Success: true, Skipped: true, Message: "No linter for " + ext + " files"}, nil
	}
	base := spaceFields(command)[0]
	if ext == ".ts" && hasAncestorTSConfig(o, path) {
		return LintResult{Success: true, Skipped: true, Message: "Project tsconfig.json detected — per-file tsc skipped " +
			"(single-file tsc can't resolve project aliases/globals; " +
			"use the LSP tier or `tsc -p tsconfig.json` for real " +
			"diagnostics)."}, nil
	}
	if (ext == ".ts" || ext == ".go" || ext == ".rs") && l.willHandle(ctx, o, path) {
		return LintResult{Success: true, Skipped: true, Message: "LSP server handles " + ext + " — shell linter skipped"}, nil
	}
	if (ext == ".ts" || ext == ".rs") && o.env.HostFilesystem() {
		trusted, err := l.diagnostics.WorkspaceTrusted(ctx, o.liveCwd())
		if err != nil {
			return LintResult{}, err
		}
		if !trusted {
			return LintResult{Success: true, Skipped: true, Message: base + " skipped: untrusted workspace (add it to lsp.trusted_workspaces to lint " + ext + " here)"}, nil
		}
	}
	available, err := o.hasCommand(ctx, base)
	if err != nil {
		return LintResult{}, err
	}
	if !available {
		return LintResult{Success: true, Skipped: true, Message: base + " not available"}, nil
	}
	res, err := o.execIn(ctx, strings.ReplaceAll(command, "{file}", nativeToolArg(path)), "", lintTimeout, nil)
	if err != nil {
		return LintResult{}, err
	}
	if res.exitCode != 0 && linterUnusable(base, res.stdout) {
		cleaned := trimSpace(tool.StripANSI(res.stdout))
		firstLine := charPrefix(cleaned, 120)
		for _, line := range splitLines(cleaned, false) {
			if s := trimSpace(line); s != "" {
				firstLine = s
				break
			}
		}
		return LintResult{Success: true, Skipped: true, Message: base + " not usable: " + charPrefix(firstLine, 200)}, nil
	}
	return LintResult{Success: res.exitCode == 0, Output: trimSpace(res.stdout)}, nil
}

func (l *Lint) Delta(ctx context.Context, o *Operations, path string, pre *string, post string) (LintResult, error) {
	after, err := l.check(ctx, o, path, &post)
	if err != nil {
		return LintResult{}, err
	}
	if after.Success || after.Skipped || pre == nil {
		return after, nil
	}
	before, err := l.check(ctx, o, path, pre)
	if err != nil {
		return LintResult{}, err
	}
	if before.Success || before.Skipped || before.Output == "" {
		return after, nil
	}
	existing := make(map[string]struct{})
	for _, line := range splitLines(before.Output, false) {
		if s := trimSpace(line); s != "" {
			existing[s] = struct{}{}
		}
	}
	var introduced []string
	for _, line := range splitLines(after.Output, false) {
		s := trimSpace(line)
		if s == "" {
			continue
		}
		if _, old := existing[s]; !old {
			introduced = append(introduced, line)
		}
	}
	if len(introduced) == 0 {
		return LintResult{Output: after.Output,
			Message: "Pre-existing lint errors — this edit didn't introduce new ones but the file is still broken."}, nil
	}
	return LintResult{Output: "New lint errors introduced by this edit " +
		"(pre-existing errors filtered out):\n" + strings.Join(introduced, "\n")}, nil
}

func (l *Lint) Diagnostics(ctx context.Context, o *Operations, path string, pre *string, post string, started time.Time) (string, error) {
	if !l.willHandle(ctx, o, path) {
		return "", nil
	}
	return l.diagnostics.EditCheck(ctx, path, pre, post, started)
}
