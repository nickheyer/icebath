package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Tools whose sessions import, by the name --from takes
var foreignLabels = map[string]string{"claude": "Claude Code", "codex": "Codex CLI"}

// User messages a tool writes itself, which are not part of the conversation
var scaffolding = regexp.MustCompile(`(?i)^<(user_instructions|environment_context|recommended_plugins|skills_instructions|permissions[_-]instructions|turn_context|` +
	`command-name|command-message|local-command-stdout|system-reminder)\b`)

// Session of another tool read from its file
type foreign struct {
	tool, path, id, cwd, title string
	modified                   time.Time
	messages                   []*sessionsv1.Message
}

func (f *foreign) turns() int {
	n := 0
	for _, m := range f.messages {
		if m.GetRole() == sessionsv1.Role_ROLE_USER || m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && m.GetText() != "" {
			n++
		}
	}
	return n
}

// Folder a tool keeps its sessions in
func foreignRoot(tool string) string {
	home, _ := os.UserHomeDir()
	expand := func(path string) string {
		if rest, ok := strings.CutPrefix(path, "~"); ok {
			return home + rest
		}
		return path
	}
	if tool == "claude" {
		if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
			return filepath.Join(expand(dir), "projects")
		}
		return filepath.Join(home, ".claude", "projects")
	}
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return filepath.Join(expand(dir), "sessions")
	}
	return filepath.Join(home, ".codex", "sessions")
}

// Session files of a tool on this machine, newest first, each with at least one turn
func discover(tool string) []*foreign {
	root := foreignRoot(tool)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	var paths []string
	if tool == "claude" {
		paths, _ = filepath.Glob(filepath.Join(resolved, "*", "*.jsonl"))
	} else {
		filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasPrefix(entry.Name(), "rollout-") && strings.HasSuffix(entry.Name(), ".jsonl") {
				paths = append(paths, path)
			}
			return nil
		})
	}
	var out []*foreign
	for _, path := range paths {
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(real, resolved+string(filepath.Separator)) {
			continue
		}
		if info, err := os.Stat(real); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if session, err := parseForeign(tool, real); err == nil && session.turns() > 0 {
			out = append(out, session)
		}
	}
	slices.SortFunc(out, func(a, b *foreign) int { return b.modified.Compare(a.modified) })
	return out
}

// Objects of the lines of a JSONL file, lines that are not objects skipped
func lines(path string) ([]map[string]any, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	info, _ := os.Stat(path)
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	text := strings.ReplaceAll(strings.ToValidUTF8(string(data), "�"), "\r\n", "\n")
	var out []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var object map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(scanner.Text())), &object) == nil && object != nil {
			out = append(out, object)
		}
	}
	return out, info.ModTime(), scanner.Err()
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

// Session of the tool's file: its turns as icebath messages, tool calls, their results and reasoning kept
func parseForeign(tool, path string) (*foreign, error) {
	objects, modified, err := lines(path)
	if err != nil {
		return nil, err
	}
	session := &foreign{tool: tool, path: path, modified: modified}
	add := func(m *sessionsv1.Message, at string) {
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			m.Time = timestamppb.New(t)
		}
		session.messages = append(session.messages, m)
	}
	for _, object := range objects {
		if tool == "claude" {
			switch str(object["type"]) {
			case "summary":
				if summary := strings.TrimSpace(str(object["summary"])); summary != "" {
					session.title = summary
				}
			case "user", "assistant":
				if object["isSidechain"] == true || object["isMeta"] == true {
					continue
				}
				if session.cwd == "" {
					session.cwd = str(object["cwd"])
				}
				if session.id == "" {
					session.id = str(object["sessionId"])
				}
				message, _ := object["message"].(map[string]any)
				for _, m := range claudeMessage(message) {
					add(m, str(object["timestamp"]))
				}
			}
			continue
		}
		payload, ok := object["payload"].(map[string]any)
		if !ok {
			continue
		}
		switch str(object["type"]) {
		case "session_meta":
			session.cwd = str(payload["cwd"])
			session.id = cmp.Or(str(payload["session_id"]), str(payload["id"]))
		case "response_item":
			if m := codexItem(payload); m != nil {
				add(m, str(object["timestamp"]))
			}
		}
	}
	session.messages = merge(session.messages)
	if session.title == "" {
		for _, m := range session.messages {
			if m.GetRole() == sessionsv1.Role_ROLE_USER {
				line, _, _ := strings.Cut(strings.TrimSpace(m.GetText()), "\n")
				session.title = strs.Head(line, 120)
				break
			}
		}
	}
	return session, nil
}

