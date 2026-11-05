package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// tsserver category to LSP severity, as typescript-language-server maps it
var tsSeverity = map[string]int64{"error": 1, "warning": 2, "message": 3, "suggestion": 4}

var tsServers = map[string]bool{"typescript": true}

var tsCommands = []string{"syntacticDiagnosticsSync", "semanticDiagnosticsSync"}

const syncLogger = "agent.lsp.sync_diagnostics"

// Whether the client answers what is wrong with a file now through a synchronous request
func syncSupported(c *Client) bool { return tsServers[c.ServerID] && !c.syncUnsupported }

// Diagnostics for the text last synced, false when the server has no synchronous request, a TimeoutError past budget
func syncRequest(ctx context.Context, c *Client, absPath string, timeout time.Duration) ([]map[string]any, bool, error) {
	if !syncSupported(c) || timeout <= 0 {
		return nil, false, nil
	}
	out := []map[string]any{}
	for _, command := range tsCommands {
		reply, err := c.sendRequestWithRetry(ctx, "workspace/executeCommand", map[string]any{
			"command": "typescript.tsserverRequest",
			"arguments": []any{command, map[string]any{"file": absPath, "includeLinePosition": true},
				map[string]any{"executionTarget": 0}},
		}, timeout)
		if err != nil {
			var te *TimeoutError
			var re *RequestError
			var pe *ProtocolError
			switch {
			case errors.As(err, &te):
				return nil, false, &TimeoutError{msg: fmt.Sprintf("%s did not answer in %.1fs", command, timeout.Seconds())}
			case errors.As(err, &re):
				c.rt.logf(slog.LevelDebug, syncLogger, "[%s] %s rejected: %s", c.ServerID, command, err)
				c.syncUnsupported = true
				return nil, false, nil
			case errors.As(err, &pe):
				c.rt.logf(slog.LevelDebug, syncLogger, "[%s] %s failed: %s", c.ServerID, command, err)
				return nil, false, nil
			}
			return nil, false, err
		}
		var body any
		if m, ok := reply.(map[string]any); ok && pyrepr.Truthy(dgetOr(m, "success", true)) {
			body = m["body"]
		}
		items, ok := body.([]any)
		if !ok {
			c.syncUnsupported = true
			return nil, false, nil
		}
		for _, item := range items {
			d, err := fromTsserver(item)
			if err != nil {
				return nil, false, err
			}
			if d != nil {
				out = append(out, d)
			}
		}
	}
	return out, true, nil
}

// Python x - 1 on a decoded JSON number, the TypeError for anything else
func pyMinusOne(v any) (any, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return int64(0), nil
		}
		return int64(-1), nil
	case int64:
		return t - 1, nil
	case int:
		return int64(t) - 1, nil
	case float64:
		return t - 1, nil
	}
	return nil, &pyExc{typ: "TypeError", text: fmt.Sprintf("unsupported operand type(s) for -: '%s' and 'int'", pyrepr.TypeName(v))}
}

// One tsserver diagnostic in the 0 based shape typescript-language-server pushes
func fromTsserver(item any) (map[string]any, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return nil, nil
	}
	start, okS := m["startLocation"].(map[string]any)
	end, okE := m["endLocation"].(map[string]any)
	if !okS || !okE {
		return nil, nil
	}
	pos := func(loc map[string]any) (map[string]any, error) {
		line, err := pyMinusOne(dgetOr(loc, "line", int64(1)))
		if err != nil {
			return nil, err
		}
		char, err := pyMinusOne(dgetOr(loc, "offset", int64(1)))
		if err != nil {
			return nil, err
		}
		return map[string]any{"line": line, "character": char}, nil
	}
	s, err := pos(start)
	if err != nil {
		return nil, err
	}
	e, err := pos(end)
	if err != nil {
		return nil, err
	}
	severity, ok := tsSeverity[pyrepr.Str(pyOr(m["category"], "error"))]
	if !ok {
		severity = 1
	}
	diag := map[string]any{
		"range":    map[string]any{"start": s, "end": e},
		"message":  pyrepr.Str(pyOr(m["message"], "")),
		"severity": severity,
		"source":   pyOr(m["source"], "typescript"),
	}
	if code, ok := m["code"]; ok && code != nil {
		diag["code"] = code
	}
	return diag, nil
}
