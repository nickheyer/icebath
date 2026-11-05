package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Local backend env rooted at a fresh workspace with every file tool cache reset
func newFileToolTestEnv(t *testing.T) (*Env, string) {
	t.Helper()
	ws := t.TempDir()
	env := newTerminalTestEnv(t, ws)
	env.Cwd = ws
	t.Setenv("TERMINAL_CWD", ws)
	config.ResetCache()
	ClearFileOpsCache("")
	t.Cleanup(func() { ClearFileOpsCache(""); config.ResetCache() })
	return env, ws
}

// Decodes a tool result into a generic object
func ftDecode(t *testing.T, out string) map[string]any {
	t.Helper()
	v, err := pyjson.DecodeWTF8([]byte(out))
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		t.Fatalf("not a JSON object %q: %v", out, err)
	}
	return m
}

func TestFileToolsSmokeReadWritePatch(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	p := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(p, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := ftDecode(t, readFileTool(context.Background(), env, p, nil, nil, "t1"))
	if r["content"] != "1|one\n2|two" {
		t.Fatalf("read %v", r)
	}
	w := ftDecode(t, writeFileTool(context.Background(), env, p, "one\nthree\n", "t1", false, ""))
	if w["error"] != nil {
		t.Fatalf("write %v", w)
	}
	old, nw := "three", "four"
	pr := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "replace", path: p, oldString: &old, newString: &nw}, "t1", nil))
	if pr["success"] != true {
		t.Fatalf("patch %v", pr)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one\nfour\n" {
		t.Fatalf("disk %q", b)
	}
}

// Backend answering each scripted call itself and forwarding the rest to the real local backend
type scriptedFileOps struct {
	fileToolOps
	readFileFn      func(path string, offset, limit int) *ReadResult
	readFileRawFn   func(path string) *ReadResult
	readFileBytesFn func(path string, maxBytes int) *ReadResult
	writeFileFn     func(path, content string, preContent *string) *WriteResult
	patchReplaceFn  func(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult
	patchV4AFn      func(patchContent string, probe *editProbe) *PatchResult
	searchFn        func(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult
	probeFn         func(path string) (int64, string)
	calls           []string
}

func (s *scriptedFileOps) readFile(path string, offset, limit int) *ReadResult {
	s.calls = append(s.calls, "readFile:"+path)
	if s.readFileFn != nil {
		return s.readFileFn(path, offset, limit)
	}
	return s.fileToolOps.readFile(path, offset, limit)
}

func (s *scriptedFileOps) ReadFileRaw(path string) *ReadResult {
	s.calls = append(s.calls, "ReadFileRaw:"+path)
	if s.readFileRawFn != nil {
		return s.readFileRawFn(path)
	}
	return s.fileToolOps.ReadFileRaw(path)
}

func (s *scriptedFileOps) readFileBytes(path string, maxBytes int) *ReadResult {
	s.calls = append(s.calls, "readFileBytes:"+path)
	if s.readFileBytesFn != nil {
		return s.readFileBytesFn(path, maxBytes)
	}
	return s.fileToolOps.readFileBytes(path, maxBytes)
}

func (s *scriptedFileOps) WriteFile(path, content string, preContent *string) *WriteResult {
	s.calls = append(s.calls, "WriteFile:"+path)
	if s.writeFileFn != nil {
		return s.writeFileFn(path, content, preContent)
	}
	return s.fileToolOps.WriteFile(path, content, preContent)
}

func (s *scriptedFileOps) patchReplace(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult {
	s.calls = append(s.calls, "patchReplace:"+path)
	if s.patchReplaceFn != nil {
		return s.patchReplaceFn(path, oldString, newString, replaceAll, probe)
	}
	return s.fileToolOps.patchReplace(path, oldString, newString, replaceAll, probe)
}

func (s *scriptedFileOps) patchV4A(patchContent string, probe *editProbe) *PatchResult {
	s.calls = append(s.calls, "patchV4A")
	if s.patchV4AFn != nil {
		return s.patchV4AFn(patchContent, probe)
	}
	return s.fileToolOps.patchV4A(patchContent, probe)
}

func (s *scriptedFileOps) search(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
	s.calls = append(s.calls, "search:"+path)
	if s.searchFn != nil {
		return s.searchFn(pattern, path, target, fileGlob, limit, offset, outputMode, context, order)
	}
	return s.fileToolOps.search(pattern, path, target, fileGlob, limit, offset, outputMode, context, order)
}

func (s *scriptedFileOps) probeRegularFile(path string) (int64, string) {
	s.calls = append(s.calls, "probeRegularFile:"+path)
	if s.probeFn != nil {
		return s.probeFn(path)
	}
	return s.fileToolOps.probeRegularFile(path)
}

// Routes every task's file backend to the scripted ops over the given terminal, nil terminal meaning the host
func installScriptedFileOps(t *testing.T, env *Env, ops *scriptedFileOps, terminal Environment) {
	t.Helper()
	if ops.fileToolOps == nil {
		real, err := getFileOpsShared(env, "default")
		if err != nil {
			t.Fatal(err)
		}
		ops.fileToolOps = real.ops
	}
	prev := getFileOps
	getFileOps = func(*Env, string) (*fileOpsHandle, error) {
		return &fileOpsHandle{ops: ops, terminal: terminal, cwd: "/", bind: func(context.Context) fileToolOps { return ops }}, nil
	}
	t.Cleanup(func() { getFileOps = prev })
}
