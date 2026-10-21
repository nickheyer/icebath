package files

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const editSchema = `{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to edit"},
"old_string":{"type":"string","description":"Exact text to replace, it must occur once unless replace_all is set"},
"new_string":{"type":"string","description":"Replacement text, must differ from old_string"},
"replace_all":{"type":"boolean","description":"Replace every occurrence instead of requiring a unique one"}},
"required":["file_path","old_string","new_string"],"additionalProperties":false}`

const editContext = 3

type edit struct{ Options }

// Tool that replaces exact text in a file and shows the edited region back
func Edit(o Options) tool.Tool { return edit{o} }

func (edit) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        EditName,
		Description: "Replaces exact text in a file. old_string must match the file byte for byte including whitespace and must be unique unless replace_all is true. Returns the edited region with line numbers.",
		Parameters:  json.RawMessage(editSchema),
	}
}

func (edit) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true}
}

func (e edit) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Error(err.Error()), nil
	}
	var in struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if in.OldString == "" {
		return tool.Error("old_string is empty, use Write to create content"), nil
	}
	if in.OldString == in.NewString {
		return tool.Error("old_string and new_string are identical"), nil
	}
	p, err := e.Resolve(in.FilePath)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return tool.Errorf("%s does not exist", p), nil
	}
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if st.IsDir() {
		return tool.Errorf("%s is a directory", p), nil
	}
	if st.Size() > int64(e.maxFileBytes()) {
		return tool.Errorf("%s is %d bytes, over the %d byte edit limit", p, st.Size(), e.maxFileBytes()), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if isBinary(data[:min(len(data), 8192)]) {
		return tool.Errorf("%s is a binary file", p), nil
	}
	content := string(data)
	count := strings.Count(content, in.OldString)
	switch {
	case count == 0:
		return tool.Errorf("old_string not found in %s", p), nil
	case count > 1 && !in.ReplaceAll:
		return tool.Errorf("old_string occurs %d times in %s, add surrounding context to make it unique or set replace_all", count, p), nil
	}
	first := strings.Index(content, in.OldString)
	if !in.ReplaceAll {
		count = 1
	}
	updated := strings.Replace(content, in.OldString, in.NewString, count)
	if _, err := writeFile(p, []byte(updated)); err != nil {
		return tool.Error(err.Error()), nil
	}
	text := tool.Textf("Replaced %d occurrence(s) in %s\n%s", count, p, e.snippet(updated, first, len(in.NewString)))
	return text.With(Change{Path: p, Old: content, New: updated, Replaced: count}), nil
}

// Numbered lines around the first replacement with a few lines of context on each side
func (e edit) snippet(content string, at, length int) string {
	ls := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		ls = ls[:len(ls)-1]
	}
	startLine := strings.Count(content[:at], "\n")
	endLine := startLine + strings.Count(content[at:min(at+length, len(content))], "\n")
	from := max(startLine-editContext, 0)
	to := min(endLine+editContext, len(ls)-1)
	if from > to {
		return ""
	}
	return e.numbered(from+1, ls[from:to+1])
}
