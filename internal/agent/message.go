// Conversation turn loop of one session and the contract its hosts and subpackages build on
package agent

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
)

type Message struct {
	wire.Message
	Row                 state.Message
	APIContent          *string
	Scaffold            Scaffold
	Withheld            string
	Risk                *tool.Risk
	MergedPrefix        string
	InflightRestatement bool
	Summary             string
	SummaryHasUserTurn  *bool
	MicroCompact        bool
	CompactionTail      bool

	kindBeforeSummary sql.Null[string]
	hiddenBySummary   bool
}

type Scaffold uint8

const (
	ScaffoldNone Scaffold = iota
	ScaffoldEmptyRecovery
	ScaffoldEmptyTerminal
	ScaffoldThinkingPrefill
	ScaffoldVerificationStop
	ScaffoldPreVerify
	ScaffoldKanbanStop
	ScaffoldDroppedToolCallNudge
)

var scaffoldFlags = [...]string{
	ScaffoldNone:                 "",
	ScaffoldEmptyRecovery:        "_empty_recovery_synthetic",
	ScaffoldEmptyTerminal:        "_empty_terminal_sentinel",
	ScaffoldThinkingPrefill:      "_thinking_prefill",
	ScaffoldVerificationStop:     "_verification_stop_synthetic",
	ScaffoldPreVerify:            "_pre_verify_synthetic",
	ScaffoldKanbanStop:           "_kanban_stop_synthetic",
	ScaffoldDroppedToolCallNudge: "_dropped_toolcall_nudge",
}

func (s Scaffold) Flag() string {
	if int(s) >= len(scaffoldFlags) {
		return ""
	}
	return scaffoldFlags[s]
}

func ScaffoldOf(flag string) Scaffold {
	if flag == "" {
		return ScaffoldNone
	}
	for i, name := range scaffoldFlags {
		if name == flag {
			return Scaffold(i)
		}
	}
	return ScaffoldNone
}

func ScaffoldOfMembers(members []wire.Member) Scaffold {
	for i := 1; i < len(scaffoldFlags); i++ {
		for _, mb := range members {
			if mb.Key != scaffoldFlags[i] {
				continue
			}
			if v, err := wire.ParseValue(mb.Value); err == nil && v.Truthy() {
				return Scaffold(i)
			}
		}
	}
	return ScaffoldNone
}

func (m *Message) Ephemeral() bool {
	return m != nil && m.Scaffold != ScaffoldNone
}

const (
	WithheldPromptNotice = "[Message withheld: a pre_llm_call policy hook blocked it.]"

	withheldPromptKey       = "withheld_prompt"
	inflightRestatementKey  = "compaction_inflight_task_restatement"
	dataUseKey              = "data_use"
	hiddenDisplayKind       = "hidden"
	steerDisplayKind        = "steer"
	multimodalResultSummary = "[multimodal tool result]"

	toolCallArgumentsCorruptionMarker = "[icebath: tool call arguments were corrupted in this session and " +
		"have been dropped to keep the conversation alive. See issue #15236.]"
)

var ErrNoStoredMessage = errors.New("agent: no stored message to decode")

func mintUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

func epochSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func NewMessage(m wire.Message, now time.Time) *Message {
	out := &Message{Message: m}
	out.StampUID()
	out.StampTimestamp(now)
	return out
}

func (m *Message) StampUID() string {
	if m.Row.UID == "" {
		m.Row.UID = mintUID()
	}
	return m.Row.UID
}

func (m *Message) StampTimestamp(now time.Time) {
	if !m.Row.Timestamp.Valid {
		m.Row.Timestamp = sql.Null[float64]{V: epochSeconds(now), Valid: true}
	}
}

func (m *Message) StampTimestampAt(seconds float64) {
	if !m.Row.Timestamp.Valid {
		m.Row.Timestamp = sql.Null[float64]{V: seconds, Valid: true}
	}
}

func Append(msgs []*Message, m *Message, now time.Time) []*Message {
	m.StampTimestamp(now)
	return append(msgs, m)
}

