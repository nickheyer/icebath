package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Name of the tool that connects MCP servers, as the engine registers it
const manageTool = "manage_connections"

// Words a settled operation's head ends with, by how it settled
var settledWords = map[string]string{"continue": "the agent went on", "deadline": "time ran out", "interrupt": "it was stopped"}

// manage_connections shows its action and servers on its line, and each server's outcome under it
func (screens) Views() []toolsapp.View {
	return []toolsapp.View{{Tool: manageTool, Line: manageLine, Result: manageResult}}
}

func manageLine(call toolsapp.Call) string {
	var args struct {
		Action     string            `json:"action"`
		Connectors []json.RawMessage `json:"connectors"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return ""
	}
	var names []string
	for _, raw := range args.Connectors {
		var name string
		if json.Unmarshal(raw, &name) != nil {
			var named struct {
				Name string `json:"name"`
			}
			json.Unmarshal(raw, &named)
			name = named.Name
		}
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return strings.TrimSpace(cmp.Or(verbs[args.Action], args.Action) + " " + strings.Join(names, ", "))
}

type manageOutcome struct {
	Error   string `json:"error"`
	Settled string `json:"settled_by"`
	Status  string `json:"status"`
	Targets []struct {
		Name           string   `json:"name"`
		State          string   `json:"state"`
		Detail         string   `json:"detail"`
		ConnectURL     string   `json:"connect_url"`
		Tools          []string `json:"tools"`
		DiscoveryError string   `json:"discovery_error"`
	} `json:"targets"`
}

func manageResult(call toolsapp.Call, _ int, look theme.Theme) *toolsapp.Shown {
	var outcome manageOutcome
	if json.Unmarshal([]byte(call.Result), &outcome) != nil {
		return nil
	}
	if outcome.Error != "" {
		return &toolsapp.Shown{Head: "Refused", Rows: []string{render.Warm(outcome.Error, look)}, Open: true}
	}
	if len(outcome.Targets) == 0 {
		return nil
	}
	connected := 0
	shown := &toolsapp.Shown{Open: true}
	for _, target := range outcome.Targets {
		row := target.Name + " · " + cmp.Or(targetWords[target.State], target.State)
		switch target.State {
		case "connected":
			connected++
			row = look.Glyphs.Check + " " + row
			if len(target.Tools) > 0 {
				row += " · " + render.Count(len(target.Tools), "tool")
			}
			row = render.Good(row, look)
		case "failed":
			row = render.Warm(look.Glyphs.Fail+" "+row, look)
		case "skipped", "not_connected":
			row = render.Dim(look.Glyphs.Skip+" "+row, look)
		default:
			row = look.Glyphs.Bullet + " " + row
		}
		if detail := strings.TrimSpace(target.Detail); detail != "" {
			row += render.Dim(" · "+detail, look)
		}
		shown.Rows = append(shown.Rows, row)
		if target.ConnectURL != "" {
			shown.Rows = append(shown.Rows, "  "+render.Warm(target.ConnectURL, look))
		}
		if target.DiscoveryError != "" {
			shown.Rows = append(shown.Rows, "  "+render.Dim(target.DiscoveryError, look))
		}
	}
	shown.Head = fmt.Sprintf("%d of %d connected", connected, len(outcome.Targets))
	if word := settledWords[outcome.Settled]; word != "" {
		shown.Head += ", then " + word
	}
	return shown
}
