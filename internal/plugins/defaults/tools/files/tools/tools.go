package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	filesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/document"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

const toolset = "file"

// Heading the Tools screen lists the file tools under
const group = "Files"

// Tool names
const (
	ReadFile    = "read_file"
	WriteFile   = "write_file"
	Patch       = "patch"
	SearchFiles = "search_files"
)

type registrant struct {
	terminal  *terminal.Terminal
	web       *web.Registry
	skills    *skills.Registry
	documents *document.Reader
	memory    *memory
	walks     *walks
	// What each environment told of its commands, by the key it is kept under
	mu       sync.Mutex
	commands map[string]*commands
	// Registry the tools are in, whose checkers, the language servers among them, check the writes
	checkers *tools.Registry
	// Package manager, whose ripgrep search_files prefers and whose Node the JavaScript and TypeScript checks run on
	packages *packages.Packages
}

func Register(registry *tools.Registry, profiles *profile.Registry, host *terminal.Terminal, backends *web.Registry, sources *skills.Registry,
	pm *packages.Packages) {
	r := &registrant{terminal: host, web: backends, skills: sources, documents: document.New(), memory: newMemory(), walks: newWalks(),
		commands: map[string]*commands{}, packages: pm, checkers: registry}
	registry.Register(r)
	profiles.Register(r)
}

// Forgets the sessions of a deleted profile
func (r *registrant) Delete(profile string) error {
	r.memory.drop(profile)
	return nil
}

// Forgets what the sessions of a profile read before its folder moves to a new name, the paths under it read no longer where they were
func (r *registrant) Stop(profile string) error {
	r.memory.drop(profile)
	return nil
}

// Paths the session read through the file tools, its own writes counted as reads of what they wrote
func (r *registrant) KnownReads(cfg *config.Config, session string) []string {
	return r.memory.knownReads(cfg, session)
}

// Paths among those given that a session besides the one left out wrote last since the time, by that session
func (r *registrant) WritesSince(cfg *config.Config, exclude string, since time.Time, paths []string) map[string][]string {
	return r.memory.writesSince(exclude, since, paths)
}

// Another tool is about to run: the session's reads and searches in a row start counting again
func (r *registrant) Dispatched(call *tools.Call) {
	if call.Name == ReadFile || call.Name == SearchFiles {
		return
	}
	r.memory.mu.Lock()
	t := r.memory.sessions[sessionKey(call.Env.Config, call.SessionID)]
	r.memory.mu.Unlock()
	if t != nil {
		r.memory.other(t)
	}
}

// The session's context was compacted: reads it made before are no longer in the model's context, so none of them is answered with the
// unchanged stub again. What the compaction of a session calls
func (r *registrant) Compacted(cfg *config.Config, session string) {
	r.memory.mu.Lock()
	t := r.memory.sessions[sessionKey(cfg, session)]
	r.memory.mu.Unlock()
	if t != nil {
		r.memory.compacted(t)
	}
}

// Whether a file tool can run: the profile's terminal backend can
func (r *registrant) available(env *tools.Env) bool { return r.terminal.Check(env.Config) == nil }

// What a file tool lacks to run: the reason the profile's terminal backend can't run
func (r *registrant) lacks(env *tools.Env) string {
	if err := r.terminal.Check(env.Config); err != nil {
		return err.Error()
	}
	return ""
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{
		{Name: ReadFile, Toolset: toolset, Title: "Read", About: "Read a file, including PDFs and office documents", Group: group, Icon: tools.IconRead, Subject: "path", Schema: r.readSchema(nil), Dynamic: r.readSchema,
			Handler: r.read, Check: r.available, Lacks: r.lacks, MaxResultChars: 100_000, Effect: tools.ReadOnly, Paths: paths(false)},
		{Name: WriteFile, Toolset: toolset, Title: "Write", About: "Create a file, or replace everything in one", Group: group, Icon: tools.IconWrite, Subject: "path", Risks: []string{sshConfigRisk}, Schema: writeSchema,
			Handler: r.write, Check: r.available, Lacks: r.lacks, MaxResultChars: 100_000, Effect: tools.Mutates, Paths: paths(true)},
		{Name: Patch, Toolset: toolset, Title: "Edit", About: "Change part of a file, or of several files at once", Group: group, Icon: tools.IconEdit, Subject: "path", Risks: []string{sshConfigRisk}, Schema: patchSchema(false),
			Dynamic: r.patchSchema, Handler: r.patch, Check: r.available, Lacks: r.lacks, MaxResultChars: 100_000, Effect: tools.Mutates, Paths: paths(true)},
		{Name: SearchFiles, Toolset: toolset, Title: "Search", About: "Search inside files, or find files by their name", Group: group, Icon: tools.IconSearch, Subject: "pattern", Schema: searchSchema, Handler: r.search,
			Check: r.available, Lacks: r.lacks, MaxResultChars: 100_000, Effect: tools.ReadOnly, Paths: paths(false)},
	}
}

