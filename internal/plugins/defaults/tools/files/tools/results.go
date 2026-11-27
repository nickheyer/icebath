package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools"
)

// Key of a refusal of a call that repeats one already answered, which nothing failed
const guardrail = "guardrail_refusal"

// Error result with the text cut to the error budget and the extra keys after it, in order
func failure(message string, extra ...any) string {
	var b bytes.Buffer
	b.WriteString(`{"error":`)
	b.WriteString(encode(tools.BoundError(message)))
	for i := 0; i+1 < len(extra); i += 2 {
		b.WriteString("," + encode(extra[i]) + ":" + encode(extra[i+1]))
	}
	b.WriteString("}")
	return b.String()
}

func encode(v any) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// One file read as the model reads it
type readResult struct {
	Content        string   `json:"content"`
	TotalLines     int      `json:"total_lines"`
	FileSize       int64    `json:"file_size"`
	Truncated      bool     `json:"truncated"`
	TruncatedLines bool     `json:"truncated_lines,omitempty"`
	Hint           string   `json:"hint,omitempty"`
	IsBinary       bool     `json:"is_binary"`
	IsImage        bool     `json:"is_image"`
	Error          string   `json:"error,omitempty"`
	NotFound       bool     `json:"not_found"`
	SimilarFiles   []string `json:"similar_files,omitempty"`
	Expired        bool     `json:"expired,omitempty"`
	TruncatedBy    string   `json:"truncated_by,omitempty"`
	NextOffset     int      `json:"next_offset,omitempty"`
	ConflictBlocks int      `json:"conflict_blocks,omitempty"`
	Note           string   `json:"_hint,omitempty"`
	Warning        string   `json:"_warning,omitempty"`

	// Leading bytes of the binary file carry the signature of a document the reader reads
	document bool
	// Bytes of a binary read and the sha256 of the exact bytes of a raw read
	data   []byte
	sha256 string
	// Stat identity and digest of what a direct read saw
	version *fileVersion
}

// Outcome of a syntax check after a write
type lintResult struct {
	ok      bool
	skipped bool
	output  string
	message string
}

func (l *lintResult) MarshalJSON() ([]byte, error) {
	if l.skipped {
		return json.Marshal(struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}{"skipped", l.message})
	}
	status := "error"
	if l.ok {
		status = "ok"
	}
	return json.Marshal(struct {
		Status  string `json:"status"`
		Output  string `json:"output"`
		Message string `json:"message,omitempty"`
	}{status, l.output, l.message})
}

// One write as the model reads it
type writeResult struct {
	BytesWritten   int         `json:"bytes_written"`
	DirsCreated    bool        `json:"dirs_created"`
	Verified       *bool       `json:"verified,omitempty"`
	Lint           *lintResult `json:"lint,omitempty"`
	LSPDiagnostics string      `json:"lsp_diagnostics,omitempty"`
	Error          string      `json:"error,omitempty"`
	Warning        string      `json:"_warning,omitempty"`
	Hint           string      `json:"hint,omitempty"`
	ResolvedPath   string      `json:"resolved_path,omitempty"`
	FilesModified  []string    `json:"files_modified,omitempty"`

	// Sha256 of the bytes written, empty when they could not be hashed
	sha256 string
}

// One write of a patch: the file and the sha256 of the bytes it read and wrote
type patchWrite struct {
	path         string
	read         string
	readKnown    bool
	written      string
	writtenKnown bool
}

// One patch as the model reads it
type patchResult struct {
	Success        bool     `json:"success"`
	NoChange       bool     `json:"no_change,omitempty"`
	Note           string   `json:"note,omitempty"`
	Diff           string   `json:"diff,omitempty"`
	FilesModified  []string `json:"files_modified,omitempty"`
	FilesCreated   []string `json:"files_created,omitempty"`
	FilesDeleted   []string `json:"files_deleted,omitempty"`
	Lint           any      `json:"lint,omitempty"`
	LSPDiagnostics string   `json:"lsp_diagnostics,omitempty"`
	Error          string   `json:"error,omitempty"`
	Warning        string   `json:"_warning,omitempty"`
	ResolvedPath   string   `json:"resolved_path,omitempty"`
	Hint           string   `json:"_hint,omitempty"`

	writes []patchWrite
}

// One search hit
type match struct {
	Path    string
	Line    string
	Content string
}

// Match count of one file
type count struct {
	path string
	n    int
}

// Counts by file in the order they were found
type counts []count

func (c counts) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{")
	for i, entry := range c {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(encode(entry.path) + ":" + strconv.Itoa(entry.n))
	}
	b.WriteString("}")
	return b.Bytes(), nil
}

// Sets the count of a file, keeping its place when it is there
func (c *counts) set(path string, n int) {
	for i := range *c {
		if (*c)[i].path == path {
			(*c)[i].n = n
			return
		}
	}
	*c = append(*c, count{path, n})
}

// One search as the tool hands it on
type searchResult struct {
	matches     []match
	files       []string
	counts      counts
	total       int
	truncated   bool
	limitReason string
	warning     string
	err         string
	// Unreadable paths behind the skipped sentence of the warning, and how many more the diagnostics left out
	skipped        []string
	skippedOmitted int
}

// Matches below this many are listed as they are, more grouped by file
const groupFrom = 5

// The search as the model reads it, with what the tool adds after it
func (r *searchResult) json(extra ...string) string {
	var b bytes.Buffer
	b.WriteString(`{"total_count":` + strconv.Itoa(r.total))
	if len(r.matches) >= groupFrom {
		var lines []string
		current := ""
		for i, m := range r.matches {
			if i == 0 || m.Path != current {
				lines = append(lines, m.Path)
				current = m.Path
			}
			lines = append(lines, "  "+m.Line+": "+strings.TrimRightFunc(m.Content, unicode.IsSpace))
		}
		b.WriteString(`,"matches_format":` + encode("path-grouped: each file path on its own line, followed by indented '<line>: <content>' rows for "+
			"matches in that file"))
		b.WriteString(`,"matches_text":` + encode(strings.Join(lines, "\n")))
	} else if len(r.matches) > 0 {
		b.WriteString(`,"matches":[`)
		for i, m := range r.matches {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"path":` + encode(m.Path) + `,"line":` + m.Line + `,"content":` + encode(m.Content) + `}`)
		}
		b.WriteString("]")
	}
	if len(r.files) > 0 {
		b.WriteString(`,"files":` + encode(r.files))
	}
	if len(r.counts) > 0 {
		raw, _ := r.counts.MarshalJSON()
		b.WriteString(`,"counts":` + string(raw))
	}
	if r.truncated {
		b.WriteString(`,"truncated":true,"total_count_is_lower_bound":true`)
	}
	for _, kv := range [][2]string{{"limit_reason", r.limitReason}, {"warning", r.warning}, {"error", r.err}} {
		if kv[1] != "" {
			b.WriteString("," + encode(kv[0]) + ":" + encode(kv[1]))
		}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] != "" {
			b.WriteString("," + encode(extra[i]) + ":" + encode(extra[i+1]))
		}
	}
	b.WriteString("}")
	return b.String()
}

// Result as JSON, its text unescaped
func result(v any) string { return tools.JSON(v) }

// Result as JSON with every character past ASCII escaped
func asciiJSON(v any) string {
	var b strings.Builder
	for _, r := range result(v) {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			b.WriteString(fmt.Sprintf(`\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff)))
		default:
			b.WriteString(fmt.Sprintf(`\u%04x`, r))
		}
	}
	return b.String()
}
