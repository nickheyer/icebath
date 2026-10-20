package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

var imageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

const readSchema = `{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to read, relative paths resolve against the working directory"},
"offset":{"type":"integer","minimum":1,"description":"First line to return, counted from 1"},
"limit":{"type":"integer","minimum":1,"description":"Number of lines to return"}},
"required":["file_path"],"additionalProperties":false}`

type read struct{ Options }

// Tool that returns numbered lines of a text file or the bytes of an image
func Read(o Options) tool.Tool { return read{o} }

func (r read) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        ReadName,
		Description: fmt.Sprintf("Reads a file and returns its lines numbered in cat -n form, up to %d lines per call with offset and limit to page through longer files. Lines over %d bytes are cut. PNG, JPEG, GIF and WebP files come back as images.", r.maxLines(), r.maxLineBytes()),
		Parameters:  json.RawMessage(readSchema),
	}
}

func (read) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: true, Idempotent: true}
}

func (r read) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	p, err := r.resolve(in.FilePath)
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
		return tool.Errorf("%s is a directory, use Glob to list it", p), nil
	}
	if media, ok := imageTypes[strings.ToLower(filepath.Ext(p))]; ok {
		return r.image(p, media, st.Size())
	}
	return r.text(p, in.Offset, in.Limit, st.Size())
}

func (r read) image(p, media string, size int64) (tool.Result, error) {
	if size > int64(r.maxFileBytes()) {
		return tool.Errorf("%s is %d bytes, over the %d byte image limit", p, size, r.maxFileBytes()), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Image(media, data), nil
}

// Streams the file once, collecting the requested window and counting every line
func (r read) text(p string, offset, limit int, size int64) (tool.Result, error) {
	if size == 0 {
		return tool.Textf("%s is empty", p), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	defer f.Close()
	head, err := peek(f)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if isBinary(head) {
		return tool.Errorf("%s is a binary file", p), nil
	}
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 || limit > r.maxLines() {
		limit = r.maxLines()
	}
	var window []string
	total := 0
	err = lines(f, func(line string) bool {
		total++
		if total >= offset && len(window) < limit {
			window = append(window, line)
		}
		return true
	})
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if len(window) == 0 {
		return tool.Errorf("%s has %d lines, offset %d is past the end", p, total, offset), nil
	}
	out := r.numbered(offset, window)
	last := offset + len(window) - 1
	if last < total {
		out += fmt.Sprintf("\n\n[lines %d-%d of %d, continue with offset=%d]", offset, last, total, last+1)
	}
	return tool.Text(out), nil
}
