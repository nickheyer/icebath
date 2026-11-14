package qqbot

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	ApprovalButtonPrefix = "approve:"
	UpdatePromptPrefix   = "update_prompt:"
)

// Compiled Python pattern, panicking at init on a malformed constant
func mustPyRe(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

var (
	approvalDataRe = mustPyRe(`^approve:(.+):(allow-once|allow-always|deny)$`)
	updatePromptRe = mustPyRe(`^update_prompt:(y|n)$`)
)

type KeyboardButtonPermission struct{ Type int }

type KeyboardButtonAction struct {
	Type       int
	Data       string
	Permission KeyboardButtonPermission
	ClickLimit int
}

type KeyboardButtonRenderData struct {
	Label        string
	VisitedLabel string
	Style        int
}

type KeyboardButton struct {
	ID         string
	RenderData KeyboardButtonRenderData
	Action     KeyboardButtonAction
	GroupID    string
}

type KeyboardRow struct{ Buttons []KeyboardButton }

type KeyboardContent struct{ Rows []KeyboardRow }

type InlineKeyboard struct{ Content KeyboardContent }

func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// ToDict serializes the permission in field declaration order
func (p KeyboardButtonPermission) ToDict() *pyjson.Object { return obj("type", int64(p.Type)) }

// ToDict serializes the action in field declaration order
func (a KeyboardButtonAction) ToDict() *pyjson.Object {
	return obj("type", int64(a.Type), "data", a.Data, "permission", a.Permission.ToDict(), "click_limit", int64(a.ClickLimit))
}

// ToDict serializes the render data in field declaration order
func (r KeyboardButtonRenderData) ToDict() *pyjson.Object {
	return obj("label", r.Label, "visited_label", r.VisitedLabel, "style", int64(r.Style))
}

// ToDict serializes the button in field declaration order
func (b KeyboardButton) ToDict() *pyjson.Object {
	return obj("id", b.ID, "render_data", b.RenderData.ToDict(), "action", b.Action.ToDict(), "group_id", b.GroupID)
}

// ToDict serializes the row in field declaration order
func (r KeyboardRow) ToDict() *pyjson.Object {
	buttons := make([]any, len(r.Buttons))
	for i, b := range r.Buttons {
		buttons[i] = b.ToDict()
	}
	return obj("buttons", buttons)
}

// ToDict serializes the content in field declaration order
func (c KeyboardContent) ToDict() *pyjson.Object {
	rows := make([]any, len(c.Rows))
	for i, r := range c.Rows {
		rows[i] = r.ToDict()
	}
	return obj("rows", rows)
}

// ToDict serializes the keyboard payload for MessageToCreate.keyboard
func (k InlineKeyboard) ToDict() *pyjson.Object { return obj("content", k.Content.ToDict()) }

// ParseApprovalButtonData splits approve data into session key and decision, ok false when it does not match
func ParseApprovalButtonData(buttonData string) (sessionKey, decision string, ok bool) {
	m, _ := approvalDataRe.FindStringMatch(buttonData)
	if m == nil {
		return "", "", false
	}
	return m.GroupByNumber(1).String(), m.GroupByNumber(2).String(), true
}

// ParseUpdatePromptButtonData returns y or n from update prompt data, ok false when it does not match
func ParseUpdatePromptButtonData(buttonData string) (string, bool) {
	m, _ := updatePromptRe.FindStringMatch(buttonData)
	if m == nil {
		return "", false
	}
	return m.GroupByNumber(1).String(), true
}

type buttonSpec struct {
	id, label, visited, data string
	style                    int
}

// One row of callback buttons sharing a group so a click greys the rest
func singleRowKeyboard(groupID string, buttons ...buttonSpec) InlineKeyboard {
	row := KeyboardRow{}
	for _, b := range buttons {
		row.Buttons = append(row.Buttons, KeyboardButton{
			ID: b.id, GroupID: groupID,
			Action:     KeyboardButtonAction{Type: 1, Data: b.data, Permission: KeyboardButtonPermission{Type: 2}, ClickLimit: 1},
			RenderData: KeyboardButtonRenderData{Label: b.label, VisitedLabel: b.visited, Style: b.style},
		})
	}
	return InlineKeyboard{Content: KeyboardContent{Rows: []KeyboardRow{row}}}
}

// BuildApprovalKeyboard builds allow once, optional always allow and deny buttons carrying the session key
func BuildApprovalKeyboard(sessionKey string, allowPermanent bool) InlineKeyboard {
	prefix := ApprovalButtonPrefix + sessionKey
	buttons := []buttonSpec{{"allow", "✅ 允许一次", "已允许", prefix + ":allow-once", 1}}
	if allowPermanent {
		buttons = append(buttons, buttonSpec{"always", "⭐ 始终允许", "已始终允许", prefix + ":allow-always", 1})
	}
	buttons = append(buttons, buttonSpec{"deny", "❌ 拒绝", "已拒绝", prefix + ":deny", 0})
	return singleRowKeyboard("approval", buttons...)
}

// BuildUpdatePromptKeyboard builds the confirm and cancel buttons of an update prompt
func BuildUpdatePromptKeyboard() InlineKeyboard {
	return singleRowKeyboard("update_prompt",
		buttonSpec{"yes", "✓ 确认", "已确认", UpdatePromptPrefix + "y", 1},
		buttonSpec{"no", "✗ 取消", "已取消", UpdatePromptPrefix + "n", 0})
}

type ApprovalRequest struct {
	SessionKey     string
	Title          string
	Description    string
	CommandPreview string
	Cwd            string
	ToolName       string
	Severity       string
	TimeoutSec     int
	AllowPermanent bool
}

// NewApprovalRequest fills the dataclass defaults of a 120 second timeout and permanent scope allowed
func NewApprovalRequest(sessionKey, title string) ApprovalRequest {
	return ApprovalRequest{SessionKey: sessionKey, Title: title, TimeoutSec: 120, AllowPermanent: true}
}

var severityIcons = map[string]string{"critical": "🔴", "info": "🔵"}

// BuildApprovalText renders the markdown body of an exec or plugin approval request
func BuildApprovalText(req ApprovalRequest) string {
	var lines []string
	if req.CommandPreview != "" || req.Cwd != "" {
		lines = []string{"🔐 **命令执行审批**", ""}
		if req.CommandPreview != "" {
			preview := []rune(req.CommandPreview)
			lines = append(lines, "```\n"+string(preview[:min(300, len(preview))])+"\n```")
		}
		if req.Cwd != "" {
			lines = append(lines, "📁 目录: "+req.Cwd)
		}
		if req.Title != "" && req.Title != req.CommandPreview {
			lines = append(lines, "📋 "+req.Title)
		}
		if req.Description != "" {
			lines = append(lines, "📝 "+req.Description)
		}
	} else {
		icon, ok := severityIcons[req.Severity]
		if !ok {
			icon = "🟡"
		}
		lines = []string{icon + " **审批请求**", "", "📋 " + req.Title}
		if req.Description != "" {
			lines = append(lines, "📝 "+req.Description)
		}
		if req.ToolName != "" {
			lines = append(lines, "🔧 工具: "+req.ToolName)
		}
	}
	lines = append(lines, "", fmt.Sprintf("⏱️ 超时: %d 秒", req.TimeoutSec))
	return strings.Join(lines, "\n")
}

type InteractionEvent struct {
	ID                string
	Type              int64
	ChatType          int64
	Scene             string
	GroupOpenID       string
	GroupMemberOpenID string
	UserOpenID        string
	ChannelID         string
	GuildID           string
	ButtonData        string
	ButtonID          string
	ResolverUserID    string
}

// OperatorOpenID is the group member, then the c2c user, then the resolver user id
func (e InteractionEvent) OperatorOpenID() string {
	for _, v := range []string{e.GroupMemberOpenID, e.UserOpenID, e.ResolverUserID} {
		if v != "" {
			return v
		}
	}
	return ""
}

var sceneNames = map[int64]string{0: "guild", 1: "group", 2: "c2c"}

// Dict view over a decoded JSON object or a Go map, ok false for any other value
func asDict(v any) (map[string]any, bool) {
	switch d := v.(type) {
	case *pyjson.Object:
		return d.Vals, true
	case map[string]any:
		return d, true
	}
	return nil, false
}

func getOr(d map[string]any, key string, def any) any {
	if v, ok := d[key]; ok {
		return v
	}
	return def
}

func orDefault(v, def any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return def
}

// Python int of a value, failing like int() on text or types it rejects
func pyInt64(v any) (int64, error) {
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("Python int too large to convert to C ssize_t")
	}
	return n.Int64(), nil
}

