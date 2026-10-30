package vsc

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool/files"
)

// Before and after text of one file a tool call changes, OldText nil when the file does not exist
type FileChange struct {
	Path    string
	OldText *string
	NewText string
}

// Turns a Write or Edit call into the before and after text, false for calls that change nothing
func FileChangeOf(call protocol.ToolCall, dir string) (FileChange, bool) {
	o := files.Options{Dir: dir}
	switch call.Name {
	case files.WriteName:
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(call.Args, &in); err != nil || in.FilePath == "" {
			return FileChange{}, false
		}
		p, err := o.Resolve(in.FilePath)
		if err != nil {
			return FileChange{}, false
		}
		fc := FileChange{Path: p, NewText: in.Content}
		if data, err := os.ReadFile(p); err == nil {
			old := string(data)
			fc.OldText = &old
		}
		return fc, true
	case files.EditName:
		return editChange(call, o)
	}
	return FileChange{}, false
}

// One replacement as the Edit tool decodes it
type editOp struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// Before and after text of an Edit call by applying its replacements in memory
func editChange(call protocol.ToolCall, o files.Options) (FileChange, bool) {
	var in struct {
		FilePath   string   `json:"file_path"`
		OldString  string   `json:"old_string"`
		NewString  string   `json:"new_string"`
		ReplaceAll bool     `json:"replace_all"`
		Edits      []editOp `json:"edits"`
	}
	if err := json.Unmarshal(call.Args, &in); err != nil || in.FilePath == "" {
		return FileChange{}, false
	}
	ops := in.Edits
	if len(ops) == 0 {
		ops = []editOp{{OldString: in.OldString, NewString: in.NewString, ReplaceAll: in.ReplaceAll}}
	}
	p, err := o.Resolve(in.FilePath)
	if err != nil {
		return FileChange{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return FileChange{}, false
	}
	old := string(data)
	updated := old
	for _, op := range ops {
		if op.OldString == "" {
			return FileChange{}, false
		}
		count := strings.Count(updated, op.OldString)
		if count == 0 || (count > 1 && !op.ReplaceAll) {
			return FileChange{}, false
		}
		if !op.ReplaceAll {
			count = 1
		}
		updated = strings.Replace(updated, op.OldString, op.NewString, count)
	}
	return FileChange{Path: p, OldText: &old, NewText: updated}, true
}
