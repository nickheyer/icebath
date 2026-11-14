package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	startTimeDriftTolerance = 200
	terminateGrace          = 15 * time.Second
	orphanStopWait          = 5 * time.Second
)

var modernStateFields = []string{"create_time", "executable", "owner_pid", "owner_create_time"}

type serverRecord map[string]any

func (r *Runtime) StatePath() string { return home.LlamaCppServer.In(r.root) }

func decodeRecord(data []byte) (serverRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("server state is not an object")
	}
	return serverRecord(m), nil
}

func (r *Runtime) readState() serverRecord {
	data, err := os.ReadFile(r.StatePath())
	if err != nil {
		return serverRecord{}
	}
	state, err := decodeRecord(data)
	if err != nil {
		return serverRecord{}
	}
	return state
}

func (s serverRecord) modern() bool {
	for _, key := range modernStateFields {
		if _, ok := s[key]; ok {
			return true
		}
	}
	return false
}

func (s serverRecord) text(key string) (string, bool) {
	v, ok := s[key].(string)
	return v, ok
}

func recordPID(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	pid, err := strconv.Atoi(string(n))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func recordBirth(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f <= 0 {
		return 0, false
	}
	return f, true
}

func sameBirth(pid int, state serverRecord, prefix string) bool {
	recorded, ok := recordBirth(state[prefix+"start_time"])
	if !ok {
		created, err := proc.Started(pid)
		if err != nil {
			return false
		}
		want, ok := recordBirth(state[prefix+"create_time"])
		return ok && created == want
	}
	current, err := proc.Fingerprint(pid)
	if err != nil {
		return false
	}
	diff := current - int64(recorded)
	return diff <= startTimeDriftTolerance && diff >= -startTimeDriftTolerance
}

func liveParent(pid int) (int, bool) {
	ppid, err := proc.ParentOf(pid)
	if err != nil || ppid <= 0 || !proc.Exists(ppid) {
		return 0, false
	}
	created, err := proc.Started(pid)
	if err != nil {
		return 0, false
	}
	parentCreated, err := proc.Started(ppid)
	if err != nil || parentCreated > created {
		return 0, false
	}
	return ppid, true
}

func recordedProcess(state serverRecord) (proc.Identity, bool) {
	pid, okPID := recordPID(state["pid"])
	created, okCreated := recordBirth(state["create_time"])
	ownerPID, okOwner := recordPID(state["owner_pid"])
	ownerCreated, okOwnerCreated := recordBirth(state["owner_create_time"])
	exe, okExe := state.text("executable")
	if !okPID || !okCreated || !okOwner || !okOwnerCreated || ownerCreated > created || !okExe || exe == "" {
		return proc.Identity{}, false
	}
	if !proc.Exists(pid) || !sameBirth(pid, state, "") {
		return proc.Identity{}, false
	}
	actual, err := proc.Exe(pid)
	if err != nil || filepath.Clean(actual) != filepath.Clean(exe) {
		return proc.Identity{}, false
	}
	if runtime.GOOS == "windows" {
		if ppid, err := proc.ParentOf(pid); err != nil || ppid != ownerPID {
			return proc.Identity{}, false
		}
	}
	if parent, ok := liveParent(pid); ok {
		if parent == ownerPID {
			if !sameBirth(parent, state, "owner_") {
				return proc.Identity{}, false
			}
		} else if runtime.GOOS == "windows" || !ownerIsDead(state) {
			return proc.Identity{}, false
		}
	}
	id, err := proc.Identify(pid)
	if err != nil {
		return proc.Identity{}, false
	}
	return id, true
}

func ownerIsDead(state serverRecord) bool {
	pid, okPID := recordPID(state["owner_pid"])
	created, okCreated := recordBirth(state["owner_create_time"])
	if !okPID || !okCreated {
		return false
	}
	if !proc.Exists(pid) {
		return true
	}
	if _, ok := recordBirth(state["owner_start_time"]); ok {
		return !sameBirth(pid, state, "owner_")
	}
	started, err := proc.Started(pid)
	if err != nil {
		return errors.Is(err, proc.ErrNotFound)
	}
	return started > created
}

func flagValue(argv []string, flag string) (string, bool) {
	index, count := -1, 0
	for i, a := range argv {
		if a == flag {
			count++
			if index < 0 {
				index = i
			}
		}
	}
	if count != 1 || index+1 >= len(argv) {
		return "", false
	}
	return argv[index+1], true
}

func (r *Runtime) legacyRecordedProcess(state serverRecord) (proc.Identity, string, bool) {
	if state.modern() {
		return proc.Identity{}, "", false
	}
	pid, ok := recordPID(state["pid"])
	if !ok {
		return proc.Identity{}, "", false
	}
	exe, err := proc.Exe(pid)
	if err != nil || exe == "" {
		return proc.Identity{}, "", false
	}
	root := filepath.Dir(r.StatePath())
	relative, err := filepath.Rel(root, exe)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return proc.Identity{}, "", false
	}
	if name := strings.ToLower(filepath.Base(exe)); name != "llama-server" && name != "llama-server.exe" {
		return proc.Identity{}, "", false
	}
	created, err := proc.Started(pid)
	if err != nil {
		return proc.Identity{}, "", false
	}
	fi, err := os.Stat(r.StatePath())
	if err != nil || created > float64(fi.ModTime().UnixNano())/1e9 {
		return proc.Identity{}, "", false
	}
	baseURL, _ := state.text("base_url")
	apiKey, okKey := state.text("api_key")
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "0" || !okKey || apiKey == "" {
		return proc.Identity{}, "", false
	}
	argv, err := proc.Cmdline(pid)
	if err != nil {
		return proc.Identity{}, "", false
	}
	port, _ := strconv.Atoi(u.Port())
	host, okHost := flagValue(argv, "--host")
	argPort, okPort := flagValue(argv, "--port")
	key, okArgKey := flagValue(argv, "--api-key")
	if !okHost || host != "127.0.0.1" || !okPort || argPort != strconv.Itoa(port) || !okArgKey || key != apiKey {
		return proc.Identity{}, "", false
	}
	preset, hasPreset := flagValue(argv, "--models-preset")
	directory, hasDirectory := flagValue(argv, "--models-dir")
	presetMatches := hasPreset && preset != "" && filepath.Clean(preset) == filepath.Join(root, "presets.ini") && !hasDirectory
	dirMatches := hasDirectory && directory != "" && filepath.Clean(directory) == filepath.Clean(r.ModelsDir()) && !hasPreset
	if !presetMatches && !dirMatches {
		return proc.Identity{}, "", false
	}
	id, err := proc.Identify(pid)
	if err != nil {
		return proc.Identity{}, "", false
	}
	return id, relative, true
}

