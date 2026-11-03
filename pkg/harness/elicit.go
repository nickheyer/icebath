package harness

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
)

// Answers an elicitation of an MCP server, hooks first, then the person through the question card
func (a *App) elicit(ctx context.Context, server string, req mcp.Elicitation) mcp.ElicitationResult {
	fields := map[string]any{"mcp_server_name": server, "message": req.Message, "mode": req.Mode}
	if req.ID != "" {
		fields["elicitation_id"] = req.ID
	}
	if len(req.Schema) > 0 {
		fields["requested_schema"] = json.RawMessage(req.Schema)
	}
	if req.URL != "" {
		fields["url"] = req.URL
	}
	v, err := a.FireSession(ctx, a.Session, agent.Elicitation, server, fields)
	if err != nil || v.Block {
		return mcp.ElicitationResult{Action: "decline"}
	}
	result := mcp.ElicitationResult{}
	switch v.Action {
	case "accept", "decline", "cancel":
		result = mcp.ElicitationResult{Action: v.Action, Content: v.Content}
	default:
		result = a.askElicitation(ctx, server, req)
	}
	fields["action"] = result.Action
	if len(result.Content) > 0 {
		fields["content"] = result.Content
	}
	after, err := a.FireSession(ctx, a.Session, agent.ElicitationResult, server, fields)
	if err == nil {
		if after.Block {
			return mcp.ElicitationResult{Action: "decline"}
		}
		if after.Action == "accept" || after.Action == "decline" || after.Action == "cancel" {
			result.Action = after.Action
			if after.Content != nil {
				result.Content = after.Content
			}
		}
	}
	return result
}

// One property of an elicitation schema as the question card asks it
type schemaProperty struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Enum        []string `json:"enum"`
	EnumNames   []string `json:"enumNames"`
	Default     any      `json:"default"`
}

// Puts the elicitation to the person as questions, one per property, and reads their answers into the content
func (a *App) askElicitation(ctx context.Context, server string, req mcp.Elicitation) mcp.ElicitationResult {
	if req.Mode == "url" || req.URL != "" {
		q := Question{Text: req.Message + "\n\nOpen " + req.URL + " and come back, then confirm", Header: server, Choices: []Choice{{Label: "done", Description: "the visit is complete"}, {Label: "decline", Description: "do not proceed"}}}
		answers, err := a.Questions.Ask(ctx, "", a.Session.ID(), []Question{q})
		if err != nil || len(answers) == 0 || !answers[0].Given() || contains(answers[0].Picked, "decline") {
			return mcp.ElicitationResult{Action: "decline"}
		}
		return mcp.ElicitationResult{Action: "accept"}
	}
	var schema struct {
		Properties map[string]schemaProperty `json:"properties"`
		Required   []string                  `json:"required"`
	}
	json.Unmarshal(req.Schema, &schema)
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sortStrings(names)
	var questions []Question
	for _, name := range names {
		p := schema.Properties[name]
		text := p.Title
		if text == "" {
			text = name
		}
		if p.Description != "" {
			text += ": " + p.Description
		}
		q := Question{Text: text, Header: server}
		switch {
		case len(p.Enum) > 0:
			for i, e := range p.Enum {
				label := e
				if i < len(p.EnumNames) {
					label = p.EnumNames[i]
				}
				q.Choices = append(q.Choices, Choice{Label: label, Description: e})
			}
		case p.Type == "boolean":
			q.Choices = []Choice{{Label: "yes", Description: "true"}, {Label: "no", Description: "false"}}
		}
		questions = append(questions, q)
	}
	if len(questions) == 0 {
		questions = []Question{{Text: req.Message, Header: server, Choices: []Choice{{Label: "accept", Description: "proceed"}, {Label: "decline", Description: "do not proceed"}}}}
		answers, err := a.Questions.Ask(ctx, "", a.Session.ID(), questions)
		if err != nil || len(answers) == 0 || !answers[0].Given() || contains(answers[0].Picked, "decline") {
			return mcp.ElicitationResult{Action: "decline"}
		}
		return mcp.ElicitationResult{Action: "accept", Content: map[string]any{}}
	}
	questions[0].Text = req.Message + "\n\n" + questions[0].Text
	answers, err := a.Questions.Ask(ctx, "", a.Session.ID(), questions)
	if err != nil {
		return mcp.ElicitationResult{Action: "cancel"}
	}
	content := map[string]any{}
	for i, name := range names {
		if i >= len(answers) {
			break
		}
		ans := answers[i]
		p := schema.Properties[name]
		if !ans.Given() {
			if p.Default != nil {
				content[name] = p.Default
			}
			continue
		}
		content[name] = elicitValue(p, ans)
	}
	for _, name := range schema.Required {
		if _, ok := content[name]; !ok {
			return mcp.ElicitationResult{Action: "decline"}
		}
	}
	return mcp.ElicitationResult{Action: "accept", Content: content}
}

// Value of one answer in the type the schema asks for
func elicitValue(p schemaProperty, ans Answer) any {
	text := strings.TrimSpace(ans.Text)
	if len(ans.Picked) > 0 {
		text = ans.Picked[0]
		for i, label := range p.EnumNames {
			if label == text && i < len(p.Enum) {
				text = p.Enum[i]
			}
		}
	}
	switch p.Type {
	case "boolean":
		return text == "yes" || text == "true"
	case "integer":
		if n, err := strconv.Atoi(text); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return f
		}
	}
	return text
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
