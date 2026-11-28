// Package export writes sessions out: a JSONL backup, Markdown and QMD transcripts that verify themselves, a browsable HTML page, the
// prompts the user sent, and Claude Code JSONL traces
package export

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/tools/redact"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

var roleNames = map[sessionsv1.Role]string{
	sessionsv1.Role_ROLE_USER:      "user",
	sessionsv1.Role_ROLE_ASSISTANT: "assistant",
	sessionsv1.Role_ROLE_TOOL:      "tool",
	sessionsv1.Role_ROLE_SYSTEM:    "system",
}

// Role of a message as exports name it
func Role(m *sessionsv1.Message) string {
	if name, ok := roleNames[m.GetRole()]; ok {
		return name
	}
	return "unknown"
}

// Time as exports write it: UTC with microseconds
func iso(t *timestamppb.Timestamp) any {
	if t == nil {
		return nil
	}
	return t.AsTime().UTC().Format("2006-01-02T15:04:05.000000Z")
}

func seconds(t *timestamppb.Timestamp) any {
	if t == nil {
		return nil
	}
	return float64(t.AsTime().UnixNano()) / 1e9
}

// Folder under icebath's own folder of the user, such as where exports go
func Folder(name string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "icebath", name), nil
}

// Messages a person reads of a stored transcript: those taken back by an undo left out
func Kept(messages []*sessionsv1.Message) []*sessionsv1.Message {
	return slices.DeleteFunc(slices.Clone(messages), func(m *sessionsv1.Message) bool { return m.GetRemoved() })
}

// Transcripts with every text in them masked of secrets: messages, titles, prompts, directories and settings
func Redact(transcripts []*sessionsv1.Transcript) []*sessionsv1.Transcript {
	out := make([]*sessionsv1.Transcript, len(transcripts))
	for i, t := range transcripts {
		copied := proto.Clone(t).(*sessionsv1.Transcript)
		mask := func(text string) string { return redact.File(text, true) }
		s := copied.GetSession()
		s.Title, s.Directory, s.ModelConfig, s.Preview, s.Activity = mask(s.GetTitle()), mask(s.GetDirectory()), mask(s.GetModelConfig()),
			mask(s.GetPreview()), mask(s.GetActivity())
		if s.Peer != nil {
			s.Peer.Origin = mask(s.Peer.GetOrigin())
		}
		copied.SystemPrompt = mask(copied.GetSystemPrompt())
		for _, m := range copied.GetMessages() {
			m.Text, m.Reasoning, m.Sent = mask(m.GetText()), mask(m.GetReasoning()), mask(m.GetSent())
			for _, call := range m.GetToolCalls() {
				call.Arguments = mask(call.GetArguments())
			}
		}
		out[i] = copied
	}
	return out
}

// Record of a session in the JSONL backup: its fields, its messages, and the timings between them
func record(t *sessionsv1.Transcript) map[string]any {
	s := t.GetSession()
	usage, peer := s.GetUsage(), s.GetPeer()
	out := map[string]any{
		"id": s.GetId(), "profile_name": s.GetProfile(), "source": s.GetSource(), "created_source": strs.Nullable(s.GetCreatedSource()),
		"user_id": strs.Nullable(peer.GetUserId()), "session_key": strs.Nullable(peer.GetSessionKey()), "chat_id": strs.Nullable(peer.GetChatId()),
		"chat_type": strs.Nullable(peer.GetChatType()), "thread_id": strs.Nullable(peer.GetThreadId()), "display_name": strs.Nullable(peer.GetDisplayName()),
		"origin_json": strs.Nullable(peer.GetOrigin()), "model": strs.Nullable(s.GetModel()), "model_config": strs.Nullable(s.GetModelConfig()),
		"system_prompt": strs.Nullable(t.GetSystemPrompt()), "parent_session_id": strs.Nullable(s.GetParentId()), "started_at": seconds(s.GetCreated()),
		"ended_at": seconds(s.GetEnded()), "end_reason": strs.Nullable(s.GetEndReason()), "message_count": s.GetMessages(), "tool_call_count": s.GetToolCalls(),
		"input_tokens": usage.GetInput(), "output_tokens": usage.GetOutput(), "cache_read_tokens": usage.GetCacheRead(),
		"cache_write_tokens": usage.GetCacheWrite(), "reasoning_tokens": usage.GetReasoning(), "api_call_count": usage.GetApiCalls(),
		"cwd": strs.Nullable(s.GetDirectory()), "git_branch": strs.Nullable(s.GetGitBranch()), "git_repo_root": strs.Nullable(s.GetGitRoot()),
		"billing_provider": strs.Nullable(s.GetProvider()), "billing_base_url": strs.Nullable(s.GetBaseUrl()), "estimated_cost_usd": usage.GetEstimatedCost(),
		"actual_cost_usd": usage.GetActualCost(), "cost_status": strs.Nullable(usage.GetCostStatus()), "cost_source": strs.Nullable(usage.GetCostSource()),
		"pricing_version": strs.Nullable(usage.GetPricingVersion()), "title": strs.Nullable(s.GetTitle()), "title_source": strs.Nullable(s.GetTitleSource()),
		"last_activity_at": seconds(s.GetUpdated()), "last_activity_description": strs.Nullable(s.GetActivity()), "rewind_count": s.GetRewinds(),
		"archived": s.GetArchived(), "auto_archived": s.GetAutoArchived(), "pinned": s.GetPinned(), "hidden": s.GetHidden(), "yolo": s.GetYolo(),
		"last_read_at": seconds(s.GetRead()), "last_active": seconds(s.GetUpdated()),
	}
	if s.GetBranchedFrom() != "" {
		out["_branched_from"] = s.GetBranchedFrom()
	}
	messages := make([]map[string]any, len(t.GetMessages()))
	for i, m := range t.GetMessages() {
		messages[i] = message(s.GetId(), m)
	}
	out["messages"] = messages
	out["timings"] = timings(t.GetMessages())
	return out
}

