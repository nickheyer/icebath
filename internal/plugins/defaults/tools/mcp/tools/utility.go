package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/tools"
)

// Handler of a utility tool of a server: its required argument checked before anything is sent, its answer as JSON
func (p *pool) utilityHandler(server, key string) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		p.mu.Lock()
		srv := p.servers[server]
		p.mu.Unlock()
		if srv == nil {
			return failure(fmt.Sprintf("MCP server '%s' is not connected", server))
		}
		timeout := p.timeout(srv.currentSpec())
		live, err := p.live(ctx, server, min(5*time.Second, timeout))
		if err != nil || live == nil {
			return failure(fmt.Sprintf("MCP server '%s' is not connected", server))
		}
		args := call.Args
		if args == nil {
			args = map[string]any{}
		}
		for _, u := range utilities {
			if u.key == key && len(u.required) > 0 && !present(args[u.required[0]]) {
				return failure(fmt.Sprintf("Missing required parameter '%s'", u.required[0]))
			}
		}
		r := &rendering{pool: p, server: server}
		once := func() (string, error) {
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := live.lock(callCtx); err != nil {
				return "", err
			}
			defer live.unlock()
			live.called()
			_, session, _ := live.snapshot()
			if session == nil {
				return "", fmt.Errorf("MCP server '%s' has no active session", server)
			}
			payload, err := utilityCall(callCtx, session, key, args, r)
			if err != nil {
				return "", err
			}
			return tools.JSON(payload), nil
		}
		return p.dispatch(ctx, live, strings.ReplaceAll(key, "_", "/"), once, true, false)
	}
}

// Whether an argument holds something
func present(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	}
	return true
}

func utilityCall(ctx context.Context, session *mcp.Session, key string, args map[string]any, r *rendering) (any, error) {
	switch key {
	case "list_resources":
		resources, err := session.ListResources(ctx)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, res := range resources {
			if res == nil {
				continue
			}
			entry := map[string]any{"uri": res.URI, "name": res.Name}
			if res.Description != "" {
				entry["description"] = res.Description
			}
			if res.MimeType != "" {
				entry["mimeType"] = res.MimeType
			}
			out = append(out, entry)
		}
		return map[string]any{"resources": out}, nil
	case "read_resource":
		uri, ok := args["uri"].(string)
		if !ok {
			return nil, fmt.Errorf("1 validation error for ReadResourceRequestParams\nuri\n  Input should be a valid string")
		}
		result, err := session.ReadResource(ctx, uri)
		if err != nil {
			return nil, err
		}
		var parts []string
		for _, block := range result.Contents {
			switch {
			case block == nil:
			case block.Text != nil:
				parts = append(parts, stripTags(*block.Text))
			case block.Blob != nil:
				rendered := r.resource(&mcp.ContentBlock{Type: "resource", Resource: block})
				if rendered == "" {
					rendered = fmt.Sprintf("[binary data, %d bytes]", utf8.RuneCountInString(*block.Blob))
				}
				parts = append(parts, rendered)
			}
		}
		return map[string]any{"result": strings.Join(parts, "\n")}, nil
	case "list_prompts":
		prompts, err := session.ListPrompts(ctx)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, prompt := range prompts {
			if prompt == nil {
				continue
			}
			entry := map[string]any{"name": prompt.Name}
			if prompt.Description != "" {
				entry["description"] = prompt.Description
			}
			if len(prompt.Arguments) > 0 {
				var arguments []any
				for _, a := range prompt.Arguments {
					if a == nil {
						continue
					}
					argument := map[string]any{"name": a.Name, "required": a.Required != nil && *a.Required}
					if a.Description != "" {
						argument["description"] = a.Description
					}
					arguments = append(arguments, argument)
				}
				entry["arguments"] = arguments
			}
			out = append(out, entry)
		}
		return map[string]any{"prompts": out}, nil
	case "get_prompt":
		name, ok := args["name"].(string)
		if !ok {
			return nil, fmt.Errorf("1 validation error for GetPromptRequestParams\nname\n  Input should be a valid string")
		}
		values, err := promptArguments(args)
		if err != nil {
			return nil, err
		}
		result, err := session.GetPrompt(ctx, name, values)
		if err != nil {
			return nil, err
		}
		messages := []any{}
		for i, msg := range result.Messages {
			if msg == nil {
				return nil, fmt.Errorf("1 validation error for GetPromptResult\nmessages.%d\n  Input should be a prompt message object", i)
			}
			content, err := r.promptContent(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("1 validation error for GetPromptResult\nmessages.%d.content\n  %s", i, err)
			}
			messages = append(messages, map[string]any{"role": msg.Role, "content": stripTags(content)})
		}
		out := map[string]any{"messages": messages}
		if result.Description != "" {
			out["description"] = result.Description
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown utility %s", key)
}

// Prompt arguments as the text table prompts/get takes, a validation error for any other shape
func promptArguments(args map[string]any) (map[string]string, error) {
	raw, ok := args["arguments"]
	if !ok || raw == nil {
		return map[string]string{}, nil
	}
	table, isTable := raw.(map[string]any)
	if !isTable {
		return nil, fmt.Errorf("1 validation error for GetPromptRequestParams\narguments\n  Input should be a valid dictionary")
	}
	out := make(map[string]string, len(table))
	var bad []string
	for _, k := range slices.Sorted(maps.Keys(table)) {
		text, ok := table[k].(string)
		if !ok {
			bad = append(bad, "arguments."+k+"\n  Input should be a valid string")
			continue
		}
		out[k] = text
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%d validation error(s) for GetPromptRequestParams\n%s", len(bad), strings.Join(bad, "\n"))
	}
	return out, nil
}