func AppendAt(msgs []*Message, m *Message, seconds *float64, now time.Time) []*Message {
	if seconds != nil {
		m.StampTimestampAt(*seconds)
	} else {
		m.StampTimestamp(now)
	}
	return append(msgs, m)
}

func metadataValue(raw json.RawMessage) wire.Value {
	if len(raw) == 0 {
		return wire.Value{}
	}
	v, err := wire.ParseValue(raw)
	if err != nil || !v.IsObject() {
		return wire.Value{}
	}
	return v
}

func metadataWith(raw json.RawMessage, key string, value json.RawMessage) json.RawMessage {
	d := wire.DictOf(metadataValue(raw))
	d.Set(key, value)
	return d.JSON()
}

func metadataWithout(raw json.RawMessage, key string) json.RawMessage {
	v := metadataValue(raw)
	if !v.Get(key).Exists() {
		return raw
	}
	d := wire.DictOf(v)
	d.Delete(key)
	return d.JSON()
}

func nullText(s string, valid bool) sql.Null[string] {
	return sql.Null[string]{V: s, Valid: valid}
}

func cloneRow(r *state.Message) state.Message {
	out := *r
	out.ToolCalls = slices.Clone(r.ToolCalls)
	out.ReasoningDetails = slices.Clone(r.ReasoningDetails)
	out.CodexReasoningItems = slices.Clone(r.CodexReasoningItems)
	out.CodexMessageItems = slices.Clone(r.CodexMessageItems)
	out.DisplayMetadata = slices.Clone(r.DisplayMetadata)
	out.AbsorbedUIDs = slices.Clone(r.AbsorbedUIDs)
	out.DataUse = slices.Clone(r.DataUse)
	out.AbsorbedRowIDs = slices.Clone(r.AbsorbedRowIDs)
	out.RetiredDurableRows = slices.Clone(r.RetiredDurableRows)
	for i := range out.RetiredDurableRows {
		out.RetiredDurableRows[i].ToolCallIDs = slices.Clone(out.RetiredDurableRows[i].ToolCallIDs)
	}
	if r.ToolCallUIDs != nil {
		out.ToolCallUIDs = make(state.ToolCallUIDs, len(r.ToolCallUIDs))
		for id, uid := range r.ToolCallUIDs {
			out.ToolCallUIDs[id] = state.ToolCallUID{One: uid.One, Many: slices.Clone(uid.Many)}
		}
	}
	if r.Canonical != nil {
		c := *r.Canonical
		c.Message = cloneRow(&r.Canonical.Message)
		out.Canonical = &c
	}
	return out
}

func Decode(row *state.Message) (*Message, error) {
	if row == nil {
		return nil, ErrNoStoredMessage
	}
	d := &wire.Dict{}
	d.SetString("role", row.Role)
	if text, ok := row.Content.Text(); ok {
		d.SetString("content", text)
	} else if raw, ok := row.Content.JSON(); ok && !row.Content.IsNull() {
		d.Set("content", raw)
	} else {
		d.Set("content", json.RawMessage("null"))
	}
	if len(row.ToolCalls) > 0 {
		d.Set("tool_calls", row.ToolCalls)
	}
	if row.ToolCallID.Valid {
		d.SetString("tool_call_id", row.ToolCallID.V)
	}
	if row.ToolName.Valid {
		d.SetString("tool_name", row.ToolName.V)
	}
	if row.FinishReason.Valid {
		d.SetString("finish_reason", row.FinishReason.V)
	}
	if row.Reasoning.Valid {
		d.SetString("reasoning", row.Reasoning.V)
	}
	if row.ReasoningContent.Valid {
		d.SetString("reasoning_content", row.ReasoningContent.V)
	}
	if len(row.ReasoningDetails) > 0 {
		d.Set("reasoning_details", row.ReasoningDetails)
	}
	if len(row.CodexReasoningItems) > 0 {
		d.Set("codex_reasoning_items", row.CodexReasoningItems)
	}
	if len(row.CodexMessageItems) > 0 {
		d.Set("codex_message_items", row.CodexMessageItems)
	}
	if len(row.DataUse) > 0 {
		d.Set(dataUseKey, wire.Strings(row.DataUse))
	}
	var w wire.Message
	if err := w.UnmarshalJSON(d.JSON()); err != nil {
		return nil, fmt.Errorf("agent: stored %s message does not decode: %w", row.Role, err)
	}
	m := &Message{Message: w, Row: cloneRow(row)}
	if row.APIContent.Valid {
		v := row.APIContent.V
		m.APIContent = &v
	}
	meta := metadataValue(row.DisplayMetadata)
	if text, ok := meta.Get(withheldPromptKey).Str(); ok {
		m.Withheld = text
	}
	m.InflightRestatement = meta.Get(inflightRestatementKey).Truthy()
	return m, nil
}

