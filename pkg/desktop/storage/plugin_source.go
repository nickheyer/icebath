package storage

import (
	"encoding/json"
	"os"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

const pluginSourceMaxBytes = 16 * 1024 * 1024

// Answers readPluginSource with the checked file's text, size and path
func (s *Service) ReadPluginSource(filePath json.RawMessage) (*Object, error) {
	requested, _ := jsParse(string(filePath))
	rf, err := hardening.ResolveReadableFileForIpc(requested, hardening.ReadableFileOptions{MaxBytes: pluginSourceMaxBytes, Purpose: "Plugin source"})
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(rf.ResolvedPath)
	if err != nil {
		return nil, hardening.NodeErr(err)
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	out := NewObject()
	out.Set("byteSize", float64(rf.Stat.Size()))
	out.Set("path", rf.ResolvedPath)
	out.Set("text", text)
	out.Set("truncated", false)
	return out, nil
}
