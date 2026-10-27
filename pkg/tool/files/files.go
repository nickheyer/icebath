// File tools that read, write, edit, glob and grep inside an optional root
package files

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const (
	ReadName  = "Read"
	WriteName = "Write"
	EditName  = "Edit"
	GlobName  = "Glob"
	GrepName  = "Grep"
)

// Directory names skipped by Glob and Grep
var ignored = []string{".git", ".hg", ".svn", "node_modules", ".venv", "venv", "__pycache__", ".cache", ".next", ".turbo"}

const (
	// Lines one Read returns
	maxLines = 2000
	// Bytes kept of one line before it is cut
	maxLineBytes = 2000
	// Largest file Read loads whole and Edit rewrites
	maxFileBytes = 8 << 20
	// Entries Glob and Grep list
	maxResults = 1000
	// Bytes of Grep output
	maxOutput = 50000
)

// Shared settings of every file tool
type Options struct {
	// Directory relative paths resolve against, the process working directory when empty
	Dir string
}

// Every file tool sharing one set of options
func All(o Options) []tool.Tool {
	return []tool.Tool{Read(o), Write(o), Edit(o), Glob(o), Grep(o)}
}

// Content replaced by Write or Edit, with the whole file before and after
type Change struct {
	Path     string
	Old      string
	New      string
	Created  bool
	Replaced int
}

// One path found by Glob
type Match struct {
	Path    string
	Dir     bool
	ModTime time.Time
}

// Structured outcome of Glob
type GlobData struct {
	Dir     string
	Pattern string
	Matches []Match
	Total   int
}

func (o Options) dir() (string, error) {
	if o.Dir != "" {
		return filepath.Abs(o.Dir)
	}
	return os.Getwd()
}

// Turns a user path into a clean absolute path
func (o Options) Resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		base, err := o.dir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, v)
}

// Reports whether the first bytes contain a NUL, the mark of a binary file
func isBinary(head []byte) bool {
	return bytes.IndexByte(head, 0) >= 0
}

// Reads up to 8 KiB from the start of the file for the binary check
func peek(f *os.File) ([]byte, error) {
	head := make([]byte, 8192)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return head[:n], nil
}

// Yields every line of r without its line ending
func lines(r io.Reader, fn func(line string) bool) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if line == "" && err == io.EOF {
			return nil
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if !fn(line) || err == io.EOF {
			return nil
		}
	}
}

// Cuts a line at the byte budget on a rune boundary and marks how much was dropped
func (o Options) clip(line string) string {
	limit := maxLineBytes
	if len(line) <= limit {
		return line
	}
	for limit > 0 && !isRuneStart(line[limit]) {
		limit--
	}
	return fmt.Sprintf("%s [+%d bytes]", line[:limit], len(line)-limit)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Renders numbered lines in cat -n form starting at the given line number
func (o Options) numbered(start int, ls []string) string {
	var sb strings.Builder
	for i, l := range ls {
		fmt.Fprintf(&sb, "%6d\t%s\n", start+i, o.clip(l))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// Writes data to p through a temporary file, following a symlink and keeping the mode of an existing file
func writeFile(p string, data []byte) (created bool, err error) {
	mode := fs.FileMode(0o644)
	st, err := os.Lstat(p)
	switch {
	case err == nil && st.Mode()&fs.ModeSymlink != 0:
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			return false, err
		}
		p = target
		st, err = os.Stat(p)
		if err != nil {
			return false, err
		}
		if st.IsDir() {
			return false, fmt.Errorf("%s is a directory", p)
		}
		mode = st.Mode().Perm()
	case err == nil:
		if st.IsDir() {
			return false, fmt.Errorf("%s is a directory", p)
		}
		mode = st.Mode().Perm()
	case errors.Is(err, fs.ErrNotExist):
		created = true
	default:
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	return created, nil
}

// Reads the current content of p for a change record, empty when it does not exist
func current(p string) (string, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type found struct {
	path string
	mod  time.Time
}

// Visits every entry below dir in lexical order, skipping ignored directory names and gitignored paths
func (o Options) walk(dir string, dirs bool, visit func(path string, d fs.DirEntry) error) error {
	rules := loadIgnoreStack(dir)
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		if p == dir {
			return nil
		}
		isDir := d.IsDir()
		if isDir && slices.Contains(ignored, d.Name()) {
			return fs.SkipDir
		}
		if rules.ignored(p, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if isDir {
			rules.push(p)
			if !dirs {
				return nil
			}
		}
		return visit(p, d)
	})
}

// Orders newest first, then by path for equal times
func byNewest(fs []found) {
	sort.SliceStable(fs, func(i, j int) bool {
		if !fs[i].mod.Equal(fs[j].mod) {
			return fs[i].mod.After(fs[j].mod)
		}
		return fs[i].path < fs[j].path
	})
}

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
		Description: fmt.Sprintf("Reads a file and returns its lines numbered in cat -n form, up to %d lines per call with offset and limit to page through longer files. Lines over %d bytes are cut. PNG, JPEG, GIF and WebP files come back as images.", maxLines, maxLineBytes),
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
	p, err := r.Resolve(in.FilePath)
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
	if size > int64(maxFileBytes) {
		return tool.Errorf("%s is %d bytes, over the %d byte image limit", p, size, maxFileBytes), nil
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
	if limit <= 0 || limit > maxLines {
		limit = maxLines
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
	if err := ctx.Err(); err != nil {
		return tool.Error(err.Error()), nil
	}
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	p, err := w.Resolve(in.FilePath)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	old, err := current(p)
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
	return tool.Textf("%s %s (%d bytes)", verb, p, len(in.Content)).With(Change{Path: p, Old: old, New: in.Content, Created: created, Replaced: 1}), nil
}

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
	if st.Size() > int64(maxFileBytes) {
		return tool.Errorf("%s is %d bytes, over the %d byte edit limit", p, st.Size(), maxFileBytes), nil
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
