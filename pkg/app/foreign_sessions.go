package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const foreignTitleMax = 60

var foreignWrapperTagRE = func() *regexp2.Regexp {
	re, err := regexutil.Compile(`^<(?:user_instructions|environment_context|recommended_plugins|`+
		`skills_instructions|permissions[_-]instructions|turn_context|`+
		`command-name|command-message|local-command-stdout|system-reminder)\b`, regexutil.IgnoreCase())
	if err != nil {
		panic(err)
	}
	return re
}()

// A discoverable session in the on disk store of another coding agent
type ForeignSession struct {
	Source     string
	Path       string
	Mtime      float64
	Cwd        *string
	TitleGuess *string
	TurnCount  int
	SessionID  *string
}

// Source tag and title, the file stem when no title was guessed, cut to sixty code points
func (s *ForeignSession) Label() string {
	title := ""
	if s.TitleGuess != nil {
		title = strings.TrimSpace(*s.TitleGuess)
	}
	if title == "" {
		title = pathutil.Stem(s.Path)
	}
	return "[" + foreignSourceLabel(s.Source) + "] " + runePrefix(title, foreignTitleMax)
}

// Display name of a foreign source, the source itself when unknown
func foreignSourceLabel(source string) string {
	if label, ok := foreignSourceLabels[source]; ok {
		return label
	}
	return source
}

// One role and text turn of an imported conversation
type foreignTurn struct{ role, content string }

// Normalized turns and metadata of one foreign session file
type foreignParsed struct {
	turns                      []foreignTurn
	cwd, titleGuess, sessionID *string
}

// Parsed JSON objects of a log file, unparseable lines and non object values skipped
func readForeignJSONLines(path string) []*jsonutil.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []*jsonutil.Object
	for _, line := range strings.SplitAfter(textutil.NormalizeNewlines(strings.ToValidUTF8(string(textutil.TrimBOM(raw)), string(utf8.RuneError))), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		obj, err := jsonutil.Decode(line)
		if err != nil {
			continue
		}
		if o, ok := obj.(*jsonutil.Object); ok {
			out = append(out, o)
		}
	}
	return out
}

// Plain text of one content block, empty for tool results, reasoning and unknown types
func foreignBlockText(block any) string {
	if s, ok := block.(string); ok {
		return s
	}
	obj, ok := block.(*jsonutil.Object)
	if !ok {
		return ""
	}
	btype, _ := obj.Value("type").(string)
	switch btype {
	case "text", "input_text", "output_text":
		text, _ := obj.Value("text").(string)
		return text
	case "tool_use":
		name := obj.Value("name")
		if !anyutil.IsTrue(name) {
			name = "tool"
		}
		return "[ran tool: " + anyutil.String(name) + "]"
	case "image":
		return "[image]"
	}
	return ""
}

