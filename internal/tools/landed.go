package tools

import (
	"encoding/json"
	"strings"
)

// What a file tool's result says it wrote
type written struct {
	Error         any      `json:"error"`
	Success       *bool    `json:"success"`
	BytesWritten  *int     `json:"bytes_written"`
	ResolvedPath  string   `json:"resolved_path"`
	FilesModified []string `json:"files_modified"`
	FilesCreated  []string `json:"files_created"`
	FilesDeleted  []string `json:"files_deleted"`
}

// Files the write of a call landed in, as its result says: those it modified, made and deleted, both ends of a move, else the one it
// resolved, else the paths the call names; none for a tool that writes no paths or a result that is no success. Paths are as the tool
// gave them
func Landed(def Def, call *Call, result string) []string {
	if def.Paths == nil || call.Args == nil {
		return nil
	}
	targets, writes := def.Paths(call.Env, call.Args)
	if !writes {
		return nil
	}
	var out written
	if json.Unmarshal([]byte(strings.TrimSpace(result)), &out) != nil || out.Error != nil && out.Error != "" {
		return nil
	}
	if out.BytesWritten == nil && (out.Success == nil || !*out.Success) {
		return nil
	}
	landed := append(append(append([]string{}, out.FilesModified...), out.FilesCreated...), out.FilesDeleted...)
	if len(landed) == 0 && out.ResolvedPath != "" {
		landed = []string{out.ResolvedPath}
	}
	if len(landed) == 0 {
		landed = targets
	}
	var paths []string
	for _, path := range landed {
		if from, to, moved := strings.Cut(path, " -> "); moved {
			paths = append(paths, strings.TrimSpace(from))
			path = to
		}
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}
