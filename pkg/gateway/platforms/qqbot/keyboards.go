package qqbot

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	approvalButtonPrefix = "approve:"
	updatePromptPrefix   = "update_prompt:"
)

var (
	approvalDataRE = regexp.MustCompile(`^approve:(.+):(allow-once|allow-always|deny)$`)
	updatePromptRE = regexp.MustCompile(`^update_prompt:(y|n)$`)
)

type keyboardPermission struct {
	Type int `json:"type"`
}

type keyboardAction struct {
	Type       int                `json:"type"`
	Data       string             `json:"data"`
	Permission keyboardPermission `json:"permission"`
	ClickLimit int                `json:"click_limit"`
}

type keyboardRenderData struct {
	Label        string `json:"label"`
	VisitedLabel string `json:"visited_label"`
	Style        int    `json:"style"`
}

type keyboardButton struct {
	ID         string             `json:"id"`
	RenderData keyboardRenderData `json:"render_data"`
	Action     keyboardAction     `json:"action"`
	GroupID    string             `json:"group_id"`
}

type keyboardRow struct {
	Buttons []keyboardButton `json:"buttons"`
}

type keyboardContent struct {
	Rows []keyboardRow `json:"rows"`
}

// Inline keyboard payload of a QQ message body
type inlineKeyboard struct {
	Content keyboardContent `json:"content"`
}

type buttonSpec struct {
	id, label, visited, data string
	style                    int
}

// One row of single use callback buttons sharing a group so a click greys the rest
func singleRowKeyboard(groupID string, buttons ...buttonSpec) *inlineKeyboard {
	row := keyboardRow{}
	for _, b := range buttons {
		row.Buttons = append(row.Buttons, keyboardButton{
			ID: b.id, GroupID: groupID,
			Action:     keyboardAction{Type: 1, Data: b.data, Permission: keyboardPermission{Type: 2}, ClickLimit: 1},
			RenderData: keyboardRenderData{Label: b.label, VisitedLabel: b.visited, Style: b.style},
		})
	}
	return &inlineKeyboard{Content: keyboardContent{Rows: []keyboardRow{row}}}
}

// Allow once, optional always allow and deny buttons carrying the session key
func buildApprovalKeyboard(sessionKey string, allowPermanent bool) *inlineKeyboard {
	prefix := approvalButtonPrefix + sessionKey
	buttons := []buttonSpec{{"allow", "✅ 允许一次", "已允许", prefix + ":allow-once", 1}}
	if allowPermanent {
		buttons = append(buttons, buttonSpec{"always", "⭐ 始终允许", "已始终允许", prefix + ":allow-always", 1})
	}
	buttons = append(buttons, buttonSpec{"deny", "❌ 拒绝", "已拒绝", prefix + ":deny", 0})
	return singleRowKeyboard("approval", buttons...)
}

// Confirm and cancel buttons of an update prompt
func buildUpdatePromptKeyboard() *inlineKeyboard {
	return singleRowKeyboard("update_prompt",
		buttonSpec{"yes", "✓ 确认", "已确认", updatePromptPrefix + "y", 1},
		buttonSpec{"no", "✗ 取消", "已取消", updatePromptPrefix + "n", 0})
}

// Session key and decision of approval button data, false when it does not match
func parseApprovalButtonData(data string) (string, string, bool) {
	m := approvalDataRE.FindStringSubmatch(data)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Answer y or n of update prompt button data, false when it does not match
func parseUpdatePromptButtonData(data string) (string, bool) {
	m := updatePromptRE.FindStringSubmatch(data)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Display data of one exec approval card
type approvalRequest struct {
	sessionKey     string
	title          string
	description    string
	commandPreview string
	timeoutSec     int
	allowPermanent bool
}

// Markdown body of an approval card, the command layout when a command is shown
func buildApprovalText(req approvalRequest) string {
	var lines []string
	if req.commandPreview != "" {
		lines = []string{"🔐 **命令执行审批**", "", "```\n" + cutRunes(req.commandPreview, 300) + "\n```"}
		if req.title != "" && req.title != req.commandPreview {
			lines = append(lines, "📋 "+req.title)
		}
	} else {
		lines = []string{"🟡 **审批请求**", "", "📋 " + req.title}
	}
	if req.description != "" {
		lines = append(lines, "📝 "+req.description)
	}
	lines = append(lines, "", fmt.Sprintf("⏱️ 超时: %d 秒", req.timeoutSec))
	return strings.Join(lines, "\n")
}

// Parsed INTERACTION_CREATE dispatch
type interactionEvent struct {
	id                string
	scene             string
	groupOpenID       string
	groupMemberOpenID string
	userOpenID        string
	guildID           string
	buttonData        string
	resolverUserID    string
}

// Group member, then c2c user, then resolver user who clicked
func (e interactionEvent) operatorOpenID() string {
	for _, v := range []string{e.groupMemberOpenID, e.userOpenID, e.resolverUserID} {
		if v != "" {
			return v
		}
	}
	return ""
}

var sceneNames = map[int64]string{0: "guild", 1: "group", 2: "c2c"}

// Reads the d payload of an INTERACTION_CREATE dispatch
func parseInteractionEvent(raw map[string]any) interactionEvent {
	resolved := jmap(jmap(raw["data"])["resolved"])
	code, _ := jint(raw["chat_type"])
	return interactionEvent{
		id: jstr(raw["id"]), scene: sceneNames[code],
		groupOpenID: jstr(raw["group_openid"]), groupMemberOpenID: jstr(raw["group_member_openid"]),
		userOpenID: jstr(raw["user_openid"]), guildID: jstr(raw["guild_id"]),
		buttonData: jstr(resolved["button_data"]), resolverUserID: jstr(resolved["user_id"]),
	}
}