// Message content flattened to plain text, blocks joined by blank lines
func foreignFlattenBlocks(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	list, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range list {
		if p := strings.TrimSpace(foreignBlockText(b)); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Merged same role turns in strict alternation, a leading assistant reply getting one synthetic user opener
func foreignMergeTurns(raw []foreignTurn) []foreignTurn {
	var merged []foreignTurn
	for _, t := range raw {
		text := strings.TrimSpace(t.content)
		if text == "" {
			continue
		}
		if len(merged) > 0 && merged[len(merged)-1].role == t.role {
			merged[len(merged)-1].content += "\n\n" + text
		} else {
			merged = append(merged, foreignTurn{t.role, text})
		}
	}
	if len(merged) > 0 && merged[0].role == "assistant" {
		merged = append([]foreignTurn{{"user", "(imported conversation begins with an assistant reply)"}}, merged...)
	}
	return merged
}

// Role and text turn of one message, false when it is not importable
func foreignMessageTurn(message any) (foreignTurn, bool) {
	obj, ok := message.(*jsonutil.Object)
	if !ok {
		return foreignTurn{}, false
	}
	role, _ := obj.Value("role").(string)
	if role != "user" && role != "assistant" {
		return foreignTurn{}, false
	}
	text := foreignFlattenBlocks(obj.Value("content"))
	if text == "" {
		return foreignTurn{}, false
	}
	if role == "user" {
		if wrapped, _ := foreignWrapperTagRE.MatchString(strings.TrimLeftFunc(text, unicode.IsSpace)); wrapped {
			return foreignTurn{}, false
		}
	}
	return foreignTurn{role, text}, true
}

// First line of the first non blank user turn cut to twice the title width, nil without one
func foreignFirstUserLine(turns []foreignTurn) *string {
	for _, t := range turns {
		if t.role != "user" {
			continue
		}
		head, _, _ := strings.Cut(strings.TrimSpace(t.content), "\n")
		if line := strings.TrimSpace(head); line != "" {
			cut := runePrefix(line, foreignTitleMax*2)
			return &cut
		}
	}
	return nil
}

// Parse result with merged turns and the title falling back to the first user line
func foreignParsedOf(turns []foreignTurn, cwd, sessionID, title *string) foreignParsed {
	if title == nil {
		title = foreignFirstUserLine(turns)
	}
	return foreignParsed{turns: foreignMergeTurns(turns), cwd: cwd, titleGuess: title, sessionID: sessionID}
}

// Turns and metadata of one Claude Code session log
func parseClaudeSession(path string) foreignParsed {
	var turns []foreignTurn
	var cwd, summary, sessionID *string
	for _, obj := range readForeignJSONLines(path) {
		otype, _ := obj.Value("type").(string)
		if otype == "summary" {
			if s, ok := obj.Value("summary").(string); ok && strings.TrimSpace(s) != "" {
				stripped := strings.TrimSpace(s)
				summary = &stripped
			}
		} else if (otype == "user" || otype == "assistant") &&
			!(anyutil.IsTrue(obj.Value("isSidechain")) || anyutil.IsTrue(obj.Value("isMeta"))) {
			if s, ok := obj.Value("cwd").(string); cwd == nil && ok {
				cwd = &s
			}
			if s, ok := obj.Value("sessionId").(string); sessionID == nil && ok {
				sessionID = &s
			}
			if turn, ok := foreignMessageTurn(obj.Value("message")); ok {
				turns = append(turns, turn)
			}
		}
	}
	return foreignParsedOf(turns, cwd, sessionID, summary)
}

// Turns and metadata of one Codex CLI rollout log
func parseCodexSession(path string) foreignParsed {
	var turns []foreignTurn
	var cwd, sessionID *string
	for _, obj := range readForeignJSONLines(path) {
		payload, ok := obj.Value("payload").(*jsonutil.Object)
		if !ok {
			continue
		}
		switch otype, _ := obj.Value("type").(string); otype {
		case "session_meta":
			if s, ok := payload.Value("cwd").(string); ok {
				cwd = &s
			}
			sid := payload.Value("session_id")
			if !anyutil.IsTrue(sid) {
				sid = payload.Value("id")
			}
			if s, ok := sid.(string); ok {
				sessionID = &s
			}
		case "response_item":
			ptype, _ := payload.Value("type").(string)
			if ptype == "message" {
				if turn, ok := foreignMessageTurn(payload); ok {
					turns = append(turns, turn)
				}
			} else if ptype == "custom_tool_call" || ptype == "function_call" || ptype == "local_shell_call" {
				name := payload.Value("name")
				if !anyutil.IsTrue(name) {
					name = payload.Value("tool")
				}
				if !anyutil.IsTrue(name) {
					name = "tool"
				}
				turns = append(turns, foreignTurn{"assistant", "[ran tool: " + anyutil.String(name) + "]"})
			}
		}
	}
	return foreignParsedOf(turns, cwd, sessionID, nil)
}

// Store layout of one foreign source
type foreignSourceSpec struct {
	defaultParts      []string
	envVar, envSubdir string
	pattern           string
	recursive         bool
	parse             func(string) foreignParsed
}

var foreignSources = map[string]foreignSourceSpec{
	"claude": {[]string{".claude", "projects"}, "CLAUDE_CONFIG_DIR", "projects", "*/*.jsonl", false, parseClaudeSession},
	"codex":  {[]string{".codex", "sessions"}, "CODEX_HOME", "sessions", "rollout-*.jsonl", true, parseCodexSession},
}

// Log parser of a known source
func foreignParser(source string) func(string) foreignParsed { return foreignSources[source].parse }

// Default session store of a source honoring the relocation variable of the tool, blank meaning unset
func foreignDefaultRoot(source string) (string, error) {
	spec := foreignSources[source]
	if override := strings.TrimSpace(os.Getenv(spec.envVar)); override != "" {
		expanded, err := pathutil.ExpandUserPath(override)
		if err != nil {
			return "", err
		}
		return pathutil.Join(expanded, spec.envSubdir), nil
	}
	home, err := pathutil.Home()
	if err != nil {
		return "", err
	}
	return pathutil.Join(home, spec.defaultParts...), nil
}

// Path, modification time and size of one regular foreign log file
type foreignFile struct {
	path  string
	mtime float64
	size  int64
}

// Regular log files of a source under a root, newest first, escaping symlinks and unreadable entries skipped
func foreignWalk(source, root string) ([]foreignFile, error) {
	spec := foreignSources[source]
	if root == "" {
		var err error
		if root, err = foreignDefaultRoot(source); err != nil {
			return nil, err
		}
	}
	root, err := pathutil.Resolve(root)
	if err != nil {
		return nil, err
	}
	var found []foreignFile
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return found, nil
	}
	pattern := spec.pattern
	if spec.recursive {
		pattern = "**/" + pattern
	}
	matches, err := pathutil.Glob(root, pattern)
	if err != nil {
		return nil, err
	}
	for _, path := range matches {
		resolved, err := pathutil.Resolve(path)
		if err != nil {
			continue
		}
		st, err := os.Stat(resolved)
		if err != nil {
			continue
		}
		if _, under := pathutil.RelativeTo(resolved, root); under && st.Mode().IsRegular() {
			mt := st.ModTime()
			found = append(found, foreignFile{resolved, float64(mt.Unix()) + float64(mt.Nanosecond())*1e-9, st.Size()})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].mtime > found[j].mtime })
	return found, nil
}