func (r *Runtime) legacyOrphanProcess(state serverRecord) (proc.Identity, bool) {
	if runtime.GOOS != "windows" {
		return proc.Identity{}, false
	}
	id, relative, ok := r.legacyRecordedProcess(state)
	if !ok {
		return proc.Identity{}, false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 3 || strings.ToLower(parts[2]) != "llama-server.exe" {
		return proc.Identity{}, false
	}
	if _, hasParent := liveParent(id.PID); hasParent {
		return proc.Identity{}, false
	}
	if ppid, err := proc.ParentOf(id.PID); err != nil || ppid <= 0 {
		return proc.Identity{}, false
	}
	return id, true
}

func terminateTree(ctx context.Context, root proc.Identity, verified bool) error {
	var children []proc.Identity
	if table, err := proc.ReadTable(); err == nil {
		children = table.Descendants(root)
	}
	if verified && !root.Running() {
		return fmt.Errorf("pid %d: %w", root.PID, proc.ErrNotFound)
	}
	err := proc.KillTree(ctx, root, terminateGrace)
	for _, child := range children {
		if child.Running() {
			_ = child.Signal(syscall.SIGKILL)
		}
	}
	if err != nil && !errors.Is(err, proc.ErrNotFound) {
		return err
	}
	return nil
}

func (r *Runtime) StopRecordedOrphan(ctx context.Context) bool {
	data, err := os.ReadFile(r.StatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	state, err := decodeRecord(data)
	if err != nil {
		return false
	}
	pid, ok := recordPID(state["pid"])
	if !ok {
		return false
	}
	if !proc.Exists(pid) {
		return true
	}
	var id proc.Identity
	if state.modern() {
		id, ok = recordedProcess(state)
		if !ok || !ownerIsDead(state) {
			return false
		}
	} else {
		id, ok = r.legacyOrphanProcess(state)
	}
	if !ok || !reflect.DeepEqual(r.readState(), state) || !id.Running() {
		return false
	}
	if err := terminateTree(ctx, id, true); err != nil {
		slog.Warn("could not verify or stop recorded managed llama-server")
		return false
	}
	waitCtx, cancel := context.WithTimeout(ctx, orphanStopWait)
	defer cancel()
	if err := id.Wait(waitCtx); err != nil {
		slog.Warn("could not verify or stop recorded managed llama-server")
		return false
	}
	slog.Info(fmt.Sprintf("stopped orphaned managed llama-server pid=%d", id.PID))
	return true
}