func message(session string, m *sessionsv1.Message) map[string]any {
	var calls any
	if len(m.GetToolCalls()) > 0 {
		list := make([]map[string]any, len(m.GetToolCalls()))
		for i, call := range m.GetToolCalls() {
			list[i] = map[string]any{"id": call.GetId(), "type": "function", "function": map[string]any{"name": call.GetName(), "arguments": call.GetArguments()}}
		}
		calls = list
	}
	out := map[string]any{
		"id": m.GetId(), "session_id": session, "role": Role(m), "content": m.GetText(), "tool_call_id": strs.Nullable(m.GetToolCallId()), "tool_calls": calls,
		"tool_name": strs.Nullable(m.GetToolName()), "timestamp": seconds(m.GetTime()), "token_count": m.GetTokens(), "finish_reason": strs.Nullable(m.GetFinishReason()),
		"reasoning": strs.Nullable(m.GetReasoning()), "platform_message_id": strs.Nullable(m.GetPlatformId()), "active": !m.GetCompacted() && !m.GetRemoved(), "compacted": m.GetCompacted(),
		"api_content": strs.Nullable(m.GetSent()), "display_kind": strs.Nullable(m.GetDisplay()), "message_uid": m.GetUid(), "absorbed_message_uids": m.GetAbsorbed(),
		"failed": m.GetFailed(), "model_only": m.GetModelOnly(),
	}
	if m.GetSummary() {
		out["_compressed_summary"] = true
	}
	if len(m.GetCarriers()) > 0 {
		out["carriers"] = m.GetCarriers()
	}
	if len(m.GetMedia()) > 0 {
		media := make([]map[string]any, len(m.GetMedia()))
		for i, item := range m.GetMedia() {
			media[i] = map[string]any{"mime_type": item.GetMimeType(), "data": item.GetData()}
		}
		out["media"] = media
	}
	return out
}

// How long the session took between its messages, read from their times
func timings(messages []*sessionsv1.Message) map[string]any {
	out := map[string]any{"source": "message_timestamps", "complete": false, "unavailable_reason": nil}
	var stamped []*sessionsv1.Message
	roles := map[string]int{}
	order := []string{}
	emitted := 0
	for _, m := range messages {
		role := Role(m)
		if _, seen := roles[role]; !seen {
			order = append(order, role)
		}
		roles[role]++
		emitted += len(m.GetToolCalls())
		if m.GetTime() != nil {
			stamped = append(stamped, m)
		}
	}
	counts := make([][2]any, 0, len(order))
	for _, role := range order {
		counts = append(counts, [2]any{role, roles[role]})
	}
	out["role_counts"] = orderedCounts(counts)
	out["tool_result_count"] = roles["tool"]
	out["tool_calls_emitted"] = emitted
	out["message_timestamps"] = map[string]int{"available": len(stamped), "missing": len(messages) - len(stamped)}
	out["available"] = len(stamped) > 0
	if len(stamped) == 0 {
		out["unavailable_reason"] = "no_timestamped_messages"
		out["first_message_at"], out["last_message_at"], out["wall_clock_ms"], out["largest_gap_ms"] = nil, nil, nil, nil
		out["intervals"] = []any{}
		return out
	}
	firstAt, lastAt := stamped[0].GetTime().AsTime(), stamped[len(stamped)-1].GetTime().AsTime()
	out["first_message_at"], out["last_message_at"] = iso(stamped[0].GetTime()), iso(stamped[len(stamped)-1].GetTime())
	out["wall_clock_ms"] = max(lastAt.Sub(firstAt).Milliseconds(), 0)
	var largest int64
	intervals := []map[string]any{}
	for i := 1; i < len(stamped); i++ {
		gap := max(stamped[i].GetTime().AsTime().Sub(stamped[i-1].GetTime().AsTime()).Milliseconds(), 0)
		largest = max(largest, gap)
		intervals = append(intervals, map[string]any{"from_message_id": stamped[i-1].GetId(), "to_message_id": stamped[i].GetId(),
			"from_role": Role(stamped[i-1]), "to_role": Role(stamped[i]), "gap_ms": gap})
	}
	out["largest_gap_ms"], out["intervals"] = largest, intervals
	return out
}

