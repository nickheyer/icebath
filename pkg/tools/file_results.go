package tools

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// ReadResult is one file read with the key order and omission rules the model reads
type ReadResult struct {
	Content        string   `json:"content"`
	TotalLines     int      `json:"total_lines"`
	FileSize       int64    `json:"file_size"`
	Truncated      bool     `json:"truncated"`
	TruncatedLines *bool    `json:"truncated_lines,omitempty"`
	Hint           string   `json:"hint,omitempty"`
	IsBinary       bool     `json:"is_binary"`
	IsImage        bool     `json:"is_image"`
	Base64Content  string   `json:"base64_content,omitempty"`
	MimeType       string   `json:"mime_type,omitempty"`
	Dimensions     string   `json:"dimensions,omitempty"`
	Error          string   `json:"error,omitempty"`
	NotFound       bool     `json:"not_found"`
	SimilarFiles   []string `json:"similar_files,omitempty"`
	// Sha256 of the exact on disk bytes a raw read decoded, nil when unknown
	ContentSha256 *string `json:"-"`
}

// WriteResult is one file write with its verification, lint and LSP diagnostics
type WriteResult struct {
	BytesWritten   int            `json:"bytes_written"`
	DirsCreated    bool           `json:"dirs_created"`
	Verified       *bool          `json:"verified,omitempty"`
	Lint           *pyjson.Object `json:"lint,omitempty"`
	LSPDiagnostics string         `json:"lsp_diagnostics,omitempty"`
	Error          string         `json:"error,omitempty"`
	Warning        string         `json:"warning,omitempty"`
	// Sha256 of the bytes written, nil when the backend could not hash them
	ContentSha256 *string `json:"-"`
}

// PatchResult is one replace or V4A patch outcome in its pinned emission order
type PatchResult struct {
	Success        bool           `json:"success"`
	NoChange       bool           `json:"no_change,omitempty"`
	Note           string         `json:"note,omitempty"`
	Diff           string         `json:"diff,omitempty"`
	FilesModified  []string       `json:"files_modified,omitempty"`
	FilesCreated   []string       `json:"files_created,omitempty"`
	FilesDeleted   []string       `json:"files_deleted,omitempty"`
	Lint           *pyjson.Object `json:"lint,omitempty"`
	LSPDiagnostics string         `json:"lsp_diagnostics,omitempty"`
	Error          string         `json:"error,omitempty"`
	// One entry per file write with the sha of the bytes read and the bytes written
	Writes []patchWrite `json:"-"`
}

// Pointer to a copy of the string for optional sha fields
func shaRef(s string) *string { return &s }