// Sessions with at least one conversation turn among the log files of a source
func foreignListSessions(source, root string) ([]*ForeignSession, error) {
	files, err := foreignWalk(source, root)
	if err != nil {
		return nil, err
	}
	results := []*ForeignSession{}
	for _, f := range files {
		parsed := foreignParser(source)(f.path)
		if len(parsed.turns) > 0 {
			results = append(results, &ForeignSession{Source: source, Path: f.path, Mtime: f.mtime, Cwd: parsed.cwd,
				TitleGuess: parsed.titleGuess, TurnCount: len(parsed.turns), SessionID: parsed.sessionID})
		}
	}
	return results, nil
}

// Foreign sessions across the chosen source or both, newest first, capped at limit when limit is positive
func gatherForeignSessions(source, claudeRoot, codexRoot string, limit int) ([]*ForeignSession, error) {
	sessions := []*ForeignSession{}
	for _, pair := range [][2]string{{"claude", claudeRoot}, {"codex", codexRoot}} {
		if source != "" && source != pair[0] {
			continue
		}
		listed, err := foreignListSessions(pair[0], pair[1])
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, listed...)
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Mtime > sessions[j].Mtime })
	if limit != 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

// Numbered picker over the newest foreign sessions, nil when nothing was chosen
func PickForeignSession(ctx context.Context, source string, w io.Writer) (*ForeignSession, error) {
	sessions, err := gatherForeignSessions(source, "", "", 25)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		where, ok := foreignSourceLabels[source]
		if !ok {
			where = "Claude Code or Codex CLI"
		}
		fmt.Fprintf(w, "No %s sessions found on this machine.\n", where)
		return nil, nil
	}
	fmt.Fprintln(w, "Foreign sessions (newest first):")
	for i, s := range sessions {
		ws := ""
		if s.Cwd != nil && *s.Cwd != "" {
			base := sessionsBasename(strings.TrimRight(*s.Cwd, "/"))
			if base == "" {
				base = *s.Cwd
			}
			ws = "  (" + base + ")"
		}
		stamp := foreignLocalTime(s.Mtime).Format("2006-01-02 15:04")
		fmt.Fprintf(w, "  %2d. %s  %s%s  [%d turns]\n", i+1, stamp, s.Label(), ws, s.TurnCount)
	}
	con := console()
	if !con.StdinIsTTY() {
		fmt.Fprint(w, "Non-interactive terminal — pass the file path directly:\n"+
			"  icebath sessions import --from claude|codex <path>\n")
		return nil, nil
	}
	prompter := &cliui.Console{In: con.In, Out: w, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}
	answer, err := prompter.Input(fmt.Sprintf("Import which session? [1-%d, empty to cancel] ", len(sessions)))
	if err != nil {
		if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
			return nil, nil
		}
		return nil, err
	}
	raw := strings.TrimSpace(answer)
	if raw == "" {
		return nil, nil
	}
	idx, err := textutil.ParseInt(raw)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		fmt.Fprintf(w, "Not a number: %s\n", raw)
		return nil, nil
	}
	if idx >= 1 && idx <= int64(len(sessions)) {
		return sessions[idx-1], nil
	}
	fmt.Fprintf(w, "Out of range: %s\n", raw)
	return nil, nil
}