// Counts by role in the order roles first appeared
type orderedCounts [][2]any

func (o orderedCounts) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, pair := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(pair[0])
		value, _ := json.Marshal(pair[1])
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// JSON of a value with text left as it is rather than escaped for HTML
func encode(value any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// JSONL backup: one line per session with every message it holds
func JSONL(transcripts []*sessionsv1.Transcript) ([]byte, error) {
	var b bytes.Buffer
	for _, t := range transcripts {
		line, err := encode(record(t))
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// Snapshot of one session in the shape of the JSONL backup, indented for reading, as /save json writes it
func JSON(t *sessionsv1.Transcript) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record(t)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Name a single session's file takes: its id with only letters, digits, dashes and underscores
func DefaultName(id, format string) string {
	kept := strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return r
		}
		return -1
	}, id)
	if kept == "" {
		kept = "session"
	}
	return "icebath_session_" + kept + "." + format
}

// Messages of a transcript a person reads: the system ones left out
func shown(messages []*sessionsv1.Message) []*sessionsv1.Message {
	return slices.DeleteFunc(slices.Clone(messages), func(m *sessionsv1.Message) bool { return m.GetRole() == sessionsv1.Role_ROLE_SYSTEM })
}

func stamp(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format("2006-01-02T15:04:05Z")
}