// Messages of one Claude Code message: text and tool calls of a reply, results of tools and the text of a person
func claudeMessage(message map[string]any) []*sessionsv1.Message {
	role := str(message["role"])
	if role != "user" && role != "assistant" {
		return nil
	}
	if text, ok := message["content"].(string); ok {
		return textMessage(role, text, "")
	}
	blocks, _ := message["content"].([]any)
	var texts, reasoning []string
	var calls []*sessionsv1.ToolCall
	var results []*sessionsv1.Message
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		switch str(block["type"]) {
		case "text":
			if text := strings.TrimSpace(str(block["text"])); text != "" {
				texts = append(texts, text)
			}
		case "thinking":
			if text := strings.TrimSpace(str(block["thinking"])); text != "" {
				reasoning = append(reasoning, text)
			}
		case "image":
			texts = append(texts, "[image]")
		case "tool_use":
			arguments, _ := json.Marshal(block["input"])
			calls = append(calls, &sessionsv1.ToolCall{Id: str(block["id"]), Name: cmp.Or(str(block["name"]), "tool"), Arguments: string(arguments)})
		case "tool_result":
			results = append(results, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: str(block["tool_use_id"]), Text: resultText(block["content"]),
				Failed: block["is_error"] == true})
		}
	}
	out := results
	if len(texts) > 0 || len(calls) > 0 || len(reasoning) > 0 {
		out = append(out, textMessage(role, strings.Join(texts, "\n\n"), strings.Join(reasoning, "\n\n"))...)
		if len(out) > 0 {
			out[len(out)-1].ToolCalls = calls
		}
	}
	return out
}

// Text of a tool result, its parts joined
func resultText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	var parts []string
	list, _ := content.([]any)
	for _, raw := range list {
		part, _ := raw.(map[string]any)
		if text := strings.TrimSpace(str(part["text"])); text != "" {
			parts = append(parts, text)
		} else if str(part["type"]) == "image" {
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n\n")
}

// Message of text the person or the model wrote, none for scaffolding a tool wrote itself
func textMessage(role, text, reasoning string) []*sessionsv1.Message {
	if role == "user" {
		if scaffolding.MatchString(strings.TrimLeft(text, " \t\r\n")) {
			return nil
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_USER, Text: text}}
	}
	return []*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: text, Reasoning: reasoning}}
}

// Message of one Codex response item: a message, a tool call, the output of one, or reasoning
func codexItem(payload map[string]any) *sessionsv1.Message {
	switch str(payload["type"]) {
	case "message":
		role := str(payload["role"])
		if role != "user" && role != "assistant" {
			return nil
		}
		var texts []string
		blocks, _ := payload["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			switch str(block["type"]) {
			case "input_text", "output_text", "text":
				if text := strings.TrimSpace(str(block["text"])); text != "" {
					texts = append(texts, text)
				}
			case "input_image", "image":
				texts = append(texts, "[image]")
			}
		}
		messages := textMessage(role, strings.Join(texts, "\n\n"), "")
		if len(messages) == 0 || messages[0].GetText() == "" {
			return nil
		}
		return messages[0]
	case "function_call", "custom_tool_call", "local_shell_call":
		arguments := cmp.Or(str(payload["arguments"]), str(payload["input"]))
		if arguments == "" {
			data, _ := json.Marshal(payload["action"])
			arguments = string(data)
		}
		name := cmp.Or(str(payload["name"]), "shell")
		return &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, ToolCalls: []*sessionsv1.ToolCall{{Id: cmp.Or(str(payload["call_id"]), str(payload["id"])),
			Name: name, Arguments: arguments}}}
	case "function_call_output", "custom_tool_call_output":
		output := payload["output"]
		text := str(output)
		if object, ok := output.(map[string]any); ok {
			text = str(object["content"])
		}
		return &sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: str(payload["call_id"]), Text: text}
	case "reasoning":
		var parts []string
		summary, _ := payload["summary"].([]any)
		for _, raw := range summary {
			part, _ := raw.(map[string]any)
			if text := strings.TrimSpace(str(part["text"])); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Reasoning: strings.Join(parts, "\n\n")}
	}
	return nil
}

// Conversation as icebath keeps it: a reply's reasoning, text and tool calls in one message, text of the same role in a row joined, tool
// results naming their tools, and a conversation that opens with a reply given a user message before it
func merge(messages []*sessionsv1.Message) []*sessionsv1.Message {
	var out []*sessionsv1.Message
	names := map[string]string{}
	for _, m := range messages {
		for _, call := range m.GetToolCalls() {
			names[call.GetId()] = call.GetName()
		}
		if m.GetRole() == sessionsv1.Role_ROLE_TOOL {
			m.ToolName = names[m.GetToolCallId()]
		}
		if n := len(out); n > 0 && m.GetRole() != sessionsv1.Role_ROLE_TOOL && out[n-1].GetRole() == m.GetRole() && len(out[n-1].GetToolCalls()) == 0 {
			last := out[n-1]
			last.Text = strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{last.GetText(), m.GetText()}, func(s string) bool { return s == "" }), "\n\n"))
			last.Reasoning = strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{last.GetReasoning(), m.GetReasoning()}, func(s string) bool { return s == "" }), "\n\n"))
			last.ToolCalls = m.GetToolCalls()
			continue
		}
		out = append(out, m)
	}
	if len(out) > 0 && out[0].GetRole() != sessionsv1.Role_ROLE_USER {
		out = append([]*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_USER, Text: "(imported conversation begins with an assistant reply)"}}, out...)
	}
	return out
}

func addImport(parent *cobra.Command, at func(*cobra.Command) *run) {
	var from string
	command := &cobra.Command{Use: "import [path]", Short: "Import a Claude Code or Codex CLI session into Icebath",
		Long: "Pull a conversation started in Claude Code (~/.claude/projects) or Codex CLI (~/.codex/sessions) into the Icebath session store so it can be " +
			"resumed from the sessions screen. The foreign files are only read, never modified.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			from = strings.TrimPrefix(strings.ToLower(from), "@")
			if from != "" && foreignLabels[from] == "" {
				return fmt.Errorf("invalid choice: '%s' (choose from 'claude', 'codex')", from)
			}
			var session *foreign
			if len(args) == 1 {
				path := args[0]
				if rest, ok := strings.CutPrefix(path, "~"); ok {
					home, _ := os.UserHomeDir()
					path = home + rest
				}
				if _, err := os.Stat(path); err != nil {
					return fmt.Errorf("Error: file not found: %s", args[0])
				}
				tool := from
				if tool == "" {
					lowered := filepath.ToSlash(path)
					if strings.Contains(lowered, "/.claude/") || strings.HasSuffix(lowered, ".jsonl") && strings.Contains(lowered, "claude") {
						tool = "claude"
					}
					if strings.Contains(lowered, "/.codex/") || strings.HasPrefix(filepath.Base(lowered), "rollout-") {
						tool = "codex"
					}
				}
				if tool == "" {
					return errors.New("Cannot infer source from path; pass --from claude|codex.")
				}
				parsed, err := parseForeign(tool, filepath.Clean(path))
				if err != nil {
					return fmt.Errorf("Session file not found: %s", path)
				}
				if parsed.turns() == 0 {
					return fmt.Errorf("No user/assistant conversation turns found in %s", path)
				}
				session = parsed
			} else {
				var err error
				if session, err = r.pick(from); err != nil || session == nil {
					return err
				}
			}
			return r.importForeign(session)
		}}
	command.Flags().StringVar(&from, "from", "", "Which tool to import from (default: pick across both)")
	parent.AddCommand(command)
}

