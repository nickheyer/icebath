package file

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/docconv"
	"github.com/nickheyer/icebath/internal/tool/env"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	toolsetName       = "file"
	maxResultChars    = 100_000
	readFileName      = "read_file"
	writeFileName     = "write_file"
	patchName         = "patch"
	searchFilesName   = "search_files"
	pdfTextLayer      = "PDF (text layer)"
	pdfScannedOrText  = "PDF (scanned or text)"
	wholeFileEditMode = "whole_file"

	readFileDescription = "Read a text file with line numbers and pagination. Use this instead of cat/head/tail in terminal. Output format: 'LINE_NUM|CONTENT'. Suggests similar filenames if not found. Use offset and limit for large files. Reads exceeding ~100K characters are truncated on a line boundary and return a next_offset; continue with offset to read the rest. Documents auto-extract to readable text: .ipynb, Office (.docx/.xlsx/.pptx and legacy .doc/.ppt/.xls), " + pdfTextLayer + ", OpenDocument, RTF, EPUB, SQLite (.db/.sqlite: schema, row counts, first rows). Cannot read images/binary — use vision_analyze for images."

	readFileParameters = `{"type": "object", "properties": {"path": {"type": "string", "description": "Path to the file to read (absolute, relative, or ~/path)"}, "offset": {"type": "integer", "description": "Line number to start reading from (1-indexed, default: 1)", "default": 1, "minimum": 1}, "limit": {"type": "integer", "description": "Maximum number of lines to read (default: 2000, max: 2000). Reads are additionally capped at a ~100K-character budget with a next_offset continuation.", "default": 2000, "maximum": 2000}}, "required": ["path"]}`

	writeFileDescription = "Write content to a file, completely replacing existing content. Use this instead of echo/cat heredoc in terminal. Creates parent directories automatically. OVERWRITES the entire file — use 'patch' for targeted edits. For an EXISTING file, call read_file first: write_file refuses (file untouched) when this task has no current full read/write of the file or the file changed on disk since; on refusal, read_file, merge, then retry. Auto-runs syntax checks on .py/.json/.yaml/.toml and other linted languages; only NEW errors introduced by this write are surfaced (pre-existing errors are filtered out). The result's verified:true means the on-disk content hash was confirmed — do NOT re-read the file to check the write landed."

	writeFileParameters = `{"type": "object", "properties": {"path": {"type": "string", "description": "Path to the file to write (will be created if it doesn't exist, overwritten if it does)"}, "content": {"type": "string", "description": "Complete content to write to the file"}}, "required": ["path", "content"]}`

	patchDescription = "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. " +
		"Uses fuzzy matching (9 strategies) so minor whitespace/indentation differences won't break it. " +
		"Returns a unified diff. Auto-runs syntax checks after editing. " +
		"Finds a unique string and replaces it."

	patchProperties = `"path": {"type": "string", "description": "File path to edit."}, "old_string": {"type": "string", "description": "Exact text to find and replace. Must be unique in the file unless replace_all=true. Include surrounding context lines to ensure uniqueness."}, "new_string": {"type": "string", "description": "Changed replacement text; it must differ from old_string. Pass empty string '' to delete the matched text."}, "replace_all": {"type": "boolean", "description": "Replace all occurrences instead of requiring a unique match (default: false)", "default": false}`

	patchParameters = `{"type": "object", "properties": {` + patchProperties + `}, "required": ["path", "old_string", "new_string"]}`

	patchDualDescription = "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. " +
		"Uses fuzzy matching (9 strategies) so minor whitespace/indentation differences won't break it. " +
		"Returns a unified diff. Auto-runs syntax checks after editing.\n\n" +
		"REPLACE MODE (mode='replace', default): find a unique string and replace it. " +
		"REQUIRED PARAMETERS: mode, path, old_string, new_string.\n" +
		"PATCH MODE (mode='patch'): apply V4A multi-file patches for bulk changes. " +
		"REQUIRED PARAMETERS: mode, patch."

	patchDualParameters = `{"type": "object", "properties": {"mode": {"type": "string", "enum": ["replace", "patch"], "description": "Edit mode. 'replace' (default): requires path + old_string + new_string. 'patch': requires patch content only.", "default": "replace"}, ` + patchProperties + `, "patch": {"type": "string", "description": "REQUIRED when mode='patch'. V4A format patch content. Format:\n*** Begin Patch\n*** Update File: path/to/file\n@@ context hint @@\n context line\n-removed line\n+added line\n*** End Patch"}}, "required": ["mode"]}`

	searchFilesLead = "Search file contents or find files by name. Use this instead of grep/rg/find/ls in terminal. Ripgrep-backed, faster than shell equivalents."

	searchFilesMacNote = " On macOS, broad searches above the user home automatically skip TCC-protected folders (Desktop, " +
		"Documents, Downloads, Library, Movies, Music, Pictures); target one directly when access is intentional."

	searchFilesTail = "\n\nContent search (target='content'): Regex search inside files. Output modes: full matches with line numbers, file paths only, or match counts.\n\nFile search (target='files'): Find files by glob pattern (e.g., '*.py', '*config*'). Also use this instead of ls. Discovery order is the fast bounded default; exact global newest-first order is an explicit opt-in and may scan the full tree."

	searchFilesParameters = `{"type": "object", "properties": {"pattern": {"type": "string", "description": "Regex pattern for content search, or glob pattern (e.g., '*.py') for file search"}, "target": {"type": "string", "enum": ["content", "files"], "description": "'content' searches inside file contents, 'files' searches for files by name", "default": "content"}, "path": {"type": "string", "description": "Directory or file to search in (default: current working directory)", "default": "."}, "file_glob": {"type": "string", "description": "Filter files by pattern in grep mode (e.g., '*.py' to only search Python files)"}, "limit": {"type": "integer", "description": "Maximum number of results to return (default: 50)", "default": 50}, "offset": {"type": "integer", "description": "Skip first N results for pagination (default: 0)", "default": 0}, "order": {"type": "string", "enum": ["discovery", "modified"], "description": "File-search order: 'discovery' is fast bounded traversal order; 'modified' is exact global newest-first and may scan the full tree; ignored for content", "default": "discovery"}, "output_mode": {"type": "string", "enum": ["content", "files_only", "count"], "description": "Output format for grep mode: 'content' shows matching lines with line numbers, 'files_only' lists file paths, 'count' shows match counts per file", "default": "content"}, "context": {"type": "integer", "description": "Number of context lines before and after each match (grep mode only)", "default": 0}}, "required": ["pattern"]}`
)

type Review interface {
	Background(ctx context.Context, c *tool.Call) bool
	MarkSkillRead(ctx context.Context, c *tool.Call, resolved string) error
}

type EditRecord struct {
	Tool     string
	Mode     string
	Outcome  string
	Strategy string
}

type EditMetrics interface {
	Enabled(ctx context.Context) bool
	FileEdit(ctx context.Context, e EditRecord)
}

type taskOps struct {
	env tool.Environment
	ops *Operations
}

type Tasks struct {
	state     *State
	tracker   *Tracker
	admission *Admission

	mu  sync.Mutex
	ops map[string]taskOps
}

func NewTasks() *Tasks {
	return &Tasks{state: NewState(), tracker: NewTracker(), admission: NewAdmission(), ops: make(map[string]taskOps)}
}

func (t *Tasks) State() *State {
	return t.state
}

func (t *Tasks) Tracker() *Tracker {
	return t.tracker
}

func (t *Tasks) ClearFileOpsCache(taskID string) {
	t.mu.Lock()
	delete(t.ops, taskID)
	t.mu.Unlock()
	t.tracker.Forget(taskID)
	t.state.ForgetTask(taskID)
}

type ToolOptions struct {
	Config    *config.Store
	Homes     *home.Resolver
	Terminal  Terminal
	Tasks     *Tasks
	Safety    *Safety
	Lint      Linter
	Backends  *env.Providers
	Ripgrep   func(ctx context.Context) (string, error)
	RunEnv    func(ctx context.Context) ([]string, error)
	Redactor  *redact.Redactor
	Documents *docconv.Converter
	Check     *tool.Check
	Confirm   Confirmer
	Review    Review
	SkillLock SkillLock
	Marker    EditMarker
	Metrics   EditMetrics
	MainModel func(ctx context.Context) (provider, model string, err error)
}

type Tools struct {
	config    *config.Store
	homes     *home.Resolver
	terminal  Terminal
	paths     *Paths
	tasks     *Tasks
	safety    *Safety
	lint      Linter
	backends  *env.Providers
	search    SearchOptions
	redactor  *redact.Redactor
	documents *docconv.Converter
	check     *tool.Check
	confirm   Confirmer
	review    Review
	skillLock SkillLock
	marker    EditMarker
	metrics   EditMetrics
	mainModel func(ctx context.Context) (provider, model string, err error)
}

func NewTools(o ToolOptions) (*Tools, error) {
	missing := ""
	switch {
	case o.Config == nil:
		missing = "Config"
	case o.Homes == nil:
		missing = "Homes"
	case o.Terminal == nil:
		missing = "Terminal"
	case o.Tasks == nil:
		missing = "Tasks"
	case o.Safety == nil:
		missing = "Safety"
	case o.Lint == nil:
		missing = "Lint"
	case o.Ripgrep == nil:
		missing = "Ripgrep"
	case o.RunEnv == nil:
		missing = "RunEnv"
	case o.Redactor == nil:
		missing = "Redactor"
	case o.Documents == nil:
		missing = "Documents"
	case o.Check == nil:
		missing = "Check"
	case o.Confirm == nil:
		missing = "Confirm"
	case o.Review == nil:
		missing = "Review"
	case o.SkillLock == nil:
		missing = "SkillLock"
	case o.Marker == nil:
		missing = "Marker"
	case o.Metrics == nil:
		missing = "Metrics"
	case o.MainModel == nil:
		missing = "MainModel"
	}
	if missing != "" {
		return nil, &tool.UnfilledError{Name: "file.ToolOptions." + missing}
	}
	paths, err := NewPaths(o.Terminal, o.Homes)
	if err != nil {
		return nil, err
	}
	return &Tools{
		config: o.Config, homes: o.Homes, terminal: o.Terminal, paths: paths, tasks: o.Tasks, safety: o.Safety,
		lint: o.Lint, backends: o.Backends,
		search:   SearchOptions{Admission: o.Tasks.admission, Ripgrep: o.Ripgrep, RunEnv: o.RunEnv},
		redactor: o.Redactor, documents: o.Documents, check: o.Check, confirm: o.Confirm, review: o.Review,
		skillLock: o.SkillLock, marker: o.Marker, metrics: o.Metrics, mainModel: o.MainModel,
	}, nil
}

func (t *Tools) Paths() *Paths {
	return t.paths
}

func taskOf(c *tool.Call) string {
	if c.TaskID == "" {
		return defaultTaskID
	}
	return c.TaskID
}

func (t *Tools) ops(ctx context.Context, c *tool.Call) (*Operations, error) {
	if c.Env == nil {
		return nil, &tool.UnfilledError{Name: "tool.Call.Env"}
	}
	raw := taskOf(c)
	key, err := t.terminal.ContainerKey(ctx, raw)
	if err != nil {
		return nil, err
	}
	t.tasks.mu.Lock()
	cached, known := t.tasks.ops[key]
	t.tasks.mu.Unlock()
	if known {
		live, active, err := t.terminal.Active(ctx, raw)
		if err != nil {
			return nil, err
		}
		if !active || live != cached.env {
			if _, recorded := t.terminal.SessionCwd(ctx, raw); !recorded && cached.ops.cwd != "" {
				t.terminal.RecordSessionCwd(ctx, raw, cached.ops.cwd)
			}
			t.tasks.mu.Lock()
			delete(t.tasks.ops, key)
			t.tasks.mu.Unlock()
			known = false
		}
	}
	live, err := c.Env.Acquire(ctx, c)
	if err != nil {
		return nil, err
	}
	if known && live == cached.env {
		return cached.ops, nil
	}
	built, err := New(Options{Env: live, Config: t.config, Guard: t.safety, Lint: t.lint, Backends: t.backends, Search: t.search})
	if err != nil {
		return nil, err
	}
	t.tasks.mu.Lock()
	t.tasks.ops[key] = taskOps{env: live, ops: built}
	t.tasks.mu.Unlock()
	return built, nil
}

type fieldList []tool.Field

func (f *fieldList) set(key string, value any) {
	for i := range *f {
		if (*f)[i].Key == key {
			(*f)[i].Value = value
			return
		}
	}
	*f = append(*f, tool.Field{Key: key, Value: value})
}

func (f fieldList) has(key string) bool {
	for i := range f {
		if f[i].Key == key {
			return true
		}
	}
	return false
}

func validText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError || size > 1 {
			b.WriteString(s[i : i+size])
			i += size
			continue
		}
		if s[i] == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			i += 2
		}
		b.WriteString(replacementChar)
		i++
	}
	return b.String()
}

func cleanFields(fields []tool.Field) Object {
	out := make(Object, len(fields))
	for i, f := range fields {
		out[i] = tool.Field{Key: validText(f.Key), Value: cleanValue(f.Value)}
	}
	return out
}

func cleanValue(v any) any {
	switch value := v.(type) {
	case string:
		return validText(value)
	case []string:
		out := make([]string, len(value))
		for i, s := range value {
			out[i] = validText(s)
		}
		return out
	case Object:
		return cleanFields(value)
	case fieldList:
		return cleanFields(value)
	case []tool.Field:
		return cleanFields(value)
	case []Object:
		out := make([]Object, len(value))
		for i, o := range value {
			out[i] = cleanFields(o)
		}
		return out
	}
	return v
}

func encode(fields fieldList) tool.Result {
	return tool.JSON(cleanFields(fields))
}

func (t *Tools) failed(ctx context.Context, err error) (tool.Result, error) {
	var unfilled *tool.UnfilledError
	if errors.As(err, &unfilled) || ctx.Err() != nil {
		return tool.Result{}, err
	}
	return tool.Failure(validText(err.Error())), nil
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any, tool.Args:
		return "object"
	}
	return "number"
}

func truthy(v any) bool {
	switch value := v.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case json.Number:
		f, err := value.Float64()
		return err != nil || f != 0
	case float64:
		return value != 0
	case int:
		return value != 0
	case int64:
		return value != 0
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	case tool.Args:
		return len(value) > 0
	}
	return true
}

func truncatedInt(f float64, def int) int {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return def
	}
	f = math.Trunc(f)
	if f >= math.MaxInt {
		return math.MaxInt
	}
	if f <= math.MinInt {
		return math.MinInt
	}
	return int(f)
}

func coerceInt(v any, def int) int {
	switch value := v.(type) {
	case bool:
		if value {
			return 1
		}
		return 0
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return truncatedInt(value, def)
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return int(n)
		}
		f, err := value.Float64()
		if err != nil {
			return def
		}
		return truncatedInt(f, def)
	case string:
		text := trimSpace(value)
		negative := false
		if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
			negative = text[0] == '-'
			text = text[1:]
		}
		if text == "" || strings.HasPrefix(text, "_") || strings.HasSuffix(text, "_") || strings.Contains(text, "__") {
			return def
		}
		digits, ok := decimalText(strings.ReplaceAll(text, "_", ""))
		if !ok {
			return def
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			if negative {
				return math.MinInt
			}
			return math.MaxInt
		}
		if negative {
			return -n
		}
		return n
	}
	return def
}