// Paths a call reads or writes, which calls of the same reply touching them wait on: the path asked for, the folder a search starts in,
// every file a V4A patch names; none for a call that names none, which then runs alone
func paths(writes bool) func(env *tools.Env, args map[string]any) ([]string, bool) {
	return func(_ *tools.Env, args map[string]any) ([]string, bool) {
		if mode, _ := args["mode"].(string); mode == "patch" {
			text, _ := args["patch"].(string)
			all, _, _, failed := headerPaths(text)
			if failed != "" {
				return nil, writes
			}
			return all, writes
		}
		if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
			return []string{path}, writes
		}
		if _, search := args["pattern"]; search && !writes {
			return []string{"."}, false
		}
		return nil, writes
	}
}

// read_file as the session's settings describe it, scanned PDFs said to be read when hosted or local OCR can read them
func (r *registrant) readSchema(env *tools.Env) map[string]any {
	budget, lines, pdfs := 100_000, readLimit, "PDF (text layer)"
	if env != nil {
		budget, lines = max(env.Config.GetInt(filesconfig.ReadMaxChars), 1), max(env.Config.GetInt(toolsconfig.OutputMaxLines), 1)
		if r.web.Parse(env.Config) != nil || document.LocalOCR() {
			pdfs = "PDF (scanned or text)"
		}
	}
	chars := fmt.Sprintf("~%dK", max(budget/1000, 1))
	return tools.Schema("Read a text file with line numbers and pagination. Use this instead of cat/head/tail in terminal. Output format: "+
		"'LINE_NUM|CONTENT'. Suggests similar filenames if not found. Use offset and limit for large files. Reads exceeding "+chars+" characters are "+
		"truncated on a line boundary and return a next_offset; continue with offset to read the rest. Documents auto-extract to readable text, "+
		"recognized by their content whatever their file name: .ipynb, Office (.docx/.xlsx/.pptx and legacy .doc/.ppt/.xls), "+pdfs+", OpenDocument, "+
		"RTF, EPUB, SQLite (.db/.sqlite: schema, row counts, first rows). Other binary files read as a hex view of 16 byte rows, offset and limit "+
		"counting rows. Use vision_analyze for images.", []string{"path"}, map[string]any{
		"path":   tools.Property("string", "Path to the file to read (absolute, relative, or ~/path)"),
		"offset": map[string]any{"type": "integer", "description": "Line number to start reading from (1-indexed, default: 1)", "default": 1, "minimum": 1},
		"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum number of lines to read (default: %d, max: %d). Reads are "+
			"additionally capped at a %s-character budget with a next_offset continuation.", min(readLimit, lines), lines, chars),
			"default": min(readLimit, lines), "maximum": lines},
	})
}