// Local wall clock time of a float timestamp, the seconds floored
func foreignLocalTime(ts float64) time.Time {
	sec := int64(ts)
	if float64(sec) > ts {
		sec--
	}
	return time.Unix(sec, int64((ts-float64(sec))*1e9)).Local()
}

// Gateway request surface an import goes through
type foreignSessionCaller interface {
	Call(ctx context.Context, method string, params map[string]any) (any, error)
}

// Imports one foreign session through the gateway session store and returns the new session id
func ImportForeignSession(ctx context.Context, source, path string) (string, error) {
	return importForeignSession(ctx, gatewaycli.SessionStore(), source, path)
}

// Imports one foreign session through the gateway mutate import and a cosmetic rename, ValueError on bad input
func importForeignSession(ctx context.Context, store foreignSessionCaller, source, path string) (string, error) {
	source = strings.TrimLeft(strings.ToLower(strings.TrimSpace(source)), "@")
	if _, ok := foreignSourceLabels[source]; !ok {
		return "", &appValueError{"Unknown foreign session source: " + errchain.Show(source)}
	}
	expanded, err := pathutil.ExpandUserPath(path)
	if err != nil {
		return "", err
	}
	expanded = filepath.Clean(expanded)
	if st, err := os.Stat(expanded); err != nil || !st.Mode().IsRegular() {
		return "", &appValueError{"Session file not found: " + expanded}
	}
	parsed := foreignParser(source)(expanded)
	if len(parsed.turns) == 0 {
		return "", &appValueError{"No user/assistant conversation turns found in " + expanded}
	}
	firstUser := pathutil.Stem(expanded)
	if line := foreignFirstUserLine(parsed.turns); line != nil {
		firstUser = *line
	}
	if len([]rune(firstUser)) > foreignTitleMax {
		firstUser = runePrefix(firstUser, foreignTitleMax-1) + "…"
	}
	sessionID := state.NewSessionID()
	var cwd any
	if parsed.cwd != nil && *parsed.cwd != "" {
		if st, err := os.Stat(*parsed.cwd); err == nil && st.IsDir() {
			cwd = *parsed.cwd
		}
	}
	messages := make([]any, 0, len(parsed.turns))
	for _, t := range parsed.turns {
		msg := jsonutil.NewObject()
		msg.Set("role", t.role)
		msg.Set("content", t.content)
		messages = append(messages, msg)
	}
	row := jsonutil.NewObject()
	row.Set("id", sessionID)
	row.Set("source", "cli")
	row.Set("cwd", cwd)
	row.Set("messages", messages)
	result, err := store.Call(ctx, "session.mutate", map[string]any{"session_id": sessionID, "request_id": uuid4HexText(),
		"expected_revision": 0, "operation": "import", "payload": map[string]any{"sessions": []any{row}}})
	if err != nil {
		return "", err
	}
	receipt, ok := jsonutil.AsObject(result)
	if !ok {
		return "", &appClassedError{class: "TypeError", msg: "session import receipt is " + errchain.Kind(result) + " where a mapping was expected"}
	}
	revision, ok := receipt.Get("revision")
	if !ok {
		return "", &appClassedError{class: "KeyError", msg: "session import receipt has no revision"}
	}
	_, err = store.Call(ctx, "session.mutate", map[string]any{"session_id": sessionID, "request_id": uuid4HexText(),
		"expected_revision": revision, "operation": "rename",
		"payload": map[string]any{"title": "Imported from " + foreignSourceLabels[source] + ": " + firstUser}})
	var refused *gateway.GatewayClientError
	if err != nil && !errors.As(err, &refused) {
		return "", err
	}
	return sessionID, nil
}

// Value the user or a reply gave that a command rejects, its text the message alone
type appValueError struct{ msg string }

func (e *appValueError) Error() string { return e.msg }

// Failure carrying the kind its callers decide on, its text the message alone
type appClassedError struct{ class, msg string }

func (e *appClassedError) Error() string     { return e.msg }
func (e *appClassedError) ErrorKind() string { return e.class }

// Random version four UUID as 32 hex digits
func uuid4HexText() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }
