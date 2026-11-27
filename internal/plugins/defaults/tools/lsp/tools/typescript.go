package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// tsserver's categories as the severities typescript-language-server pushes them with
var tsSeverity = map[string]int{"error": 1, "warning": 2, "message": 3, "suggestion": 4}

// tsserver requests that answer what is wrong with a file now
var tsCommands = []string{"syntacticDiagnosticsSync", "semanticDiagnosticsSync"}

// Whether the client tells what is wrong with a file now through a synchronous request, rather than a push later
func (c *client) syncs() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.server == "typescript" && !c.syncOff
}

func (c *client) syncUnsupported() {
	c.mu.Lock()
	c.syncOff = true
	c.mu.Unlock()
}

// One location of tsserver, counted from 1
type tsLocation struct {
	Line   int `json:"line"`
	Offset int `json:"offset"`
}

// Position counted from 0 as servers push them
func (l *tsLocation) position() position {
	return position{Line: max(l.Line, 1) - 1, Character: max(l.Offset, 1) - 1}
}

// Diagnostics of the text last synced, asked of tsserver; false when the server has no such request, a timeout past the budget
func (c *client) syncDiagnostics(ctx context.Context, abs string, timeout time.Duration) ([]diagnostic, bool, error) {
	if !c.syncs() || timeout <= 0 {
		return nil, false, nil
	}
	out := []diagnostic{}
	for _, command := range tsCommands {
		result, err := c.retried(ctx, "workspace/executeCommand", map[string]any{"command": "typescript.tsserverRequest",
			"arguments": []any{command, map[string]any{"file": abs, "includeLinePosition": true}, map[string]any{"executionTarget": 0}}}, timeout)
		if err != nil {
			_, timedOut := errors.AsType[*timeoutError](err)
			_, refused := errors.AsType[*requestError](err)
			_, broken := errors.AsType[*protocolError](err)
			switch {
			case timedOut:
				return nil, false, &timeoutError{text: fmt.Sprintf("%s did not answer in %.1fs", command, timeout.Seconds())}
			case refused:
				c.logf(slog.LevelDebug, "%s rejected: %s", command, err)
				c.syncUnsupported()
				return nil, false, nil
			case broken:
				c.logf(slog.LevelDebug, "%s failed: %s", command, err)
				return nil, false, nil
			}
			return nil, false, err
		}
		var reply struct {
			Success *bool             `json:"success"`
			Body    []json.RawMessage `json:"body"`
		}
		if json.Unmarshal(result, &reply) != nil || reply.Success != nil && !*reply.Success || reply.Body == nil {
			c.syncUnsupported()
			return nil, false, nil
		}
		for _, raw := range reply.Body {
			var item struct {
				Start    *tsLocation `json:"startLocation"`
				End      *tsLocation `json:"endLocation"`
				Category string      `json:"category"`
				Message  text        `json:"message"`
				Code     text        `json:"code"`
				Source   text        `json:"source"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Start == nil || item.End == nil {
				continue
			}
			severity, ok := tsSeverity[item.Category]
			if !ok {
				severity = 1
			}
			source := item.Source
			if source == "" {
				source = "typescript"
			}
			out = append(out, diagnostic{Range: span{Start: item.Start.position(), End: item.End.position()}, Severity: severity, Code: item.Code,
				Source: source, Message: item.Message})
		}
	}
	return out, true, nil
}