var writeSchema = tools.Schema("Write content to a file, completely replacing existing content. Use this instead of echo/cat heredoc in terminal. "+
	"Creates parent directories automatically. OVERWRITES the entire file — use 'patch' for targeted edits. For an EXISTING file, call read_file "+
	"first: write_file refuses (file untouched) when this task has no current full read/write of the file or the file changed on disk since; on "+
	"refusal, read_file, merge, then retry. Auto-runs syntax checks on .py/.json/.yaml/.toml and other linted languages; only NEW errors introduced "+
	"by this write are surfaced (pre-existing errors are filtered out). The result's verified:true means the on-disk content hash was confirmed — do "+
	"NOT re-read the file to check the write landed.", []string{"path", "content"}, map[string]any{
	"path":    tools.Property("string", "Path to the file to write (will be created if it doesn't exist, overwritten if it does)"),
	"content": tools.Property("string", "Complete content to write to the file"),
})

// Families of the OpenAI models trained on the V4A patch format, by provider and by model name
var (
	openAIProviders = strs.Set("openai", "openai-chat", "openai-codex", "azure-openai", "codex")
	openAIModels    = []string{"gpt-", "gpt.", "chatgpt", "codex", "o1", "o3", "o4", "o5"}
)

// Whether the session's main model is of the OpenAI family that handles V4A patches best
func openAIFamily(env *tools.Env) bool {
	main := env.Main()
	if openAIProviders[strings.ToLower(strings.TrimSpace(main.Provider))] || openAIProviders[strings.ToLower(strings.TrimSpace(main.Spec))] {
		return true
	}
	model := strings.ToLower(strings.TrimSpace(main.Model))
	slug := model
	if _, rest, ok := strings.Cut(model, "/"); ok {
		slug = rest
	}
	for _, prefix := range openAIModels {
		if strings.HasPrefix(slug, prefix) {
			return true
		}
	}
	return strings.Contains(model, "openai/")
}

// patch with the V4A mode for an OpenAI family main model, replace alone otherwise
func (r *registrant) patchSchema(env *tools.Env) map[string]any {
	return patchSchema(openAIFamily(env))
}

func patchSchema(v4a bool) map[string]any {
	properties := map[string]any{
		"path":       tools.Property("string", "File path to edit."),
		"old_string": tools.Property("string", "Exact text to find and replace. Must be unique in the file unless replace_all=true. Include surrounding context lines to ensure uniqueness."),
		"new_string": tools.Property("string", "Changed replacement text; it must differ from old_string. Pass empty string '' to delete the matched text."),
		"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences instead of requiring a unique match (default: false)",
			"default": false},
	}
	lead := "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. Uses fuzzy matching (9 strategies) so minor " +
		"whitespace/indentation differences won't break it. Returns a unified diff. Auto-runs syntax checks after editing."
	if !v4a {
		return tools.Schema(lead+" Finds a unique string and replaces it.", []string{"path", "old_string", "new_string"}, properties)
	}
	all := maps.Clone(properties)
	all["mode"] = map[string]any{"type": "string", "enum": []string{"replace", "patch"}, "default": "replace",
		"description": "Edit mode. 'replace' (default): requires path + old_string + new_string. 'patch': requires patch content only."}
	all["patch"] = tools.Property("string", "REQUIRED when mode='patch'. V4A format patch content. Format:\n*** Begin Patch\n*** Update File: "+
		"path/to/file\n@@ context hint @@\n context line\n-removed line\n+added line\n*** End Patch")
	return tools.Schema(lead+"\n\nREPLACE MODE (mode='replace', default): find a unique string and replace it. REQUIRED PARAMETERS: mode, path, "+
		"old_string, new_string.\nPATCH MODE (mode='patch'): apply V4A multi-file patches for bulk changes. REQUIRED PARAMETERS: mode, patch.",
		[]string{"mode"}, all)
}

// Note of the search description on a macOS machine, whose broad searches skip the folders guarded by privacy prompts
func macNote() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return " On macOS, broad searches above the user home automatically skip TCC-protected folders (Desktop, Documents, Downloads, Library, " +
		"Movies, Music, Pictures); target one directly when access is intentional."
}