// Session the person picks from those of the tool, or of both, newest first; nil when cancelled
func (r *run) pick(tool string) (*foreign, error) {
	var found []*foreign
	label := "Claude Code or Codex CLI"
	for _, name := range []string{"claude", "codex"} {
		if tool == "" || tool == name {
			found = append(found, discover(name)...)
		}
	}
	if tool != "" {
		label = foreignLabels[tool]
	}
	slices.SortFunc(found, func(a, b *foreign) int { return b.modified.Compare(a.modified) })
	found = found[:min(25, len(found))]
	if len(found) == 0 {
		r.printf("No %s sessions found on this machine.", label)
		return nil, nil
	}
	r.print("Foreign sessions (newest first):")
	for i, f := range found {
		name := strs.Head(cmp.Or(f.title, strings.TrimSuffix(filepath.Base(f.path), ".jsonl")), 60)
		where := ""
		if f.cwd != "" {
			where = "  (" + filepath.Base(f.cwd) + ")"
		}
		r.printf("  %2d. %s  [%s] %s%s  [%d turns]", i+1, f.modified.Local().Format("2006-01-02 15:04"), foreignLabels[f.tool], name, where, f.turns())
	}
	if !term.IsTerminal(os.Stdin.Fd()) {
		r.print("Non-interactive terminal — pass the file path directly:\n  icebath sessions import --from claude|codex <path>\n")
		return nil, nil
	}
	fmt.Fprintf(r.cmd.OutOrStdout(), "Import which session? [1-%d, empty to cancel] ", len(found))
	line, _ := bufio.NewReader(r.cmd.InOrStdin()).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	var choice int
	if _, err := fmt.Sscanf(line, "%d", &choice); err != nil {
		r.print("Not a number: " + line)
		return nil, nil
	}
	if choice < 1 || choice > len(found) {
		r.print("Out of range: " + line)
		return nil, nil
	}
	return found[choice-1], nil
}

// Stores the foreign session as a new one with where it came from, unless it was imported before
func (r *run) importForeign(f *foreign) error {
	label := foreignLabels[f.tool]
	listed, err := r.service.List(r.ctx(), &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{Source: f.tool, IncludeArchived: true, IncludeHidden: true,
		IncludeEmpty: true}})
	if err != nil {
		return err
	}
	for _, s := range listed.GetSessions() {
		var origin struct {
			From struct {
				Tool, Path string
				Foreign    string `json:"foreign_session_id"`
			} `json:"imported_from"`
		}
		if json.Unmarshal([]byte(s.GetPeer().GetOrigin()), &origin) == nil && origin.From.Tool == f.tool &&
			(f.id != "" && origin.From.Foreign == f.id || f.id == "" && origin.From.Path == f.path) {
			r.printf("✓ %s session already imported as %s", label, s.GetId())
			return nil
		}
	}
	origin, _ := json.Marshal(map[string]any{"imported_from": map[string]string{"tool": f.tool, "path": f.path, "foreign_session_id": f.id}})
	first := strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
	for _, m := range f.messages {
		if m.GetRole() == sessionsv1.Role_ROLE_USER {
			first, _, _ = strings.Cut(strings.TrimSpace(m.GetText()), "\n")
			break
		}
	}
	if utf8.RuneCountInString(first) > 60 {
		first = strs.Head(first, 59) + "…"
	}
	cwd := ""
	if info, err := os.Stat(f.cwd); f.cwd != "" && err == nil && info.IsDir() {
		cwd = f.cwd
	}
	id := time.Now().Format("20060102_150405") + "_" + fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	created := timestamppb.New(f.modified)
	for _, m := range f.messages {
		if m.GetTime() != nil {
			created = m.GetTime()
			break
		}
	}
	response, err := r.service.Import(r.ctx(), &sessionsv1.ImportRequest{Transcripts: []*sessionsv1.Transcript{{
		Session: &sessionsv1.Session{Id: id, Source: f.tool, Directory: cwd, Created: created, Title: "Imported from " + label + ": " + first,
			TitleSource: "user", Peer: &sessionsv1.Peer{Origin: string(origin)}, EndReason: "imported"},
		Messages: f.messages,
	}}})
	if err != nil {
		return errors.New("Error: " + tui.Reason(err))
	}
	if len(response.GetIds()) == 0 {
		return errors.New("Error: the session was not imported")
	}
	r.printf("✓ Imported %s session as %s", label, response.GetIds()[0])
	r.printf("  Continue it with: icebath --resume %s", response.GetIds()[0])
	return nil
}