func textArg(args tool.Args, key, def string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return def
}

func requiredText(args tool.Args, toolName, key, def string) (string, string) {
	raw, present := args[key]
	if !present {
		return def, ""
	}
	value, ok := raw.(string)
	if !ok {
		return "", toolName + ": '" + key + "' must be a string, got " + jsonTypeName(raw) + "."
	}
	return value, ""
}

func scopePath(args tool.Args, fallback []string) []string {
	if p, ok := args["path"].(string); ok && !isBlank(p) {
		return []string{p}
	}
	return fallback
}

func patchScopePaths(args tool.Args) []string {
	if mode, ok := args["mode"].(string); ok && mode == patchName {
		return tool.MutationTargets(patchName, args)
	}
	return scopePath(args, nil)
}

func isOpenAIFamily(provider, model string) bool {
	provider = strings.ToLower(trimSpace(provider))
	model = strings.ToLower(trimSpace(model))
	switch provider {
	case "openai", "openai-chat", "openai-codex", "azure-openai", "codex":
		return true
	}
	slug := model
	if _, after, found := strings.Cut(model, "/"); found {
		slug = after
	}
	for _, prefix := range [...]string{"gpt-", "gpt.", "chatgpt", "codex", "o1", "o3", "o4", "o5"} {
		if strings.HasPrefix(slug, prefix) {
			return true
		}
	}
	return strings.Contains(model, "openai/")
}

func (t *Tools) readSchema(ctx context.Context, _ tool.Names) (*tool.Override, bool) {
	if !t.documents.HostedOCRAvailable(ctx) {
		return nil, true
	}
	description := strings.Replace(readFileDescription, pdfTextLayer, pdfScannedOrText, 1)
	return &tool.Override{Description: &description}, true
}

func (t *Tools) patchSchema(ctx context.Context, _ tool.Names) (*tool.Override, bool) {
	provider, model, err := t.mainModel(ctx)
	if err != nil || !isOpenAIFamily(provider, model) {
		return nil, true
	}
	description := patchDualDescription
	return &tool.Override{Description: &description, Parameters: json.RawMessage(patchDualParameters)}, true
}

func searchFilesDescription() string {
	if runtime.GOOS == "darwin" {
		return searchFilesLead + searchFilesMacNote + searchFilesTail
	}
	return searchFilesLead + searchFilesTail
}

func (t *Tools) Tools() []tool.Tool {
	return []tool.Tool{
		{
			Name: readFileName, Toolset: toolsetName, Description: readFileDescription,
			Parameters: json.RawMessage(readFileParameters), Handler: t.handleRead, Check: t.check,
			Emoji: "\U0001F4D6", MaxResultChars: maxResultChars, DynamicSchema: t.readSchema,
			Concurrency: tool.PathReader, Effects: tool.Idempotent,
			ScopePaths: func(args tool.Args) []string { return scopePath(args, nil) },
		},
		{
			Name: writeFileName, Toolset: toolsetName, Description: writeFileDescription,
			Parameters: json.RawMessage(writeFileParameters), Handler: t.handleWrite, Check: t.check,
			Emoji: "✍" + string(rune(0xFE0F)), MaxResultChars: maxResultChars,
			Concurrency: tool.PathWriter, Effects: tool.Mutating,
			ScopePaths: func(args tool.Args) []string { return scopePath(args, nil) },
		},
		{
			Name: patchName, Toolset: toolsetName, Description: patchDescription,
			Parameters: json.RawMessage(patchParameters), Handler: t.handlePatch, Check: t.check,
			Emoji: "\U0001F527", MaxResultChars: maxResultChars, DynamicSchema: t.patchSchema,
			Concurrency: tool.PathWriter, Effects: tool.Mutating,
			ScopePaths: patchScopePaths,
		},
		{
			Name: searchFilesName, Toolset: toolsetName, Description: searchFilesDescription(),
			Parameters: json.RawMessage(searchFilesParameters), Handler: t.handleSearch, Check: t.check,
			Emoji: "\U0001F50E", MaxResultChars: maxResultChars,
			Concurrency: tool.PathReader, Effects: tool.Idempotent,
			ScopePaths: func(args tool.Args) []string { return scopePath(args, []string{"."}) },
		},
	}
}

