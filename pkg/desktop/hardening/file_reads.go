package hardening

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"syscall"
)

const (
	// Byte cap of an attachment read for a remote upload
	AttachmentUploadDefaultMaxBytes = 256 * 1024 * 1024
	// Byte cap of a text preview source file before its preview slice is read
	TextPreviewSourceMaxBytes = 64 * 1024 * 1024
)

// Node error.code of err, empty when the error carries none
func ErrorCode(err error) string {
	var pe *IPCPathError
	if errors.As(err, &pe) {
		return pe.Code
	}
	var ne *nodeError
	if errors.As(err, &ne) {
		return ne.code
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var errno syscall.Errno
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &errno) {
		return NodeCode(err)
	}
	return ""
}

// True when a read failed because the file or its parent directory is not on disk
func IsMissingFileError(err error) bool {
	if err == nil {
		return false
	}
	code := ErrorCode(err)
	return code == "ENOENT" || code == "ENOTDIR"
}

// Structured answer a read handler gives for a file that is not on disk
type MissingFile struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// Not on disk answer for the requested path built from the caught read error
func MissingFileResult(requestedPath string, err error) MissingFile {
	out := MissingFile{OK: false, Error: ErrorCode(err), Message: "File does not exist.", Path: requestedPath}
	if out.Error == "" {
		out.Error = "ENOENT"
	}
	if err != nil {
		out.Message = err.Error()
	}
	return out
}

// Checked file read whole as a base64 data URL of mimeType
func ReadFileDataURLForIpc(filePath any, o ReadableFileOptions, mimeType string) (string, error) {
	rf, err := ResolveReadableFileForIpc(filePath, o)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(rf.ResolvedPath)
	if err != nil {
		return "", NodeErr(err)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
