package file

import (
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
)

type Object []tool.Field

func (o Object) MarshalJSON() ([]byte, error) {
	members := make([]wire.Member, 0, len(o))
	for _, f := range o {
		value, err := fileutil.EncodeJSON(f.Value, fileutil.JSONStyle{Tight: true})
		if err != nil {
			return nil, err
		}
		members = append(members, wire.Member{Key: f.Key, Value: value})
	}
	return wire.Object(members), nil
}

type Version struct {
	Device       uint64
	Inode        uint64
	Size         int64
	ModTimeNs    int64
	ChangeTimeNs int64
	Digest       [32]byte
}

type ReadResult struct {
	Content        string
	TotalLines     int
	FileSize       int64
	Truncated      bool
	TruncatedLines bool
	Hint           string
	IsBinary       bool
	IsImage        bool
	Base64Content  *string
	Error          string
	NotFound       bool
	SimilarFiles   []string
	Version        *Version
	ContentSHA256  string
}

func (r ReadResult) Fields() []tool.Field {
	out := []tool.Field{
		{Key: "content", Value: r.Content},
		{Key: "total_lines", Value: r.TotalLines},
		{Key: "file_size", Value: r.FileSize},
		{Key: "truncated", Value: r.Truncated},
	}
	if r.TruncatedLines {
		out = append(out, tool.Field{Key: "truncated_lines", Value: true})
	}
	if r.Hint != "" {
		out = append(out, tool.Field{Key: "hint", Value: r.Hint})
	}
	out = append(out, tool.Field{Key: "is_binary", Value: r.IsBinary}, tool.Field{Key: "is_image", Value: r.IsImage})
	if r.Base64Content != nil {
		out = append(out, tool.Field{Key: "base64_content", Value: *r.Base64Content})
	}
	if r.Error != "" {
		out = append(out, tool.Field{Key: "error", Value: r.Error})
	}
	out = append(out, tool.Field{Key: "not_found", Value: r.NotFound})
	if len(r.SimilarFiles) > 0 {
		out = append(out, tool.Field{Key: "similar_files", Value: r.SimilarFiles})
	}
	return out
}

type LintResult struct {
	Success bool
	Skipped bool
	Output  string
	Message string
}

func (l LintResult) Fields() []tool.Field {
	if l.Skipped {
		return []tool.Field{{Key: "status", Value: "skipped"}, {Key: "message", Value: l.Message}}
	}
	status := "error"
	if l.Success {
		status = "ok"
	}
	out := []tool.Field{{Key: "status", Value: status}, {Key: "output", Value: l.Output}}
	if l.Message != "" {
		out = append(out, tool.Field{Key: "message", Value: l.Message})
	}
	return out
}

func (l LintResult) object() Object {
	return Object(l.Fields())
}

type WriteResult struct {
	BytesWritten   int
	DirsCreated    bool
	Verified       bool
	ContentSHA256  string
	Lint           *LintResult
	LSPDiagnostics string
	Error          string
	Warning        string
}

func (w WriteResult) Fields() []tool.Field {
	out := []tool.Field{
		{Key: "bytes_written", Value: w.BytesWritten},
		{Key: "dirs_created", Value: w.DirsCreated},
	}
	if w.Verified {
		out = append(out, tool.Field{Key: "verified", Value: true})
	}
	if w.Lint != nil {
		out = append(out, tool.Field{Key: "lint", Value: w.Lint.object()})
	}
	if w.LSPDiagnostics != "" {
		out = append(out, tool.Field{Key: "lsp_diagnostics", Value: w.LSPDiagnostics})
	}
	if w.Error != "" {
		out = append(out, tool.Field{Key: "error", Value: w.Error})
	}
	if w.Warning != "" {
		out = append(out, tool.Field{Key: "warning", Value: w.Warning})
	}
	return out
}

type SearchMatch struct {
	Path    string
	Line    int
	Content string
}

type PathCount struct {
	Path  string
	Count int
}

type SearchResult struct {
	Matches        []SearchMatch
	Files          []string
	Counts         []PathCount
	TotalCount     int
	Truncated      bool
	LimitReason    string
	Warning        string
	Error          string
	SkippedPaths   []string
	SkippedOmitted int
}

const densifyMinMatches = 5

