package provider

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Transcript label of each message role, unknown roles reading as Context
var acpRoleLabels = map[string]string{"system": "System", "user": "User", "assistant": "Assistant", "tool": "Tool", "context": "Context"}

// Opening lines of every prompt sent to an ACP agent backend
var acpPromptPreamble = []string{
	"You are being used as the active ACP agent backend for Hermes.",
	"Use ACP capabilities to complete tasks.",
	"IMPORTANT: If you take an action with a tool, you MUST output tool calls using <tool_call>{...}</tool_call> blocks with JSON exactly in OpenAI function-call shape.",
	"If no tool is needed, answer normally.",
}

// Prompt an ACP client sends for OpenAI chat messages, tools and tool choice
func ACPFormatMessagesAsPrompt(messages []any, tools []any, toolChoice any) (string, error) {
	return acpFormatMessagesAsPrompt(messages, tools, toolChoice)
}

// Single ACP prompt carrying the preamble, the forwarded tools, the transcript and the continue instruction
func acpFormatMessagesAsPrompt(messages []any, tools []any, toolChoice any) (string, error) {
	bridge, err := RenderToolBridgeSections(tools, toolChoice, nil)
	if err != nil {
		return "", err
	}
	sections := append(append([]string{}, acpPromptPreamble...), bridge...)
	var transcript []string
	for _, message := range messages {
		if !bridgeIsDict(message) {
			continue
		}
		roleRaw, _ := bridgeAttr(message, "role")
		role := "unknown"
		if pyrepr.Truthy(roleRaw) {
			role = pyrepr.Str(roleRaw)
		}
		role = pystr.Lower(pystr.Strip(role))
		content, _ := bridgeAttr(message, "content")
		rendered, err := acpRenderMessageContent(content)
		if err != nil {
			return "", err
		}
		if rendered != "" {
			label, ok := acpRoleLabels[role]
			if !ok {
				label = "Context"
			}
			transcript = append(transcript, label+":\n"+rendered)
		}
	}
	if len(transcript) > 0 {
		sections = append(sections, "Conversation transcript:\n\n"+strings.Join(transcript, "\n\n"))
	}
	sections = append(sections, "Continue the conversation from the latest user request.")
	kept := make([]string, 0, len(sections))
	for _, section := range sections {
		if stripped := pystr.Strip(section); stripped != "" {
			kept = append(kept, stripped)
		}
	}
	return strings.Join(kept, "\n\n"), nil
}

// Plain text of one message content, a text dict, a parts list or any other value
func acpRenderMessageContent(content any) (string, error) {
	if content == nil {
		return "", nil
	}
	if bridgeIsDict(content) {
		if text, ok := bridgeAttr(content, "text"); ok {
			if !pyrepr.Truthy(text) {
				return "", nil
			}
			return pystr.Strip(pyrepr.Str(text)), nil
		}
		if inner, _ := bridgeAttr(content, "content"); inner != nil {
			if s, ok := inner.(string); ok {
				return pystr.Strip(s), nil
			}
		}
		return pyjson.Dumps(content, pyjson.EnsureASCII(true))
	}
	if items, ok := bridgeList(content); ok {
		var parts []string
		for _, item := range items {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
				continue
			}
			if !bridgeIsDict(item) {
				continue
			}
			if text, ok := bridgeAttrOr(item, "text", nil).(string); ok && pystr.Strip(text) != "" {
				parts = append(parts, pystr.Strip(text))
			}
		}
		return pystr.Strip(strings.Join(parts, "\n")), nil
	}
	return pystr.Strip(pyrepr.Str(content)), nil
}
