package files

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const writeSchema = `{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to write, parent directories are created"},
"content":{"type":"string","description":"Complete new content of the file"}},
"required":["file_path","content"],"additionalProperties":false}`

type write struct{ Options }

// Tool that replaces a whole file atomically, creating parents and keeping the mode
func Write(o Options) tool.Tool { return write{o} }

func (write) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        WriteName,
		Description: "Writes the complete content of a file, creating it and any missing parent directories or replacing what is there. Prefer Edit for changing part of an existing file.",
		Parameters:  json.RawMessage(writeSchema),
	}
}

func (write) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true, Idempotent: true}
}

func (w write) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	p, err := w.resolve(in.FilePath)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	created, err := writeFile(p, []byte(in.Content))
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	verb := "Updated"
	if created {
		verb = "Created"
	}
	return tool.Textf("%s %s (%d bytes)", verb, p, len(in.Content)), nil
}
