package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	traceDefaultDataset   = "icebath-traces"
	traceVersion          = "icebath-agent"
	traceRedactionBlocked = "Trace upload blocked: secret redaction failed, so the transcript may " +
		"still contain credentials or other sensitive data. Fix the redactor or " +
		"rerun with --no-redact only after manually reviewing the transcript."
	traceNoTokenMessage = "Can't upload — no Hugging Face token is available. To set it up:\n" +
		"\n" +
		"1. Create a token with WRITE access at https://huggingface.co/settings/tokens\n" +
		"   (New token -> type \"Write\" -> copy it).\n" +
		"2. Add it to your environment as HF_TOKEN (e.g. in ~/.icebath/.env):\n" +
		"     HF_TOKEN=hf_xxxxxxxxxxxxxxxxxxxx\n" +
		"3. Run /upload-trace again (or `icebath trace upload`)."
)

var (
	traceTokenVars = []string{"HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "HUGGING_FACE_HUB_TOKEN", "HUGGINGFACE_TOKEN"}
)

// Raised when a trace cannot be safely redacted before upload
type traceRedactionError struct{ msg string }

func (e *traceRedactionError) Error() string     { return e.msg }
func (e *traceRedactionError) ErrorKind() string { return "TraceRedactionError" }

// UTC timestamp with milliseconds and a Z suffix
func traceNowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000") + "Z" }

// Forced secret redaction of a string body when enabled, a redactor failure refusing the upload
func traceRedact(text any, enabled bool) (out any, err error) {
	s, ok := text.(string)
	if !enabled || !ok || s == "" {
		return text, nil
	}
	defer func() {
		if p := recover(); p != nil {
			pkgLog().Warn(fmt.Sprintf("Trace upload redaction failed; refusing upload\n%v", p))
			out, err = nil, &traceRedactionError{traceRedactionBlocked}
		}
	}()
	return memory.RedactSensitiveText(s, memory.RedactOptions{}), nil
}

// Dict view of a message part, Go maps and ordered objects alike
func traceDict(v any) (func(string) (any, bool), bool) {
	switch d := v.(type) {
	case map[string]any:
		return func(k string) (any, bool) { x, ok := d[k]; return x, ok }, true
	case *jsonutil.Object:
		return d.Get, true
	}
	return nil, false
}

// Value of a key with a default, the dict get
func traceGet(get func(string) (any, bool), key string, def any) any {
	if v, ok := get(key); ok {
		return v
	}
	return def
}

// List view of a value, nil and false for anything that is not a list
func traceList(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = x
		}
		return out, true
	case []*jsonutil.Object:
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = x
		}
		return out, true
	}
	return nil, false
}

// json.dumps with default options
func traceDumps(v any) (string, error) {
	return jsonutil.Encode(v, jsonutil.Spaced(), jsonutil.ASCII())
}

// Text block with its body redacted
func traceTextBlock(text any, redact bool) (*jsonutil.Object, error) {
	body, err := traceRedact(text, redact)
	if err != nil {
		return nil, err
	}
	return jsonutil.ObjectOf("type", "text", "text", body), nil
}

// Anthropic content block of one OpenAI content part, images omitted
func tracePartToBlock(part any, redact bool) (*jsonutil.Object, error) {
	get, ok := traceDict(part)
	if !ok {
		return traceTextBlock(anyutil.String(part), redact)
	}
	switch traceGet(get, "type", nil) {
	case "text":
		return traceTextBlock(traceGet(get, "text", ""), redact)
	case "image_url", "image":
		return jsonutil.ObjectOf("type", "text", "text", "[image omitted]"), nil
	}
	text, err := traceDumps(part)
	if err != nil {
		return nil, err
	}
	return traceTextBlock(text, redact)
}

// Anthropic content blocks of a message content field
func traceContentToBlocks(content any, redact bool) ([]any, error) {
	if list, ok := traceList(content); ok {
		out := make([]any, 0, len(list))
		for _, part := range list {
			block, err := tracePartToBlock(part, redact)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
		}
		return out, nil
	}
	if content == nil {
		return []any{}, nil
	}
	text, ok := content.(string)
	if !ok {
		var err error
		if text, err = traceDumps(content); err != nil {
			return nil, err
		}
	}
	block, err := traceTextBlock(text, redact)
	if err != nil {
		return nil, err
	}
	return []any{block}, nil
}

// Parsed tool call arguments, the raw text kept under _raw when it is not JSON
func traceParseToolArgs(raw any) any {
	s, ok := raw.(string)
	if !ok {
		if _, isDict := traceDict(raw); isDict {
			return raw
		}
		return jsonutil.NewObject()
	}
	if strings.TrimSpace(s) == "" {
		return jsonutil.NewObject()
	}
	parsed, err := jsonutil.Decode(s)
	if err != nil {
		return jsonutil.ObjectOf("_raw", s)
	}
	return parsed
}

