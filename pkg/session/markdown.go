package session

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Controls which parts of a transcript the Markdown export includes
type MarkdownOptions struct {
	// Omits thinking blocks
	NoThinking bool
	// Omits image and document placeholders
	NoAttachments bool
	// Adds the time and producing agent under each heading
	Timestamps bool
	// Appends the record log after the transcript
	Records bool
}

// Renders the full transcript as Markdown with a heading per turn
func (s *Session) Markdown() string { return s.MarkdownWith(MarkdownOptions{}) }

// Renders the transcript as Markdown honoring the options
func (s *Session) MarkdownWith(o MarkdownOptions) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sb strings.Builder
	title := s.title
	if title == "" {
		title = s.id
	}
	fmt.Fprintf(&sb, "# %s\n\n", title)
	fmt.Fprintf(&sb, "- id: %s\n- created: %s\n", s.id, s.created.Format("2006-01-02 15:04:05 UTC"))
	if s.model != "" {
		fmt.Fprintf(&sb, "- model: %s\n", s.model)
	}
	if s.parent != "" {
		fmt.Fprintf(&sb, "- forked from: %s\n", s.parent)
	}
	fmt.Fprintf(&sb, "- usage: %d in, %d out, %d cache read, %d cache write, %d reasoning\n", s.usage.Input, s.usage.Output, s.usage.CacheRead, s.usage.CacheWrite, s.usage.Reasoning)
	for _, e := range s.entries {
		fmt.Fprintf(&sb, "\n## %s\n\n", heading(e.Role))
		if o.Timestamps {
			meta := e.Time.Format("2006-01-02 15:04:05 UTC")
			if e.Agent != "" {
				meta += " by " + e.Agent
			}
			if e.Model != "" {
				meta += " on " + e.Model
			}
			fmt.Fprintf(&sb, "_%s_\n\n", meta)
		}
		for _, p := range e.Content {
			writePart(&sb, p, o)
		}
	}
	if o.Records && len(s.records) > 0 {
		sb.WriteString("\n## Records\n\n")
		for _, r := range s.records {
			fmt.Fprintf(&sb, "- %s %s", r.Time.Format("15:04:05"), r.Kind)
			if r.Agent != "" {
				fmt.Fprintf(&sb, " (%s)", r.Agent)
			}
			if len(r.Data) > 0 {
				fmt.Fprintf(&sb, " `%s`", string(r.Data))
			}
			sb.WriteString("\n")
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

func writePart(sb *strings.Builder, p protocol.Part, o MarkdownOptions) {
	switch v := p.(type) {
	case protocol.Text:
		sb.WriteString(strings.TrimRight(v.Text, "\n") + "\n\n")
		for _, c := range v.Citations {
			label := c.Title
			if label == "" {
				label = c.URL
			}
			if label == "" {
				label = c.Document
			}
			if c.URL != "" {
				fmt.Fprintf(sb, "- [%s](%s)\n", label, c.URL)
			} else {
				fmt.Fprintf(sb, "- %s\n", label)
			}
		}
		if len(v.Citations) > 0 {
			sb.WriteString("\n")
		}
	case protocol.Image:
		if !o.NoAttachments {
			fmt.Fprintf(sb, "[image %s, %d bytes]\n\n", v.MediaType, len(v.Data))
		}
	case protocol.Document:
		if !o.NoAttachments {
			name := v.Name
			if name == "" {
				name = "document"
			}
			fmt.Fprintf(sb, "[%s %s, %d bytes]\n\n", name, v.MediaType, len(v.Data))
		}
	case protocol.Thinking:
		if o.NoThinking {
			return
		}
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
	case protocol.ServerToolCall:
		fmt.Fprintf(sb, "**Server call %s** `%s`\n\n```json\n%s\n```\n\n", v.ID, v.Name, string(v.Args))
	case protocol.ServerToolResult:
		fmt.Fprintf(sb, "**Server result %s** `%s`\n\n```json\n%s\n```\n\n", v.CallID, v.Name, string(v.Content))
	case protocol.ToolResult:
		status := ""
		if v.IsError {
			status = " (error)"
		}
		fmt.Fprintf(sb, "**Result %s** `%s`%s\n\n", v.CallID, v.Name, status)
		for _, inner := range v.Content {
			writePart(sb, inner, o)
		}
	}
}