var searchSchema = tools.Schema("Search file contents or find files by name. Use this instead of grep/rg/find/ls in terminal. Ripgrep-backed, "+
	"faster than shell equivalents."+macNote()+"\n\nContent search (target='content'): Regex search inside files. Output modes: full matches with "+
	"line numbers, file paths only, or match counts.\n\nFile search (target='files'): Find files by glob pattern (e.g., '*.py', '*config*'). Also "+
	"use this instead of ls. Discovery order is the fast bounded default; exact global newest-first order is an explicit opt-in and may scan the "+
	"full tree.", []string{"pattern"}, map[string]any{
	"pattern": tools.Property("string", "Regex pattern for content search, or glob pattern (e.g., '*.py') for file search"),
	"target": map[string]any{"type": "string", "enum": []string{"content", "files"}, "default": "content",
		"description": "'content' searches inside file contents, 'files' searches for files by name"},
	"path":      map[string]any{"type": "string", "default": ".", "description": "Directory or file to search in (default: current working directory)"},
	"file_glob": tools.Property("string", "Filter files by pattern in grep mode (e.g., '*.py' to only search Python files)"),
	"limit":     map[string]any{"type": "integer", "default": searchLimit, "description": "Maximum number of results to return (default: 50)"},
	"offset":    map[string]any{"type": "integer", "default": 0, "description": "Skip first N results for pagination (default: 0)"},
	"order": map[string]any{"type": "string", "enum": []string{"discovery", "modified"}, "default": "discovery",
		"description": "File-search order: 'discovery' is fast bounded traversal order; 'modified' is exact global newest-first and may scan the full " +
			"tree; ignored for content"},
	"output_mode": map[string]any{"type": "string", "enum": []string{"content", "files_only", "count"}, "default": "content",
		"description": "Output format for grep mode: 'content' shows matching lines with line numbers, 'files_only' lists file paths, 'count' shows " +
			"match counts per file"},
	"context": map[string]any{"type": "integer", "default": 0, "description": "Number of context lines before and after each match (grep mode only)"},
})

// What each environment told of its commands, made again when the environment under the key is a new one
func (r *registrant) cacheFor(lease *terminal.Lease) *commands {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.commands[lease.Key]
	if c == nil || c.env != lease.Env {
		c = &commands{env: lease.Env, found: map[string]bool{}, ordered: map[string]*string{}}
		r.commands[lease.Key] = c
	}
	return c
}

// Runs the operations of one call in the session's terminal environment, made when there is none and held until they end; what the
// file tools remember of sessions whose environment is gone is forgotten first, as alpha forgets it when an environment is cleaned up
func (r *registrant) with(ctx context.Context, call *tools.Call, work func(o *ops) string) string {
	cfg := call.Env.Config
	r.memory.sweep(r.terminal.Kept)
	lease, err := r.terminal.Acquire(ctx, cfg, call.SessionID, call.Env.Cwd)
	if err != nil {
		return failure(err.Error())
	}
	defer lease.Release()
	host := lease.Env.HostFiles()
	base := lease.Folder("")
	if host && !filepath.IsAbs(base) {
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
	}
	o := &ops{r: r, ctx: ctx, call: call, cfg: cfg, lease: lease, env: lease.Env, host: host, base: base, cache: r.cacheFor(lease),
		native: host && !windows && enabled(cfg, filesconfig.NativeRead)}
	r.memory.mu.Lock()
	o.track = r.memory.track(cfg, call.SessionID)
	r.memory.mu.Unlock()
	out := work(o)
	if o.err != nil {
		slog.Error("file tool failed in its terminal environment", "tool", call.Name, "backend", lease.Backend.Name, "error", o.err)
		if _, broken := errors.AsType[*terminal.ConnectionError](o.err); broken {
			lease.Evict()
		}
		return failure(o.err.Error())
	}
	return out
}

// Text of an optional text argument, nil when it is absent or null
func optional(args map[string]any, key string) *string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprint(v)
	}
	return &s
}