// Anthropic tool_use blocks of OpenAI tool calls, redacted arguments reparsed
func traceToolCallsToBlocks(toolCalls any, redact bool) ([]any, error) {
	list, _ := traceList(toolCalls)
	blocks := []any{}
	for _, tc := range list {
		get, ok := traceDict(tc)
		if !ok {
			continue
		}
		fnVal := traceGet(get, "function", nil)
		if !anyutil.IsTrue(fnVal) {
			fnVal = jsonutil.NewObject()
		}
		fn, ok := traceDict(fnVal)
		if !ok {
			return nil, &appClassedError{class: "AttributeError", msg: "trace tool call function is " + errchain.Kind(fnVal) + " where a mapping was expected"}
		}
		parsed := traceParseToolArgs(traceGet(fn, "arguments", nil))
		if redact {
			text, err := traceDumps(parsed)
			if err != nil {
				return nil, err
			}
			redacted, err := traceRedact(text, redact)
			if err != nil {
				return nil, err
			}
			if parsed, err = jsonutil.Decode(redacted.(string)); err != nil {
				pkgLog().Warn("Trace upload redacted tool arguments are not valid JSON; refusing upload")
				return nil, &traceRedactionError{traceRedactionBlocked}
			}
		}
		id := traceGet(get, "id", nil)
		if !anyutil.IsTrue(id) {
			id = "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
		}
		name := traceGet(fn, "name", nil)
		if !anyutil.IsTrue(name) {
			name = traceGet(get, "name", nil)
		}
		if !anyutil.IsTrue(name) {
			name = "tool"
		}
		blocks = append(blocks, jsonutil.ObjectOf("type", "tool_use", "id", id, "name", name, "input", parsed))
	}
	return blocks, nil
}

// Current git branch of cwd, empty outside a repository or on any failure
func traceGitBranch(ctx context.Context, cwd string) string {
	if cwd == "" {
		return ""
	}
	done, err := executil.Run(ctx, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"},
		executil.Options{Dir: cwd, Timeout: 3 * time.Second})
	if err != nil || done.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(done.Stdout), "\uFFFD"))
}

// Assistant turn with text and tool_use blocks, one empty text block when it has neither
func traceAssistantMessage(get func(string) (any, bool), model string, redact bool) (*jsonutil.Object, error) {
	blocks, err := traceContentToBlocks(traceGet(get, "content", nil), redact)
	if err != nil {
		return nil, err
	}
	calls, err := traceToolCallsToBlocks(traceGet(get, "tool_calls", nil), redact)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, calls...)
	if len(blocks) == 0 {
		blocks = []any{jsonutil.ObjectOf("type", "text", "text", "")}
	}
	if model == "" {
		model = "unknown"
	}
	return jsonutil.ObjectOf("role", "assistant", "model", model, "content", blocks), nil
}

// Tool result riding on a user turn keyed by the tool call id
func traceToolResultMessage(get func(string) (any, bool), redact bool) (*jsonutil.Object, error) {
	content := traceGet(get, "content", nil)
	text, ok := content.(string)
	if !ok {
		var err error
		if text, err = traceDumps(content); err != nil {
			return nil, err
		}
	}
	body, err := traceRedact(text, redact)
	if err != nil {
		return nil, err
	}
	id := traceGet(get, "tool_call_id", nil)
	if !anyutil.IsTrue(id) {
		id = traceGet(get, "tool_name", nil)
	}
	if !anyutil.IsTrue(id) {
		id = "tool"
	}
	return jsonutil.ObjectOf("role", "user", "content", []any{jsonutil.ObjectOf("type", "tool_result", "tool_use_id", id, "content", body)}), nil
}

// User turn with a redacted string or content blocks
func traceUserMessage(get func(string) (any, bool), redact bool) (*jsonutil.Object, error) {
	content := traceGet(get, "content", nil)
	if s, ok := content.(string); ok {
		body, err := traceRedact(s, redact)
		if err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("role", "user", "content", body), nil
	}
	blocks, err := traceContentToBlocks(content, redact)
	if err != nil {
		return nil, err
	}
	return jsonutil.ObjectOf("role", "user", "content", blocks), nil
}

