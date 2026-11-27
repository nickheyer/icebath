// How the chat shows text_to_speech: the words spoken beside its title, and the files it wrote under the service that spoke them
package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	ttstools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/tts/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

func Register(registry *toolsapp.Registry) {
	registry.Register(views{})
}

type views struct{}

func (views) Views() []toolsapp.View {
	return []toolsapp.View{{Tool: ttstools.Name, Line: line, Result: result}}
}

// Words spoken on one line, or the voices searched for
func line(call toolsapp.Call) string {
	var args struct {
		Text  string         `json:"text"`
		Find  map[string]any `json:"find_voices"`
		Voice string         `json:"voice_id"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return ""
	}
	if args.Find != nil {
		if query, _ := args.Find["query"].(string); query != "" {
			return "find voices: " + query
		}
		return "find voices"
	}
	text := strings.Join(strings.Fields(args.Text), " ")
	if args.Voice != "" {
		text += " (voice " + args.Voice + ")"
	}
	return text
}

// Files written and the service that spoke them; a voice search as its count and names
func result(call toolsapp.Call, width int, look theme.Theme) *toolsapp.Shown {
	if call.Result == "" || call.Failed {
		return nil
	}
	var r struct {
		Success  bool     `json:"success"`
		Error    string   `json:"error"`
		Files    []string `json:"file_paths"`
		Provider string   `json:"provider"`
		Bubble   bool     `json:"voice_compatible"`
		Chunks   int      `json:"chunk_count"`
		Voices   []struct {
			ID   string `json:"voice_id"`
			Name string `json:"name"`
		} `json:"voices"`
		Count int `json:"count"`
	}
	if json.Unmarshal([]byte(call.Result), &r) != nil || !r.Success {
		return nil
	}
	if r.Voices != nil || r.Count > 0 {
		shown := &toolsapp.Shown{Head: render.Dim(fmt.Sprintf("%d voices", r.Count), look)}
		for _, v := range r.Voices {
			shown.Rows = append(shown.Rows, ansi.Truncate(v.Name+"  "+render.Dim(v.ID, look), width, "…"))
		}
		return shown
	}
	about := []string{r.Provider}
	if r.Chunks > 1 {
		about = append(about, fmt.Sprintf("%d pieces", r.Chunks))
	}
	if r.Bubble {
		about = append(about, "voice bubble")
	}
	shown := &toolsapp.Shown{Head: render.Dim("spoken by "+strings.Join(about, " · "), look), Open: true}
	for _, file := range r.Files {
		shown.Rows = append(shown.Rows, ansi.Truncate(file, width, "…"))
	}
	return shown
}