// Text argument with a default when it is absent
func text(args map[string]any, key, fallback string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return fallback
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (r *registrant) read(ctx context.Context, call *tools.Call) string {
	path := ""
	if v, ok := call.Args["path"]; ok {
		s, isText := v.(string)
		if !isText {
			return failure("the path is " + kind(v) + ", expected text")
		}
		path = s
	}
	offset := max(1, paging(call.Args["offset"], 1))
	limit := max(1, min(paging(call.Args["limit"], readLimit), call.Env.Config.GetInt(toolsconfig.OutputMaxLines)))
	if refusal := ntRefusal(path, "Read"); refusal != "" {
		return failure(refusal)
	}
	return r.with(ctx, call, func(o *ops) string { return o.read(path, offset, limit) })
}

func (r *registrant) write(ctx context.Context, call *tools.Call) string {
	path, isText := call.Args["path"].(string)
	if !isText || path == "" {
		return failure("write_file: missing required field 'path'. Re-emit the tool call with both 'path' and 'content' set.")
	}
	raw, present := call.Args["content"]
	if !present {
		return failure("write_file: missing required field 'content'. The tool call included a path but no content argument — this is almost " +
			"always a dropped-arg bug under context pressure. Re-emit the tool call with the full content payload.")
	}
	content, isText := raw.(string)
	if !isText {
		return failure("write_file: 'content' must be text, got " + kind(raw) + ".")
	}
	crossProfile := truthy(call.Args["cross_profile"])
	return r.with(ctx, call, func(o *ops) string { return o.write(path, content, crossProfile) })
}

func (r *registrant) patch(ctx context.Context, call *tools.Call) string {
	c := patchCall{mode: text(call.Args, "mode", "replace"), old: optional(call.Args, "old_string"), new: optional(call.Args, "new_string"),
		all: truthy(call.Args["replace_all"]), crossProfile: truthy(call.Args["cross_profile"])}
	switch v := call.Args["patch"].(type) {
	case string:
		c.patch = v
	case nil:
	default:
		if c.mode == "patch" && truthy(v) {
			return failure("the patch is " + kind(v) + ", expected text")
		}
	}
	switch v := call.Args["path"].(type) {
	case string:
		c.path = v
	case nil:
	default:
		if truthy(v) {
			return failure("the path is " + kind(v) + ", expected text")
		}
	}
	return r.with(ctx, call, func(o *ops) string { return o.patch(c) })
}

func (r *registrant) search(ctx context.Context, call *tools.Call) string {
	target := text(call.Args, "target", "content")
	switch target {
	case "grep":
		target = "content"
	case "find":
		target = "files"
	}
	path := "."
	if v, ok := call.Args["path"]; ok && v != nil {
		s, isText := v.(string)
		if !isText {
			return failure("the path is " + kind(v) + ", expected text")
		}
		if strings.TrimSpace(s) != "" {
			path = s
		}
	}
	pattern, isText := call.Args["pattern"].(string)
	if !isText {
		return failure("the search pattern is " + kind(call.Args["pattern"]) + ", expected text")
	}
	c := searchCall{pattern: pattern, target: target, path: path, glob: text(call.Args, "file_glob", ""),
		limit: max(1, paging(call.Args["limit"], searchLimit)), offset: max(0, paging(call.Args["offset"], 0)),
		mode: text(call.Args, "output_mode", "content"), context: max(0, paging(call.Args["context"], 0)), order: text(call.Args, "order", "discovery")}
	return r.with(ctx, call, func(o *ops) string { return o.searchTool(c) })
}

// Whether a language server takes files of the extension
func (o *ops) serverHandles(extension string) bool {
	return extension != "" && o.host && o.r.checkers.Handles(o.cfg, extension)
}

// Whether a language server checks the file
func (o *ops) serverChecks(path string) bool {
	return o.host && o.r.checkers.Checks(o.cfg, path)
}

// Diagnostics a write brought in, from a language server that checks the file, late ones going to the session
func (o *ops) diagnostics(path string, before, after *string, started time.Time) string {
	if !o.serverChecks(path) {
		return ""
	}
	owner := o.call.SessionID
	if owner == "" {
		owner = "default"
	}
	return o.r.checkers.Check(o.ctx, o.cfg, o.full(path), before, after, owner, started)
}

// The group of the file tools, whose options are the files section
func (r *registrant) Groups() []tools.Group {
	return []tools.Group{{Title: group, Sections: []string{filesconfig.Key}, Order: 10}}
}
