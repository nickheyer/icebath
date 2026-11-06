package codeexec

import (
	_ "embed"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Tools allowed inside the sandbox, intersected with the session's enabled tools to pick the generated stubs
var sandboxAllowedTools = []string{"web_search", "web_extract", "read_file", "write_file", "search_files", "patch", "terminal"}

type toolStub struct {
	sig, doc, argsExpr string
}

var toolStubs = map[string]toolStub{
	"web_search": {"query: str, limit: int = 5",
		`"""Search the web. Returns dict with data.web list of {url, title, description}."""`,
		`{"query": query, "limit": limit}`},
	"web_extract": {"urls: list, char_limit: int = None",
		`"""Extract content from URLs (no LLM summarization). Returns dict with results list of {url, title, content, error}. Pages over char_limit (default 15000) are head+tail truncated with the full text stored on disk; the content footer gives the path. content is markdown."""`,
		`{"urls": urls, "char_limit": char_limit}`},
	"read_file": {"path: str, offset: int = 1, limit: int = 2000",
		`"""Read a file (1-indexed lines). Returns dict with "content" and "total_lines"."""`,
		`{"path": path, "offset": offset, "limit": limit}`},
	"write_file": {"path: str, content: str, cross_profile: bool = False",
		`"""Write content to a file (always overwrites). Returns dict with status."""`,
		`{"path": path, "content": content, "cross_profile": cross_profile}`},
	"search_files": {`pattern: str, target: str = "content", path: str = ".", file_glob: str = None, limit: int = 50, offset: int = 0, output_mode: str = "content", context: int = 0, order: str = "discovery"`,
		`"""Search file contents (target="content") or find files by name (target="files"). Returns dict with "matches"."""`,
		`{"pattern": pattern, "target": target, "path": path, "file_glob": file_glob, "limit": limit, "offset": offset, "output_mode": output_mode, "context": context, "order": order}`},
	"patch": {`path: str = None, old_string: str = None, new_string: str = None, replace_all: bool = False, mode: str = "replace", patch: str = None, cross_profile: bool = False`,
		`"""Targeted find-and-replace (mode="replace") or V4A multi-file patches (mode="patch"). Returns dict with status."""`,
		`{"path": path, "old_string": old_string, "new_string": new_string, "replace_all": replace_all, "mode": mode, "patch": patch, "cross_profile": cross_profile}`},
	"terminal": {"command: str, timeout: int = None, workdir: str = None",
		`"""Run a shell command (foreground only). Returns dict with "output" and "exit_code"."""`,
		`{"command": command, "timeout": timeout, "workdir": workdir}`},
}

//go:embed payload/uds_transport_header.py
var udsTransportHeader string

//go:embed payload/file_transport_header.py
var fileTransportHeader string

// Source of the icebath_tools wrapper module for the allowed and enabled tools over the uds or file transport
func generateToolsModule(enabledTools []string, transport string) string {
	header := udsTransportHeader
	if transport == "file" {
		header = fileTransportHeader
	}
	names := make([]string, 0, len(toolStubs))
	for name := range toolStubs {
		names = append(names, name)
	}
	sort.Strings(names)
	var stubs []string
	for _, name := range names {
		if !slices.Contains(enabledTools, name) {
			continue
		}
		s := toolStubs[name]
		stubs = append(stubs, fmt.Sprintf("def %s(%s):\n    %s\n    return _call(%s, %s)\n", name, s.sig, s.doc, pyrepr.StrRepr(name), s.argsExpr))
	}
	return header + strings.Join(stubs, "\n")
}

// Recovery hint for a name the script tried to import from icebath_tools
func missingToolsImportHint(missing string, enabledTools []string) string {
	if missing == "json_parse" || missing == "shell_quote" || missing == "retry" {
		return fmt.Sprintf("Import helpers with `from icebath_tools import %s`. "+
			"If that import failed, the generated module may be stale or another "+
			"icebath_tools may be first on sys.path. Check icebath_tools.__file__ "+
			"and retry with reset=true.", missing)
	}
	pool := enabledTools
	if len(pool) == 0 {
		pool = sandboxAllowedTools
	}
	var available []string
	for _, name := range sandboxAllowedTools {
		if slices.Contains(pool, name) {
			available = append(available, name)
		}
	}
	sort.Strings(available)
	return fmt.Sprintf("'%s' is not available inside the execute_code sandbox. "+
		"Importable tools here: %s. For anything "+
		"else, use the normal tool call instead of execute_code.", missing, strings.Join(available, ", "))
}

type failureHintRule struct {
	pattern *regexp2.Regexp
	format  func(group string, enabledTools []string) string
}

var failureHintRules = []failureHintRule{
	{regexp2.MustCompile(`cannot import name '(\w+)' from 'icebath_tools'`, regexp2.None), missingToolsImportHint},
	{regexp2.MustCompile(`NameError: name '(json_parse|shell_quote|retry)' is not defined`, regexp2.None),
		func(g string, _ []string) string {
			return fmt.Sprintf("Import %s before calling it: from icebath_tools import %s", g, g)
		}},
	{regexp2.MustCompile(`ModuleNotFoundError: No module named '([\w.]+)'`, regexp2.None),
		func(g string, _ []string) string {
			return fmt.Sprintf("'%s' is not installed in the sandbox interpreter. "+
				"Use Python stdlib inside execute_code, or run the code via "+
				"terminal() with the project venv's python instead.", g)
		}},
	{regexp2.MustCompile(`TypeError: string indices must be integers|AttributeError: 'str' object has no attribute 'get'`, regexp2.None),
		func(string, []string) string {
			return "Tool functions in the sandbox return DICTS (already parsed) — " +
				"do not json.loads() them or index them like strings. Example: read_file(path)['content']."
		}},
}

// One actionable recovery hint for a well known sandbox failure in the first 4000 characters, first match wins
func sandboxFailureHint(stderrText string, enabledTools []string) string {
	if stderrText == "" {
		return ""
	}
	window := clipRunes(stderrText, 4000)
	for _, rule := range failureHintRules {
		m, err := rule.pattern.FindStringMatch(window)
		if err != nil {
			return ""
		}
		if m != nil {
			group := ""
			if len(m.Groups()) > 1 {
				group = m.Groups()[1].String()
			}
			return rule.format(group, enabledTools)
		}
	}
	return ""
}