func rawColumn(cell sql.Null[string]) json.RawMessage {
	if !cell.Valid || strings.TrimSpace(cell.V) == "" || !json.Valid([]byte(cell.V)) {
		return nil
	}
	return json.RawMessage(cell.V)
}

func FromRow(r state.MessageRow) (*Message, error) {
	row := state.Message{
		Role:                r.Role,
		Content:             r.Content,
		ToolCallID:          r.ToolCallID,
		ToolCalls:           slices.Clone(r.ToolCalls),
		ToolName:            r.ToolName,
		EffectDisposition:   r.EffectDisposition,
		Timestamp:           r.Timestamp,
		TokenCount:          r.TokenCount,
		FinishReason:        r.FinishReason,
		Reasoning:           r.Reasoning,
		ReasoningContent:    r.ReasoningContent,
		ReasoningDetails:    rawColumn(r.ReasoningDetails),
		CodexReasoningItems: rawColumn(r.CodexReasoningItems),
		CodexMessageItems:   rawColumn(r.CodexMessageItems),
		PlatformMessageID:   r.PlatformMessageID,
		Observed:            r.Observed,
		CompressedSummary:   r.CompressedSummary,
		APIContent:          r.APIContent,
		DisplayKind:         r.DisplayKind,
		DisplayMetadata:     slices.Clone(r.DisplayMetadata),
		RowID:               r.ID,
		Persisted:           true,
	}
	if r.MessageUID.Valid {
		row.UID = r.MessageUID.V
	}
	if r.AbsorbedMessageUIDs.Valid {
		row.AbsorbedUIDs = state.DecodeUIDList(r.AbsorbedMessageUIDs.V)
	}
	if r.ToolCallUIDs.Valid {
		row.ToolCallUIDs = state.DecodeToolCallUIDs(r.ToolCallUIDs.V)
	}
	if r.ToolCallUID.Valid {
		row.ToolCallUID = r.ToolCallUID.V
	}
	for _, item := range metadataValue(r.DisplayMetadata).Get(dataUseKey).Elems() {
		if s, ok := item.Str(); ok {
			row.DataUse = append(row.DataUse, s)
		}
	}
	return Decode(&row)
}

type memberSet []wire.Member

func (s memberSet) raw(key string) (json.RawMessage, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].Key == key {
			return s[i].Value, true
		}
	}
	return nil, false
}

func (s memberSet) text(key string) sql.Null[string] {
	raw, ok := s.raw(key)
	if !ok {
		return sql.Null[string]{}
	}
	v, err := wire.ParseValue(raw)
	if err != nil {
		return sql.Null[string]{}
	}
	text, isText := v.Str()
	return nullText(text, isText)
}

func (s memberSet) list(key string) json.RawMessage {
	raw, ok := s.raw(key)
	if !ok {
		return nil
	}
	v, err := wire.ParseValue(raw)
	if err != nil || !v.IsArray() {
		return nil
	}
	return slices.Clone(raw)
}

func (s memberSet) content() state.Content {
	raw, ok := s.raw("content")
	if !ok {
		return state.Content{}
	}
	v, err := wire.ParseValue(raw)
	if err != nil || v.Kind() == wire.KindNull {
		return state.Content{}
	}
	if text, isText := v.Str(); isText {
		return state.TextContent(text)
	}
	return state.JSONContent(slices.Clone(raw))
}

