package session

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Renders the transcript as Markdown with a heading per turn
func (s *Session) Markdown() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sb strings.Builder
	title := s.Title
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&sb, "# %s\n\n", title)
	fmt.Fprintf(&sb, "- id: %s\n- created: %s\n", s.ID, s.Created.Format("2006-01-02 15:04:05 UTC"))
	if s.Model != "" {
		fmt.Fprintf(&sb, "- model: %s\n", s.Model)
	}
	if s.Parent != "" {
		fmt.Fprintf(&sb, "- forked from: %s\n", s.Parent)
	}
	fmt.Fprintf(&sb, "- usage: %d in, %d out, %d cache read, %d cache write, %d reasoning\n", s.Usage.Input, s.Usage.Output, s.Usage.CacheRead, s.Usage.CacheWrite, s.Usage.Reasoning)
	for _, m := range s.Messages {
		fmt.Fprintf(&sb, "\n## %s\n\n", heading(m.Role))
		for _, p := range m.Content {
			writePart(&sb, p)
		}
	}
	return sb.String()
}

func heading(role protocol.Role) string {
	switch role {
	case protocol.RoleUser:
		return "User"
	case protocol.RoleAssistant:
		return "Assistant"
	case protocol.RoleTool:
		return "Tool results"
	}
	return string(role)
}

func writePart(sb *strings.Builder, p protocol.Part) {
	switch v := p.(type) {
	case protocol.Text:
		sb.WriteString(strings.TrimRight(v.Text, "\n") + "\n\n")
	case protocol.Image:
		fmt.Fprintf(sb, "[image %s, %d bytes]\n\n", v.MediaType, len(v.Data))
	case protocol.Thinking:
		if v.Redacted {
			sb.WriteString("> [redacted thinking]\n\n")
			return
		}
		for _, line := range strings.Split(strings.TrimRight(v.Text, "\n"), "\n") {
			sb.WriteString("> " + line + "\n")
		}
		sb.WriteString("\n")
	case protocol.ToolCall:
		fmt.Fprintf(sb, "**Call %s** `%s`\n\n```json\n%s\n```\n\n", v.ID, v.Name, string(v.Args))
	case protocol.ToolResult:
		status := ""
		if v.IsError {
			status = " (error)"
		}
		fmt.Fprintf(sb, "**Result %s** `%s`%s\n\n", v.CallID, v.Name, status)
		for _, inner := range v.Content {
			writePart(sb, inner)
		}
	}
}