// Text in a code fence one backtick longer than any run of backticks it holds
func fenced(text, language string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + language + "\n" + text + "\n" + fence
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// Metadata bullets of a session heading a Markdown export
func bullets(s *sessionsv1.Session) []string {
	lines := []string{"- Session ID: `" + s.GetId() + "`"}
	if s.GetSource() != "" {
		lines = append(lines, "- Source: `"+s.GetSource()+"`")
	}
	if s.GetModel() != "" {
		lines = append(lines, "- Model: `"+s.GetModel()+"`")
	}
	if s.GetTitle() != "" {
		lines = append(lines, "- Title: "+oneLine(s.GetTitle()))
	}
	if s.GetCreated() != nil {
		lines = append(lines, "- Started: "+stamp(s.GetCreated()))
	}
	return append(lines, fmt.Sprintf("- Messages: %d", s.GetMessages()))
}

// Markdown transcript of the sessions, as /save writes it
func Markdown(transcripts []*sessionsv1.Transcript) []byte {
	var lines []string
	level := "#"
	if len(transcripts) > 1 {
		lines, level = append(lines, "# icebath sessions export", ""), "##"
	}
	for _, t := range transcripts {
		s := t.GetSession()
		lines = append(lines, level+" Session: "+oneLine(cmp.Or(s.GetTitle(), s.GetId())), "")
		lines = append(append(lines, bullets(s)...), "")
		messages := shown(t.GetMessages())
		if len(messages) == 0 {
			lines = append(lines, "_No messages found._", "")
			continue
		}
		mark := level + "#"
		for _, m := range messages {
			suffix := ""
			if m.GetTime() != nil {
				suffix = " - " + stamp(m.GetTime())
			}
			text := m.GetText()
			switch m.GetRole() {
			case sessionsv1.Role_ROLE_TOOL:
				name := cmp.Or(m.GetToolName(), "tool")
				lines = append(lines, mark+" Tool: "+name+suffix, "", "<details><summary>"+htmlEscape(name)+"</summary>", "", fenced(text, "text"), "",
					"</details>", "")
			case sessionsv1.Role_ROLE_USER:
				lines = append(lines, mark+" User"+suffix, "", text, "")
			default:
				lines = append(lines, mark+" Assistant"+suffix, "", text, "")
				for _, call := range m.GetToolCalls() {
					lines = append(lines, fenced(call.GetName()+" "+call.GetArguments(), "text"), "")
				}
			}
		}
	}
	return []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n")
}

// One prompt the user sent, as the user-prompts view writes it
type prompt struct {
	Session string `json:"session_id"`
	Index   int    `json:"index"`
	Created any    `json:"created_at"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	Message int64  `json:"message_id,omitempty"`
	Event   string `json:"event_id,omitempty"`
}

func prompts(t *sessionsv1.Transcript) []prompt {
	var out []prompt
	for _, m := range t.GetMessages() {
		if m.GetRole() != sessionsv1.Role_ROLE_USER {
			continue
		}
		p := prompt{Session: t.GetSession().GetId(), Index: len(out) + 1, Role: "user", Text: m.GetText(), Message: m.GetId(), Event: m.GetPlatformId()}
		if m.GetTime() != nil {
			p.Created = stamp(m.GetTime())
		}
		out = append(out, p)
	}
	return out
}

// Prompts the user sent, one JSON line each, and how many
func PromptsJSONL(transcripts []*sessionsv1.Transcript) ([]byte, int, error) {
	var b bytes.Buffer
	count := 0
	for _, t := range transcripts {
		for _, p := range prompts(t) {
			line, err := encode(p)
			if err != nil {
				return nil, 0, err
			}
			b.Write(line)
			b.WriteByte('\n')
			count++
		}
	}
	return b.Bytes(), count, nil
}

// Prompts the user sent as Markdown sections, session by session
func PromptsMarkdown(transcripts []*sessionsv1.Transcript) []byte {
	if len(transcripts) == 0 {
		return []byte("_No user prompts found._\n")
	}
	var lines []string
	level := 1
	if len(transcripts) > 1 {
		lines, level = append(lines, "# User prompts export", ""), 2
	}
	for _, t := range transcripts {
		id := t.GetSession().GetId()
		if level == 1 {
			lines = append(lines, "# User prompts for session "+id)
		} else {
			lines = append(lines, "## Session "+id)
		}
		lines = append(append(lines, bullets(t.GetSession())...), "")
		list := prompts(t)
		if len(list) == 0 {
			lines = append(lines, "_No user prompts found._", "")
		}
		for _, p := range list {
			created := "timestamp unavailable"
			if p.Created != nil {
				created = p.Created.(string)
			}
			lines = append(lines, fmt.Sprintf("%s %d. %s", strings.Repeat("#", level+1), p.Index, created))
			if p.Message != 0 {
				lines = append(lines, fmt.Sprintf("Message ID: `%d`", p.Message))
			}
			lines = append(lines, "", p.Text, "")
		}
	}
	return []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n")
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// File name of a session's Markdown or QMD export: its id and a slug of its title
func FileName(s *sessionsv1.Session, format string) string {
	slug := strings.ToLower(strings.Trim(slugUnsafe.ReplaceAllString(strings.TrimSpace(s.GetTitle()), "-"), ".-_"))
	if runes := []rune(slug); len(runes) > 60 {
		slug = string(runes[:60])
	}
	return s.GetId() + "-" + cmp.Or(slug, "session") + "." + format
}

const digestPlaceholder = "__SHA256_PLACEHOLDER__"

var digestLine = regexp.MustCompile("- SHA256 of exported body: `([0-9a-f]{64})`")

func frontValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprint(v)
	case int, int64, float64, []string:
		data, _ := json.Marshal(v)
		return string(data)
	}
	data, _ := encode(fmt.Sprint(value))
	return string(data)
}

// Markdown or QMD export of a session, or of its compression lineage oldest first, that carries a digest of itself; and how many messages it
// holds
func Memory(segments []*sessionsv1.Transcript, format string, at time.Time) ([]byte, int) {
	head := segments[len(segments)-1].GetSession()
	root := segments[0].GetSession()
	count := 0
	for _, t := range segments {
		count += len(t.GetMessages())
	}
	pairs := [][2]any{{"session_id", head.GetId()}, {"title", strs.Nullable(head.GetTitle())}, {"source", head.GetSource()},
		{"created_at", cmpAny(iso(root.GetCreated()), "")}, {"updated_at", cmpAny(iso(head.GetUpdated()), "")}, {"ended_at", cmpAny(iso(head.GetEnded()), "")},
		{"model", strs.Nullable(head.GetModel())}, {"provider", strs.Nullable(head.GetProvider())}, {"cwd", strs.Nullable(head.GetDirectory())},
		{"archived", head.GetArchived()}, {"message_count", count}, {"tool_call_count", head.GetToolCalls()}}
	if len(segments) > 1 {
		ids := make([]string, len(segments))
		for i, t := range segments {
			ids[i] = t.GetSession().GetId()
		}
		pairs = append(pairs, [2]any{"lineage_session_ids", ids})
	}
	exported := at.UTC().Format("2006-01-02T15:04:05.000000Z")
	pairs = append(pairs, [2]any{"format", format}, [2]any{"exported_at", exported}, [2]any{"exporter", "icebath sessions export (md/qmd) v1"})
	front := []string{"---"}
	for _, pair := range pairs {
		front = append(front, pair[0].(string)+": "+frontValue(pair[1]))
	}
	front = append(front, "---")
	blocks := []string{strings.Join(front, "\n"), "# " + cmp.Or(head.GetTitle(), head.GetId()), "Session ID: `" + head.GetId() + "`"}
	if head.GetSource() != "" {
		blocks = append(blocks, "Source: `"+head.GetSource()+"`")
	}
	if head.GetDirectory() != "" {
		blocks = append(blocks, "Working directory: `"+head.GetDirectory()+"`")
	}
	blocks = append(blocks, "## Messages")
	if count == 0 {
		blocks[len(blocks)-1] += "\n\n_No messages in this session._"
	}
	var calls []map[string]any
	for _, t := range segments {
		if len(segments) > 1 {
			blocks = append(blocks, "## Compression segment: "+t.GetSession().GetId())
		}
		for _, m := range t.GetMessages() {
			role := Role(m)
			heading := "### " + strs.Capital(role)
			if m.GetRole() == sessionsv1.Role_ROLE_TOOL && m.GetToolName() != "" {
				heading = "### Tool — " + m.GetToolName()
			}
			if m.GetTime() != nil {
				heading += " — " + iso(m.GetTime()).(string)
			}
			blocks = append(blocks, heading, strings.TrimRight(m.GetText(), " \t\n"))
			for _, call := range m.GetToolCalls() {
				calls = append(calls, map[string]any{"id": call.GetId(), "type": "function", "function": map[string]any{"name": call.GetName(), "arguments": call.GetArguments()}})
			}
		}
	}
	if len(calls) > 0 {
		data, _ := json.MarshalIndent(calls, "", "  ")
		blocks = append(blocks, "## Tool calls", "```json\n"+string(data)+"\n```")
	}
	blocks = append(blocks, "## Export verification", strings.Join([]string{
		"- Session id: `" + head.GetId() + "`",
		fmt.Sprintf("- Exported messages: `%d`", count),
		fmt.Sprintf("- Source DB message count at export: `%d`", head.GetMessages()),
		"- Exported at: `" + exported + "`",
		"- SHA256 of exported body: `" + digestPlaceholder + "`",
	}, "\n"))
	body := strings.TrimRight(strings.Join(blocks, "\n\n"), " \t\n") + "\n"
	sum := sha256.Sum256([]byte(strings.ReplaceAll(body, "`"+digestPlaceholder+"`", "`pending`")))
	return []byte(strings.ReplaceAll(body, digestPlaceholder, hex.EncodeToString(sum[:]))), count
}

// Whether a written Markdown or QMD export is whole: its digest matches its body, and it names the session and count it should
func Verify(data []byte, id string, count int) string {
	if !utf8.Valid(data) {
		return "the file is not text"
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	match := digestLine.FindStringSubmatch(text)
	if match == nil {
		return "the file has no SHA-256 line"
	}
	sum := sha256.Sum256([]byte(strings.Replace(text, match[0], "- SHA256 of exported body: `pending`", 1)))
	switch {
	case hex.EncodeToString(sum[:]) != match[1]:
		return "the SHA-256 in the file does not match its content"
	case !strings.Contains(text, fmt.Sprintf("- Exported messages: `%d`", count)):
		return "the file holds another number of messages than the session"
	case !strings.Contains(text, "- Session id: `"+id+"`"):
		return "the file names another session"
	}
	return "ok"
}

// Line the manifest of an export folder gets for one file
func ManifestLine(head *sessionsv1.Session, lineage []string, format, path string, count int, data []byte, at time.Time) ([]byte, error) {
	sum := sha256.Sum256(data)
	if len(lineage) == 0 {
		lineage = []string{head.GetId()}
	}
	line, err := json.Marshal(map[string]any{"exported_at": float64(at.UnixNano()) / 1e9, "format": format, "lineage_session_ids": lineage,
		"message_count": count, "path": path, "session_id": head.GetId(), "sha256": hex.EncodeToString(sum[:])})
	return append(line, '\n'), err
}

func cmpAny(value any, fallback string) any {
	if value == nil {
		return fallback
	}
	return value
}

func htmlEscape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(text)
}