func contentList(c state.Content) ([]wire.Value, bool) {
	raw, ok := c.JSON()
	if !ok || c.IsNull() {
		return nil, false
	}
	if _, isText := c.Text(); isText {
		return nil, false
	}
	v, err := wire.ParseValue(raw)
	if err != nil || !v.IsArray() {
		return nil, false
	}
	return v.Elems(), true
}

func imageMarker(part wire.Value) string {
	detail := part.Get("detail")
	if !detail.Truthy() {
		if url := part.Get("image_url"); url.IsObject() {
			detail = url.Get("detail")
		}
	}
	switch detail.Text() {
	case "low", "high", "auto":
		return "[screenshot detail=" + detail.Text() + "]"
	}
	return "[screenshot]"
}

func durableContent(c state.Content) state.Content {
	parts, ok := contentList(c)
	if !ok {
		return c
	}
	var texts []string
	for _, p := range parts {
		if !p.IsObject() {
			continue
		}
		switch p.Get("type").Text() {
		case "text":
			text := p.Get("text")
			if text.Exists() {
				texts = append(texts, text.Display())
			} else {
				texts = append(texts, "")
			}
		case "image", "image_url", "input_image":
			texts = append(texts, imageMarker(p))
		}
	}
	if len(texts) == 0 {
		return state.Content{}
	}
	return state.TextContent(strings.Join(texts, "\n"))
}

func durableToolResult(c state.Content, summary string) state.Content {
	parts, ok := contentList(c)
	if !ok {
		return c
	}
	if summary != "" {
		return state.TextContent(summary)
	}
	var texts []string
	for _, p := range parts {
		if p.IsObject() && p.Get("type").Text() == "text" {
			text := p.Get("text")
			if text.Exists() {
				texts = append(texts, text.Display())
			} else {
				texts = append(texts, "")
			}
		}
	}
	if len(texts) == 0 {
		return state.TextContent(multimodalResultSummary)
	}
	return state.TextContent(strings.Join(texts, "\n"))
}

func textSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func trimText(s string) string {
	return strings.TrimFunc(s, textSpace)
}

func overrideContent(o *PersistOverride) (state.Content, bool) {
	if o == nil || o.Content == nil {
		return state.Content{}, false
	}
	return memberSet(o.Content.Members()).content(), true
}

func (m *Message) turnUserContent(content state.Content, api *string, o *PersistOverride) (state.Content, *string) {
	override, ok := overrideContent(o)
	if !ok || m.Row.CompressedSummary {
		return content, api
	}
	_, contentIsList := contentList(content)
	_, overrideIsList := contentList(override)
	if contentIsList && !overrideIsList {
		return content, api
	}
	text, isText := content.Text()
	overrideText, overrideIsText := override.Text()
	if api == nil && isText && !(overrideIsText && overrideText == text) {
		sent := text
		api = &sent
	}
	if isText && overrideIsText && m.MergedPrefix != "" {
		if text == m.MergedPrefix || strings.HasPrefix(text, m.MergedPrefix+"\n\n") {
			if overrideText != "" {
				return state.TextContent(m.MergedPrefix + "\n\n" + overrideText), api
			}
			return state.TextContent(m.MergedPrefix), api
		}
	}
	return override, api
}

func (m *Message) Encode() *state.Message {
	return m.encode(nil)
}

func (m *Message) EncodeTurnUser(o *PersistOverride) *state.Message {
	return m.encode(o)
}