func (t *Tools) handleRead(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	path, problem := requiredText(args, readFileName, "path", "")
	if problem != "" {
		return tool.Failure(problem), nil
	}
	result, err := t.readFile(ctx, c, path, coerceInt(args["offset"], DefaultReadOffset), coerceInt(args["limit"], DefaultReadLimit))
	if err != nil {
		return t.failed(ctx, err)
	}
	return result, nil
}

func (t *Tools) handleWrite(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return tool.Failure("write_file: missing required field 'path'. Re-emit the tool call with " +
			"both 'path' and 'content' set."), nil
	}
	raw, present := args["content"]
	if !present {
		return tool.Failure("write_file: missing required field 'content'. The tool call included a " +
			"path but no content argument — this is almost always a dropped-arg bug " +
			"under context pressure. Re-emit the tool call with the full content " +
			"payload, or use execute_code with icebath_tools.write_file() for very " +
			"large files."), nil
	}
	content, ok := raw.(string)
	if !ok {
		return tool.Failure("write_file: 'content' must be a string, got " + jsonTypeName(raw) + "."), nil
	}
	if exact, ok := c.ExactArg("content"); ok && exact.Held {
		content = exact.Text
		if exact.First != 0 {
			ctx = withModelSurrogates(ctx)
		}
	}
	crossProfile := truthy(args["cross_profile"])
	return t.recordEdit(ctx, c, writeFileName, wholeFileEditMode, func(ctx context.Context) (tool.Result, error) {
		return t.writeFile(WithEditOwner(ctx, editOwner(c)), c, path, content, crossProfile)
	})
}

func (t *Tools) handlePatch(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	request := patchRequest{mode: "replace", replaceAll: truthy(args["replace_all"]), crossProfile: truthy(args["cross_profile"])}
	if raw, present := args["mode"]; present {
		mode, ok := raw.(string)
		if !ok {
			encoded, err := json.Marshal(raw)
			if err != nil {
				return tool.Result{}, err
			}
			mode = string(encoded)
		}
		request.mode = mode
	}
	for _, text := range [...]struct {
		key   string
		value *string
		given *bool
	}{
		{key: "path", value: &request.path},
		{key: "old_string", value: &request.oldString, given: &request.hasOld},
		{key: "new_string", value: &request.newString, given: &request.hasNew},
		{key: "patch", value: &request.patch},
	} {
		raw, present := args[text.key]
		if !present || raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return tool.Failure("patch: '" + text.key + "' must be a string, got " + jsonTypeName(raw) + "."), nil
		}
		if exact, ok := c.ExactArg(text.key); ok && exact.Held {
			value = exact.Text
			if exact.First != 0 {
				ctx = withModelSurrogates(ctx)
			}
		}
		*text.value = value
		if text.given != nil {
			*text.given = true
		}
	}
	return t.recordEdit(ctx, c, patchName, request.mode, func(ctx context.Context) (tool.Result, error) {
		return t.patchFile(WithEditOwner(ctx, editOwner(c)), c, request)
	})
}

