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

// How a chat shows delegate_task: the goal or the count of tasks on its line, and its results folded to one line a task
type delegateView struct{}

func (delegateView) Views() []toolsapp.View {
	return []toolsapp.View{{Tool: "delegate_task", Line: delegateLine, Result: delegateResult}}
}

func delegateLine(call toolsapp.Call) string {
	var args struct {
		Goal   string `json:"goal"`
		Action string `json:"action"`
		ID     string `json:"subagent_id"`
		Tasks  []struct {
			Goal string `json:"goal"`
		} `json:"tasks"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return ""
	}
	switch {
	case args.Action != "":
		return strings.TrimSpace(args.Action + " " + args.ID)
	case len(args.Tasks) > 1:
		return fmt.Sprintf("%d tasks · %s", len(args.Tasks), oneLine(args.Tasks[0].Goal))
	case len(args.Tasks) == 1:
		return oneLine(args.Tasks[0].Goal)
	}
	return oneLine(args.Goal)
}

func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

// Results of the call, one row a task with how it ended; a call sent to the background says what it started
func delegateResult(call toolsapp.Call, _ int, t theme.Theme) *toolsapp.Shown {
	var result struct {
		Status  string  `json:"status"`
		Total   float64 `json:"total_duration_seconds"`
		Note    string  `json:"note"`
		Results []struct {
			Index    int     `json:"task_index"`
			Status   string  `json:"status"`
			Summary  string  `json:"summary"`
			Error    string  `json:"error"`
			Duration float64 `json:"duration_seconds"`
			Model    string  `json:"model"`
			Calls    int     `json:"api_calls"`
		} `json:"results"`
		Count int      `json:"count"`
		ID    string   `json:"delegation_id"`
		Goals []string `json:"goals"`
		Units []struct {
			ID      string `json:"delegation_id"`
			Indexes []int  `json:"task_indexes"`
		} `json:"units"`
	}
	if json.Unmarshal([]byte(call.Result), &result) != nil {
		return nil
	}
	if result.Status == "dispatched" {
		shown := &toolsapp.Shown{Head: render.Cool(fmt.Sprintf("%s dispatched in the background · %s", render.Count(cmp.Or(result.Count,
			len(result.Goals)), "subagent"), result.ID), t)}
		for i, goal := range result.Goals {
			shown.Rows = append(shown.Rows, fmt.Sprintf("%s [%d] %s", render.Dim(t.Glyphs.Hollow, t), i+1, oneLine(goal)))
		}
		for _, unit := range result.Units {
			shown.Rows = append(shown.Rows, render.Dim(fmt.Sprintf("%s · tasks %v", unit.ID, unit.Indexes), t))
		}
		if result.Note != "" {
			shown.Rows = append(shown.Rows, render.Dim(oneLine(result.Note), t))
		}
		return shown
	}
	if len(result.Results) == 0 {
		return nil
	}
	done, failed := 0, 0
	var rows []string
	for _, r := range result.Results {
		status := statusOf(r.Status, r.Status)
		if status == "completed" {
			done++
		} else {
			failed++
		}
		meta := []string{status}
		if r.Duration > 0 {
			meta = append(meta, spawnDuration(r.Duration))
		}
		if r.Calls > 0 {
			meta = append(meta, render.Count(r.Calls, "call"))
		}
		if r.Model != "" {
			meta = append(meta, r.Model)
		}
		text := cmp.Or(firstLine(r.Summary), firstLine(r.Error))
		rows = append(rows, fmt.Sprintf("%s [%d] %s  %s", statusGlyph(status, t), r.Index+1, render.Dim(strings.Join(meta, " · "), t), text))
	}
	head := fmt.Sprintf("%s · %d completed", render.Count(len(result.Results), "subagent"), done)
	if failed > 0 {
		head += fmt.Sprintf(" · %d not", failed)
	}
	if result.Total > 0 {
		head += " · " + spawnDuration(result.Total)
	}
	if failed > 0 {
		head = render.Warm(head, t)
	} else {
		head = render.Good(head, t)
	}
	return &toolsapp.Shown{Head: head, Rows: rows}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
