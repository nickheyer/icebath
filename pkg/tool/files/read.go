package files

import (
	"bytes"
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

// Media type an image file announces in its first bytes, empty for anything else
func sniffImage(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

const readSchema = `{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to read, relative paths resolve against the working directory"},
"offset":{"type":"integer","minimum":1,"description":"First line to return, counted from 1"},
"limit":{"type":"integer","minimum":1,"description":"Number of lines to return"}},
"required":["file_path"],"additionalProperties":false}`

// Structured outcome of Read
type ReadData struct {
	Path string
	// First line number returned
	Offset int
	Lines  []string
	// Lines in the whole file
	Total int
	// Media type when the file was returned as an image
	MediaType string
	Bytes     int64
}

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
	if err := ctx.Err(); err != nil {
		return tool.Error(err.Error()), nil
	}
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
	if st.Size() == 0 {
		return tool.Textf("%s is empty", p).With(ReadData{Path: p, Offset: 1}), nil
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
	if media := sniffImage(head); media != "" {
		return r.image(p, media, st.Size())
	}
	if media, ok := imageTypes[strings.ToLower(filepath.Ext(p))]; ok && isBinary(head) {
		return r.image(p, media, st.Size())
	}
	if isBinary(head) {
		return tool.Errorf("%s is a binary file", p), nil
	}
	return r.text(f, p, in.Offset, in.Limit, st.Size())
}

func (r read) image(p, media string, size int64) (tool.Result, error) {
	if size > int64(r.maxFileBytes()) {
		return tool.Errorf("%s is %d bytes, over the %d byte image limit", p, size, r.maxFileBytes()), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Image(media, data).With(ReadData{Path: p, MediaType: media, Bytes: size}), nil
}

// Streams the file once, collecting the requested window and counting every line
func (r read) text(f *os.File, p string, offset, limit int, size int64) (tool.Result, error) {
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 || limit > r.maxLines() {
		limit = r.maxLines()
	}
	var window []string
	total := 0
	err := lines(f, func(line string) bool {
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
	return tool.Text(out).With(ReadData{Path: p, Offset: offset, Lines: window, Total: total, Bytes: size}), nil
}