func notDictErr(v any) error {
	return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
}

// ParseInteractionEvent reads the d payload of an INTERACTION_CREATE dispatch
func ParseInteractionEvent(rawValue any) (InteractionEvent, error) {
	raw, ok := asDict(rawValue)
	if !ok {
		return InteractionEvent{}, notDictErr(rawValue)
	}
	dataValue := orDefault(raw["data"], map[string]any{})
	dataRaw, ok := asDict(dataValue)
	if !ok {
		return InteractionEvent{}, notDictErr(dataValue)
	}
	resolvedValue := orDefault(dataRaw["resolved"], map[string]any{})
	sceneCode, err := pyInt64(orDefault(getOr(raw, "chat_type", int64(0)), int64(0)))
	if err != nil {
		return InteractionEvent{}, err
	}
	eventType, err := pyInt64(orDefault(getOr(dataRaw, "type", int64(0)), int64(0)))
	if err != nil {
		return InteractionEvent{}, err
	}
	resolved, ok := asDict(resolvedValue)
	if !ok {
		return InteractionEvent{}, notDictErr(resolvedValue)
	}
	str := func(d map[string]any, key string) string { return pyrepr.Str(getOr(d, key, "")) }
	return InteractionEvent{
		ID: str(raw, "id"), Type: eventType, ChatType: sceneCode, Scene: sceneNames[sceneCode],
		GroupOpenID: str(raw, "group_openid"), GroupMemberOpenID: str(raw, "group_member_openid"),
		UserOpenID: str(raw, "user_openid"), ChannelID: str(raw, "channel_id"), GuildID: str(raw, "guild_id"),
		ButtonData: str(resolved, "button_data"), ButtonID: str(resolved, "button_id"), ResolverUserID: str(resolved, "user_id"),
	}, nil
}
