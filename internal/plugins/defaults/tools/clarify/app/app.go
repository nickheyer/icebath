package app

import (
	"encoding/json"
	"strings"

	"github.com/nickheyer/icebath/internal/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

type views struct{}

func Register(registry *toolsapp.Registry) {
	registry.Register(views{})
}

// Questions show as each question with its answer, and why they ended when they were not all answered
func (views) Views() []toolsapp.View {
	return []toolsapp.View{{Tool: "clarify", Line: line, Result: answers}}
}

// How many questions the call asks
func line(call toolsapp.Call) string {
	var args struct {
		Questions []json.RawMessage `json:"questions"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil || len(args.Questions) == 0 {
		return ""
	}
	return render.Count(len(args.Questions), "question")
}

type result struct {
	Responses []struct {
		Question     string `json:"question"`
		Status       string `json:"status"`
		UserResponse any    `json:"user_response"`
	} `json:"responses"`
	Outcome string `json:"outcome"`
	Notice  string `json:"notice"`
}

func answers(call toolsapp.Call, _ int, look theme.Theme) *toolsapp.Shown {
	var settled result
	if json.Unmarshal([]byte(call.Result), &settled) != nil || len(settled.Responses) == 0 {
		return nil
	}
	shown := &toolsapp.Shown{Head: "answered", Open: true}
	switch settled.Outcome {
	case tools.Submitted:
	case tools.Cancelled:
		shown.Head = "dismissed"
	case tools.TimedOut:
		shown.Head = "timed out"
	default:
		shown.Head = strings.TrimSpace(settled.Outcome + " " + settled.Notice)
	}
	for _, response := range settled.Responses {
		var answer string
		switch value := response.UserResponse.(type) {
		case string:
			answer = value
		case []any:
			var picked []string
			for _, item := range value {
				if text, ok := item.(string); ok {
					picked = append(picked, text)
				}
			}
			answer = strings.Join(picked, ", ")
		}
		switch {
		case answer != "":
			shown.Rows = append(shown.Rows, look.Glyphs.Check+" "+response.Question+" → "+render.Good(answer, look))
		case response.Status == "skipped":
			shown.Rows = append(shown.Rows, render.Dim(look.Glyphs.Skip+" "+response.Question+" → (skipped)", look))
		default:
			shown.Rows = append(shown.Rows, render.Dim(look.Glyphs.Bullet+" "+response.Question+" (no answer)", look))
		}
	}
	return shown
}