// Claude Code JSONL of a transcript, one linked line per non system message
func buildTraceJSONL(ctx context.Context, messages []map[string]any, sessionID, model, cwd string, redact bool) (string, error) {
	var lines []string
	var parent any
	baseTS := traceNowISO()
	gitBranch := traceGitBranch(ctx, cwd)
	for _, msg := range messages {
		get, _ := traceDict(msg)
		role := traceGet(get, "role", nil)
		if role == "system" {
			continue
		}
		turnUUID := uuid.NewString()
		var lineType string
		var rendered *jsonutil.Object
		var err error
		switch role {
		case "assistant":
			lineType = "assistant"
			rendered, err = traceAssistantMessage(get, model, redact)
		case "tool":
			lineType = "user"
			rendered, err = traceToolResultMessage(get, redact)
		default:
			lineType = "user"
			rendered, err = traceUserMessage(get, redact)
		}
		if err != nil {
			return "", err
		}
		lineCwd := cwd
		if lineCwd == "" {
			if lineCwd, err = os.Getwd(); err != nil {
				return "", err
			}
		}
		entry := jsonutil.ObjectOf("parentUuid", parent, "isSidechain", false, "userType", "external", "cwd", lineCwd,
			"sessionId", sessionID, "version", traceVersion, "gitBranch", gitBranch, "uuid", turnUUID,
			"timestamp", baseTS, "type", lineType, "message", rendered)
		text, err := jsonutil.Encode(entry, jsonutil.Spaced())
		if err != nil {
			return "", err
		}
		lines = append(lines, text)
		parent = turnUUID
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// Hugging Face token from the usual environment variables
func traceResolveHFToken() string {
	for _, name := range traceTokenVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// Creates the dataset idempotently and pushes the trace file, a status string and never an error
func traceDoUpload(ctx context.Context, jsonl, token, sessionID, datasetName string, private bool) string {
	who, err := hfWhoami(ctx, token)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("HF whoami failed: %s", err.Error()))
		return "Your Hugging Face token was rejected (whoami failed). Make sure it has WRITE access and isn't expired."
	}
	var user any
	if obj, ok := who.(*jsonutil.Object); ok {
		user = obj.Value("name")
	}
	if !anyutil.IsTrue(user) {
		return "Could not resolve your Hugging Face username from the token."
	}
	repoID := anyutil.String(user) + "/" + datasetName
	if err := hfCreateDatasetRepo(ctx, token, repoID, private); err != nil {
		pkgLog().Warn(fmt.Sprintf("HF create_repo failed for %s: %s", repoID, err.Error()))
		return fmt.Sprintf("Could not create/access dataset %s: %s", repoID, err.Error())
	}
	pathInRepo := "sessions/" + sessionID + ".jsonl"
	if err := hfUploadFile(ctx, token, repoID, pathInRepo, []byte(jsonl), "add session trace "+sessionID); err != nil {
		pkgLog().Warn(fmt.Sprintf("HF upload_file failed for %s: %s", repoID, err.Error()))
		return "Upload to Hugging Face failed: " + err.Error()
	}
	return fmt.Sprintf("Uploaded -> https://huggingface.co/datasets/%s/blob/main/%s\n"+
		"View in the trace viewer: https://huggingface.co/datasets/%s", repoID, pathInRepo, repoID)
}

// Conversation and session row of a session, an empty row when it is missing
func traceLoadSessionMessages(ctx context.Context, sessionID, dbPath string) ([]map[string]any, *jsonutil.Object, error) {
	if dbPath == "" {
		dbPath = home.StatePath(home.Dir(ctx))
	}
	db, err := state.Acquire(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer state.ReleaseOrClose(db)
	resolved, err := db.ResolveSessionID(sessionID)
	if err != nil {
		return nil, nil, err
	}
	if resolved == "" {
		resolved = sessionID
	}
	meta, err := db.GetSession(ctx, resolved)
	if err != nil {
		return nil, nil, err
	}
	if meta == nil {
		meta = jsonutil.NewObject()
	}
	messages, err := db.GetMessagesAsConversation(ctx, resolved, state.ConversationOptions{})
	if err != nil {
		return nil, nil, err
	}
	return messages, meta, nil
}

// Loads, converts and uploads a session to the token owner dataset, returning the status line
func uploadSessionTrace(ctx context.Context, sessionID, cwd string, redact, private bool) (string, error) {
	return uploadSessionTraceWith(ctx, sessionID, "", cwd, redact, private, traceDefaultDataset, "", "")
}

// Trace upload with every keyword of the entry point
func uploadSessionTraceWith(ctx context.Context, sessionID, model, cwd string, redact, private bool, datasetName, dbPath, token string) (string, error) {
	if sessionID == "" {
		return "No active session to upload.", nil
	}
	if token == "" {
		token = traceResolveHFToken()
	}
	if token == "" {
		return traceNoTokenMessage, nil
	}
	messages, meta, err := traceLoadSessionMessages(ctx, sessionID, dbPath)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to load session %s for trace upload: %s", sessionID, err.Error()))
		return fmt.Sprintf("Could not load session %s: %s", sessionID, err.Error()), nil
	}
	if len(messages) == 0 {
		return "No transcript to upload for this session yet.", nil
	}
	if model == "" {
		if m := meta.Value("model"); anyutil.IsTrue(m) {
			model = anyutil.String(m)
		}
	}
	jsonl, err := buildTraceJSONL(ctx, messages, sessionID, model, cwd, redact)
	if err != nil {
		if _, ok := err.(*traceRedactionError); ok {
			return traceRedactionBlocked, nil
		}
		return "", err
	}
	if strings.TrimSpace(jsonl) == "" {
		return "No transcript content to upload for this session.", nil
	}
	return traceDoUpload(ctx, jsonl, token, sessionID, datasetName, private), nil
}