func (m *Message) encode(o *PersistOverride) *state.Message {
	members := memberSet(m.Message.Members())
	row := &m.Row
	role := string(m.Role)
	if role == "" {
		role = "unknown"
	}
	live := members.content()
	content := live
	var api *string
	if m.APIContent != nil {
		sent := *m.APIContent
		api = &sent
	}
	if o != nil && role == "user" {
		content, api = m.turnUserContent(content, api, o)
		if o.Timestamp != nil {
			row.Timestamp = sql.Null[float64]{V: *o.Timestamp, Valid: true}
		}
	}
	text, isText := content.Text()
	if api != nil && isText && *api == text {
		api = nil
	}
	if api == nil && (role == "user" || role == "assistant") && isText && text != "" &&
		trimText(state.SanitizeContext(text)) != trimText(text) {
		sent := text
		api = &sent
	}

	row.Role = role
	if role == "tool" {
		row.Content = durableToolResult(content, m.Summary)
	} else {
		row.Content = durableContent(content)
	}
	toolName := members.text("tool_name")
	if !toolName.Valid || toolName.V == "" {
		toolName = sql.Null[string]{}
		if name := members.text("name"); role == "tool" && name.Valid {
			toolName = name
		}
	}
	row.ToolName = toolName
	row.ToolCalls = members.list("tool_calls")
	row.ToolCallID = members.text("tool_call_id")
	row.FinishReason = members.text("finish_reason")
	row.Reasoning = members.text("reasoning")
	row.ReasoningContent = members.text("reasoning_content")
	row.ReasoningDetails = members.list("reasoning_details")
	row.CodexReasoningItems = members.list("codex_reasoning_items")
	row.CodexMessageItems = members.list("codex_message_items")
	if api != nil {
		row.APIContent = nullText(*api, true)
	} else {
		row.APIContent = sql.Null[string]{}
	}

	kind := row.DisplayKind
	if m.hiddenBySummary && kind.Valid && kind.V == hiddenDisplayKind {
		kind = m.kindBeforeSummary
	}
	if m.summaryHidden(role, live, kind) {
		m.kindBeforeSummary, m.hiddenBySummary = kind, true
		row.DisplayKind = nullText(hiddenDisplayKind, true)
	} else {
		m.kindBeforeSummary, m.hiddenBySummary = sql.Null[string]{}, false
		row.DisplayKind = kind
	}

	if m.Withheld != "" {
		row.DisplayMetadata = metadataWith(row.DisplayMetadata, withheldPromptKey, wire.Quote(m.Withheld))
	}
	if m.InflightRestatement {
		row.DisplayMetadata = metadataWith(row.DisplayMetadata, inflightRestatementKey, wire.Bool(true))
	}
	row.DataUse = slices.Clone(m.DataUse)
	return row
}

func (m *Message) summaryHidden(role string, live state.Content, kind sql.Null[string]) bool {
	if !m.Row.CompressedSummary {
		return false
	}
	if role == "user" && userTurnLive(live, kind.V, true) {
		return false
	}
	return summaryClass(live) == summaryStandalone || m.SummaryHasUserTurn == nil || !*m.SummaryHasUserTurn
}

func (m *Message) Clone() *Message {
	if m == nil {
		return nil
	}
	out := *m
	out.Message = m.Message.Clone()
	out.Row = cloneRow(&m.Row)
	if m.APIContent != nil {
		v := *m.APIContent
		out.APIContent = &v
	}
	if m.Risk != nil {
		r := *m.Risk
		r.Findings = slices.Clone(m.Risk.Findings)
		out.Risk = &r
	}
	if m.SummaryHasUserTurn != nil {
		v := *m.SummaryHasUserTurn
		out.SummaryHasUserTurn = &v
	}
	return &out
}

func (m *Message) Fresh() *Message {
	out := m.Clone()
	if out == nil {
		return nil
	}
	out.Row.Persisted = false
	out.Row.RowID = 0
	out.Row.Snapshot = ""
	out.Row.Canonical = nil
	return out
}

func Wire(msgs []*Message) []wire.Message {
	out := make([]wire.Message, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		w := m.Message.Clone()
		if m.APIContent != nil && *m.APIContent != "" && (w.Role == wire.RoleUser || w.Role == wire.RoleAssistant) {
			w.Text, w.Parts = *m.APIContent, nil
		}
		out = append(out, w)
	}
	return out
}

func (m *Message) DropAPIContent() {
	m.APIContent = nil
}

func (m *Message) CommittedRowID() (int64, bool) {
	if m == nil || !m.Row.Persisted || m.Row.RowID <= 0 {
		return 0, false
	}
	return m.Row.RowID, true
}

func (m *Message) hasToolCalls() bool {
	return m != nil && len(m.ToolCalls) > 0
}