func (s SearchResult) dense() (string, bool) {
	if len(s.Matches) < densifyMinMatches {
		return "", false
	}
	var lines []string
	current, started := "", false
	for _, m := range s.Matches {
		if !started || m.Path != current {
			lines = append(lines, m.Path)
			current, started = m.Path, true
		}
		lines = append(lines, "  "+strconv.Itoa(m.Line)+": "+strings.TrimRightFunc(m.Content, isSpace))
	}
	return strings.Join(lines, "\n"), true
}

func (s SearchResult) Fields(densify bool) []tool.Field {
	out := []tool.Field{{Key: "total_count", Value: s.TotalCount}}
	if len(s.Matches) > 0 {
		text, dense := "", false
		if densify {
			text, dense = s.dense()
		}
		if dense {
			out = append(out,
				tool.Field{Key: "matches_format", Value: "path-grouped: each file path on its own line, followed by " +
					"indented '<line>: <content>' rows for matches in that file"},
				tool.Field{Key: "matches_text", Value: text})
		} else {
			rows := make([]Object, 0, len(s.Matches))
			for _, m := range s.Matches {
				rows = append(rows, Object{{Key: "path", Value: m.Path}, {Key: "line", Value: m.Line}, {Key: "content", Value: m.Content}})
			}
			out = append(out, tool.Field{Key: "matches", Value: rows})
		}
	}
	if len(s.Files) > 0 {
		out = append(out, tool.Field{Key: "files", Value: s.Files})
	}
	if len(s.Counts) > 0 {
		counts := make(Object, 0, len(s.Counts))
		for _, c := range s.Counts {
			counts = append(counts, tool.Field{Key: c.Path, Value: c.Count})
		}
		out = append(out, tool.Field{Key: "counts", Value: counts})
	}
	if s.Truncated {
		out = append(out, tool.Field{Key: "truncated", Value: true}, tool.Field{Key: "total_count_is_lower_bound", Value: true})
	}
	if s.LimitReason != "" {
		out = append(out, tool.Field{Key: "limit_reason", Value: s.LimitReason})
	}
	if s.Warning != "" {
		out = append(out, tool.Field{Key: "warning", Value: s.Warning})
	}
	if s.Error != "" {
		out = append(out, tool.Field{Key: "error", Value: s.Error})
	}
	return out
}

type PatchWrite struct {
	Path          string
	ReadSHA256    string
	ReadKnown     bool
	WrittenSHA256 string
}

type FileLint struct {
	Path string
	Lint LintResult
}

type PatchResult struct {
	Success        bool
	Diff           string
	FilesModified  []string
	FilesCreated   []string
	FilesDeleted   []string
	Lint           *LintResult
	LintByFile     []FileLint
	LSPDiagnostics string
	Error          string
	NoChange       bool
	Note           string
	Writes         []PatchWrite
}

func (p PatchResult) Fields() []tool.Field {
	out := []tool.Field{{Key: "success", Value: p.Success}}
	if p.NoChange {
		out = append(out, tool.Field{Key: "no_change", Value: true})
	}
	if p.Note != "" {
		out = append(out, tool.Field{Key: "note", Value: p.Note})
	}
	if p.Diff != "" {
		out = append(out, tool.Field{Key: "diff", Value: p.Diff})
	}
	if len(p.FilesModified) > 0 {
		out = append(out, tool.Field{Key: "files_modified", Value: p.FilesModified})
	}
	if len(p.FilesCreated) > 0 {
		out = append(out, tool.Field{Key: "files_created", Value: p.FilesCreated})
	}
	if len(p.FilesDeleted) > 0 {
		out = append(out, tool.Field{Key: "files_deleted", Value: p.FilesDeleted})
	}
	switch {
	case p.Lint != nil:
		out = append(out, tool.Field{Key: "lint", Value: p.Lint.object()})
	case len(p.LintByFile) > 0:
		byFile := make([]tool.Field, 0, len(p.LintByFile))
		for _, fl := range p.LintByFile {
			byFile = append(byFile, tool.Field{Key: fl.Path, Value: fl.Lint.object()})
		}
		out = append(out, tool.Field{Key: "lint", Value: Object(byFile)})
	}
	if p.LSPDiagnostics != "" {
		out = append(out, tool.Field{Key: "lsp_diagnostics", Value: p.LSPDiagnostics})
	}
	if p.Error != "" {
		out = append(out, tool.Field{Key: "error", Value: p.Error})
	}
	return out
}
