package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

type toolDef struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
		OpenWorldHint   *bool `json:"openWorldHint"`
	} `json:"annotations"`
}

type listResult struct {
	Tools      []toolDef `json:"tools"`
	NextCursor string    `json:"nextCursor"`
}

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
		Blob     string `json:"blob"`
	} `json:"resource"`
}

type callResult struct {
	Content           []content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Builds the local name of a server tool, mcp, the server label and the tool name joined by double underscores
func ToolName(server, name string) string {
	return "mcp__" + unsafeName.ReplaceAllString(server, "_") + "__" + unsafeName.ReplaceAllString(name, "_")
}

// Lists every tool of the server, following pagination, wrapped as local tools
func (c *Client) Tools(ctx context.Context) ([]tool.Tool, error) {
	if !c.server.Tools {
		return nil, fmt.Errorf("mcp: server %s offers no tools", c.name)
	}
	var out []tool.Tool
	cursor := ""
	for {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		raw, err := c.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page listResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("mcp: tools/list result: %w", err)
		}
		for _, d := range page.Tools {
			out = append(out, c.wrap(d))
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// Server tool callable through the client
type remote struct {
	c     *Client
	name  string
	def   protocol.Tool
	hints tool.Annotations
}

// Maps a server definition to a local tool, applying the MCP defaults for missing hints
func (c *Client) wrap(d toolDef) tool.Tool {
	schema := bytes.TrimSpace(d.InputSchema)
	if !bytes.HasPrefix(schema, []byte("{")) || !json.Valid(schema) {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	desc := d.Description
	if desc == "" {
		desc = d.Title
	}
	hints := tool.Annotations{Destructive: true, OpenWorld: true}
	if a := d.Annotations; a != nil {
		if a.ReadOnlyHint != nil {
			hints.ReadOnly = *a.ReadOnlyHint
		}
		if a.DestructiveHint != nil {
			hints.Destructive = *a.DestructiveHint
		}
		if a.IdempotentHint != nil {
			hints.Idempotent = *a.IdempotentHint
		}
		if a.OpenWorldHint != nil {
			hints.OpenWorld = *a.OpenWorldHint
		}
	}
	return remote{c: c, name: d.Name, def: protocol.Tool{Name: ToolName(c.name, d.Name), Description: desc, Parameters: schema}, hints: hints}
}

func (r remote) Definition() protocol.Tool     { return r.def }
func (r remote) Annotations() tool.Annotations { return r.hints }

// Name the server knows the tool by
func (r remote) RemoteName() string { return r.name }

func (r remote) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := r.c.Call(ctx, "tools/call", map[string]any{"name": r.name, "arguments": args})
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	var res callResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return tool.Errorf("mcp: tools/call result: %v", err), nil
	}
	parts, err := decodeContent(res.Content)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if len(parts) == 0 && len(res.StructuredContent) > 0 {
		parts = append(parts, protocol.Text{Text: string(res.StructuredContent)})
	}
	if len(parts) == 0 {
		parts = append(parts, protocol.Text{Text: "(no content)"})
	}
	return tool.Result{Content: parts, IsError: res.IsError}, nil
}

// Maps every MCP content kind onto neutral parts, carrying binary data as base64 text when no part fits
func decodeContent(items []content) ([]protocol.Part, error) {
	var parts []protocol.Part
	for i, it := range items {
		switch it.Type {
		case "text":
			parts = append(parts, protocol.Text{Text: it.Text})
		case "image":
			data, err := base64.StdEncoding.DecodeString(it.Data)
			if err != nil {
				return nil, fmt.Errorf("mcp: content %d: image data: %w", i, err)
			}
			parts = append(parts, protocol.Image{MediaType: it.MimeType, Data: data})
		case "audio":
			parts = append(parts, protocol.Text{Text: fmt.Sprintf("[audio %s base64]\n%s", it.MimeType, it.Data)})
		case "resource":
			if it.Resource == nil {
				return nil, fmt.Errorf("mcp: content %d: resource without a body", i)
			}
			r := it.Resource
			switch {
			case r.Blob != "" && strings.HasPrefix(r.MimeType, "image/"):
				data, err := base64.StdEncoding.DecodeString(r.Blob)
				if err != nil {
					return nil, fmt.Errorf("mcp: content %d: resource blob: %w", i, err)
				}
				parts = append(parts, protocol.Image{MediaType: r.MimeType, Data: data})
			case r.Blob != "":
				parts = append(parts, protocol.Text{Text: fmt.Sprintf("[%s %s base64]\n%s", r.URI, r.MimeType, r.Blob)})
			default:
				parts = append(parts, protocol.Text{Text: fmt.Sprintf("[%s]\n%s", r.URI, r.Text)})
			}
		case "resource_link":
			label := it.Name
			if label == "" {
				label = it.URI
			}
			parts = append(parts, protocol.Text{Text: fmt.Sprintf("[resource link] %s: %s", label, it.URI)})
		default:
			return nil, fmt.Errorf("mcp: content %d has unsupported type %q", i, it.Type)
		}
	}
	return parts, nil
}