func (t *Tools) handleSearch(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	pattern, problem := requiredText(args, searchFilesName, "pattern", "")
	if problem != "" {
		return tool.Failure(problem), nil
	}
	target := textArg(args, "target", "content")
	switch target {
	case "grep":
		target = "content"
	case "find":
		target = "files"
	}
	path := "."
	if raw, present := args["path"]; present && raw != nil {
		value, ok := raw.(string)
		if !ok {
			return tool.Failure("search_files: 'path' must be a string, got " + jsonTypeName(raw) + "."), nil
		}
		if !isBlank(value) {
			path = value
		}
	}
	result, err := t.searchFiles(ctx, c, searchRequestArgs{
		pattern: pattern, target: target, path: path, fileGlob: textArg(args, "file_glob", ""),
		limit:  coerceInt(args["limit"], DefaultSearchLimit),
		offset: coerceInt(args["offset"], DefaultSearchOffset), outputMode: textArg(args, "output_mode", "content"),
		context: coerceInt(args["context"], 0), order: textArg(args, "order", "discovery"),
	})
	if err != nil {
		return t.failed(ctx, err)
	}
	return result, nil
}

type ownerKey struct{}

func WithEditOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

func EditOwner(ctx context.Context) string {
	owner, _ := ctx.Value(ownerKey{}).(string)
	return owner
}

func editOwner(c *tool.Call) string {
	if c.SessionID != "" {
		return c.SessionID
	}
	return taskOf(c)
}

type surrogateKey struct{}

func withModelSurrogates(ctx context.Context) context.Context {
	return context.WithValue(ctx, surrogateKey{}, true)
}

func modelSurrogates(ctx context.Context) bool {
	held, _ := ctx.Value(surrogateKey{}).(bool)
	return held
}

func loneSurrogate(s string) (rune, bool) {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), true
		}
	}
	return 0, false
}

type editProbe struct {
	strategies []string
	misses     []string
}

func (p *editProbe) EditMatched(strategy string) {
	p.strategies = append(p.strategies, strategy)
}

func (p *editProbe) EditMissed(reason string) {
	p.misses = append(p.misses, reason)
}

func strategyRank(name string) int {
	for i, s := range strategies {
		if s.name == name {
			return i
		}
	}
	return -1
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func editRecord(name, mode, resultText string, probe *editProbe) EditRecord {
	var data map[string]any
	if err := json.Unmarshal([]byte(resultText), &data); err != nil {
		data = nil
	}
	_, hasSuccess := data["success"]
	outcome := "failed"
	switch {
	case data["success"] == true || (len(data) > 0 && !hasSuccess && !truthy(data["error"])):
		outcome = "applied"
		if data["no_change"] == true {
			outcome = "already_applied"
		}
	case contains(probe.misses, missAmbiguous):
		outcome = missAmbiguous
	case contains(probe.misses, missNoMatch):
		outcome = missNoMatch
	}
	strategy := "none"
	if outcome == "applied" && len(probe.strategies) > 0 {
		strategy = probe.strategies[0]
		for _, s := range probe.strategies[1:] {
			if strategyRank(s) > strategyRank(strategy) {
				strategy = s
			}
		}
	}
	editMode := "replace"
	switch strings.ToLower(trimSpace(mode)) {
	case patchName:
		editMode = "v4a"
	case wholeFileEditMode:
		editMode = wholeFileEditMode
	}
	return EditRecord{Tool: name, Mode: editMode, Outcome: outcome, Strategy: strategy}
}

func (t *Tools) recordEdit(ctx context.Context, c *tool.Call, name, mode string, call func(ctx context.Context) (tool.Result, error)) (tool.Result, error) {
	if !t.metrics.Enabled(ctx) || t.review.Background(ctx, c) {
		return call(ctx)
	}
	probe := &editProbe{}
	result, err := call(WithMatchObserver(ctx, probe))
	if err != nil {
		return result, err
	}
	t.metrics.FileEdit(ctx, editRecord(name, mode, result.Text, probe))
	return result, nil
}
