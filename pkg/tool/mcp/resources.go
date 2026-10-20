package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Resource a server exposes for reading
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

// Parameterized resource address a server exposes
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// One piece of a resource as read
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     []byte `json:"blob,omitempty"`
}

// Prompt template a server offers
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// Argument a prompt template accepts
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Rendered prompt as messages ready for a request
type PromptResult struct {
	Description string
	Messages    []protocol.Message
}

// Lists every resource of the server, following pagination
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	if !c.server.Resources {
		return nil, fmt.Errorf("mcp: server %s offers no resources", c.name)
	}
	var out []Resource
	cursor := ""
	for {
		var page struct {
			Resources  []Resource `json:"resources"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.page(ctx, "resources/list", cursor, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Resources...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// Lists every resource template of the server, following pagination
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	if !c.server.Resources {
		return nil, fmt.Errorf("mcp: server %s offers no resources", c.name)
	}
	var out []ResourceTemplate
	cursor := ""
	for {
		var page struct {
			Templates  []ResourceTemplate `json:"resourceTemplates"`
			NextCursor string             `json:"nextCursor"`
		}
		if err := c.page(ctx, "resources/templates/list", cursor, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Templates...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func (c *Client) page(ctx context.Context, method, cursor string, v any) error {
	var params any
	if cursor != "" {
		params = map[string]string{"cursor": cursor}
	}
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("mcp: %s result: %w", method, err)
	}
	return nil
}

// Reads a resource by URI
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	raw, err := c.Call(ctx, "resources/read", map[string]string{"uri": uri})
	if err != nil {
		return nil, err
	}
	var res struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     []byte `json:"blob"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: resources/read result: %w", err)
	}
	out := make([]ResourceContent, len(res.Contents))
	for i, rc := range res.Contents {
		out[i] = ResourceContent{URI: rc.URI, MimeType: rc.MimeType, Text: rc.Text, Blob: rc.Blob}
	}
	return out, nil
}

// Asks for update notifications about a resource
func (c *Client) Subscribe(ctx context.Context, uri string) error {
	if !c.server.ResourcesSubscribe {
		return fmt.Errorf("mcp: server %s does not support resource subscriptions", c.name)
	}
	_, err := c.Call(ctx, "resources/subscribe", map[string]string{"uri": uri})
	return err
}

// Stops update notifications about a resource
func (c *Client) Unsubscribe(ctx context.Context, uri string) error {
	_, err := c.Call(ctx, "resources/unsubscribe", map[string]string{"uri": uri})
	return err
}

// Lists every prompt of the server, following pagination
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if !c.server.Prompts {
		return nil, fmt.Errorf("mcp: server %s offers no prompts", c.name)
	}
	var out []Prompt
	cursor := ""
	for {
		var page struct {
			Prompts    []Prompt `json:"prompts"`
			NextCursor string   `json:"nextCursor"`
		}
		if err := c.page(ctx, "prompts/list", cursor, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Prompts...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// Renders a prompt with arguments into neutral messages
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	raw, err := c.Call(ctx, "prompts/get", params)
	if err != nil {
		return PromptResult{}, err
	}
	var res struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string  `json:"role"`
			Content content `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return PromptResult{}, fmt.Errorf("mcp: prompts/get result: %w", err)
	}
	out := PromptResult{Description: res.Description}
	for i, m := range res.Messages {
		parts, err := decodeContent([]content{m.Content})
		if err != nil {
			return PromptResult{}, fmt.Errorf("mcp: prompt message %d: %w", i, err)
		}
		role := protocol.RoleUser
		if m.Role == "assistant" {
			role = protocol.RoleAssistant
		}
		out.Messages = append(out.Messages, protocol.Message{Role: role, Content: parts})
	}
	return out, nil
}
