package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
)

var bootLog = slog.Default().With("logger", "icebath_cli.boot_bootstrap")

const (
	bootRecordSchemaVersion = 1
	bootLockStaleSeconds    = 600
)

// PM package reader the git selection asks first, the pm attribute hermes tests patch
var bootInstalledPackage = func(ctx context.Context, name string) (*pm.InstalledPackage, error) {
	return pm.Installed(ctx, name, false)
}

// The git to run, pm's pinned package first then PATH, empty when this machine has no usable one
func bootGitBinary(ctx context.Context) string {
	if installed, err := bootInstalledPackage(ctx, "git"); err != nil {
		bootLog.Debug(fmt.Sprintf("pm git lookup failed: %s", err.Error()))
	} else if installed != nil && installed.Binary != "" {
		return installed.Binary
	}
	found, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	return found
}

// Commit SHA of the checkout at root, asked of git, empty when git cannot answer
func ReadGitHead(ctx context.Context, root string) string {
	git := bootGitBinary(ctx)
	if git == "" {
		return ""
	}
	out, err := pysubprocess.RunWith(ctx, []string{git, "-C", root, "rev-parse", "HEAD"}, pysubprocess.Options{Timeout: 10 * time.Second})
	if err != nil || out.Returncode != 0 {
		return ""
	}
	sha := strings.TrimSpace(pystr.DecodeUTF8Replace(out.Stdout))
	if len(sha) < 7 {
		return ""
	}
	return sha
}

// What code this install is, the stamp commit for sealed trees, git HEAD for checkouts, empty for broken trees
func CurrentInstallIdentity(ctx context.Context, projectRoot string) string {
	if pathPresent(filepath.Join(projectRoot, ".git")) {
		return ReadGitHead(ctx, projectRoot)
	}
	stamp := pm.ReadInstallStamp(projectRoot)
	if stamp == nil {
		return ""
	}
	if commit, ok := stamp.Vals["commit"].(string); ok && len(commit) >= 7 {
		return commit
	}
	if tag, ok := stamp.Vals["tag"].(string); ok && tag != "" {
		return tag
	}
	return ""
}

// Per install and per profile bootstrap record under the default home's installs tree
func BootRecordPath(ctx context.Context, projectRoot string) string {
	name := profiles.GetActiveProfileName(ctx)
	if name == "" {
		name = "default"
	}
	return filepath.Join(pm.InstallStateDir(ctx, projectRoot), "bootstrap", name+".json")
}

// The last bootstrap record, empty when missing, unreadable or not an object
func ReadLastKnown(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return map[string]any{}
	}
	var data any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return map[string]any{}
	}
	if m, ok := data.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Writes the bootstrap record atomically beside its path
func writeBootRecord(path, identity string, results map[string]map[string]any) error {
	resultsObj := pyjson.NewObject()
	for name, result := range results {
		resultsObj.Set(name, result)
	}
	payload := pyjson.ObjectOf("schemaVersion", bootRecordSchemaVersion, "identity", identity,
		"bootstrappedAt", time.Now().Format("2006-01-02T15:04:05-0700"), "results", resultsObj)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	text, err := pyjson.Dumps(payload, pyjson.Indent(2))
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteBootRecord exports writeBootRecord
func WriteBootRecord(path, identity string, results map[string]map[string]any) error {
	return writeBootRecord(path, identity, results)
}

// The new identity when this install changed since its last bootstrap, empty otherwise or for a broken tree
func NeedsBootstrap(ctx context.Context, projectRoot string) string {
	identity := CurrentInstallIdentity(ctx, projectRoot)
	if identity == "" {
		return ""
	}
	if known, _ := ReadLastKnown(BootRecordPath(ctx, projectRoot))["identity"].(string); known == identity {
		return ""
	}
	return identity
}

// Existence as mutex beside a record file, a stale lock broken and retried once
type RecordLock struct {
	Path     string
	Acquired bool
}

// Lock beside a record file
func NewRecordLock(record string) *RecordLock { return &RecordLock{Path: record + ".lock"} }

func (l *RecordLock) tryCreate() bool {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o777); err != nil {
		return false
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	body, _ := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "startedAt", float64(time.Now().UnixNano())/1e9))
	f.WriteString(body)
	return true
}

// Whether the lock is older than the stale window, aged by its mtime when its body is unreadable
func (l *RecordLock) isStale() bool {
	var started float64
	raw, err := os.ReadFile(l.Path)
	parsed := false
	if err == nil {
		if text, derr := pystr.DecodeUTF8Sig(raw); derr == nil {
			var body map[string]any
			if json.Unmarshal([]byte(text), &body) == nil {
				switch v := body["startedAt"].(type) {
				case float64:
					started, parsed = v, true
				case nil:
					started, parsed = 0, true
				}
			}
		}
	}
	if !parsed {
		info, err := os.Stat(l.Path)
		if err != nil {
			return false
		}
		started = float64(info.ModTime().UnixNano()) / 1e9
	}
	return float64(time.Now().UnixNano())/1e9-started > bootLockStaleSeconds
}

// Takes the lock, a loser skipping unless the holder's lock is stale
func (l *RecordLock) Acquire() bool {
	if l.tryCreate() {
		l.Acquired = true
		return true
	}
	if l.isStale() {
		if err := os.Remove(l.Path); err != nil {
			return false
		}
		if l.tryCreate() {
			l.Acquired = true
			return true
		}
	}
	return false
}

// Drops the lock this process took
func (l *RecordLock) Release() {
	if !l.Acquired {
		return
	}
	os.Remove(l.Path)
	l.Acquired = false
}

// Lock taker of the boot bootstrap, the method hermes tests patch
var recordLockAcquire = func(l *RecordLock) bool { return l.Acquire() }

// Bounds home maintenance to one attempt per installed revision
func RunBootBootstrap(ctx context.Context, projectRoot string) (map[string]any, error) {
	identity := NeedsBootstrap(ctx, projectRoot)
	if identity == "" {
		return map[string]any{"home": "skipped"}, nil
	}
	record := BootRecordPath(ctx, projectRoot)
	lock := NewRecordLock(record)
	if !recordLockAcquire(lock) {
		return map[string]any{"home": "lost-race"}, nil
	}
	defer lock.Release()
	if known, _ := ReadLastKnown(record)["identity"].(string); known == identity {
		return map[string]any{"home": "done-by-other"}, nil
	}
	bootLog.Info(fmt.Sprintf("home maintenance: code changed to %s, running steps", identity[:min(12, len(identity))]))
	results := RunSteps(ctx, BootHomeSteps)
	if err := writeBootRecord(record, identity, results); err != nil {
		return nil, err
	}
	return map[string]any{"home": results}, nil
}

// Boot bootstrap runner, the module attribute hermes tests patch
var runBootBootstrapFn = RunBootBootstrap

// The one call boot paths use, a bootstrap problem logged and never stopping the launch
func MaybeRunBootBootstrap(ctx context.Context, projectRoot string) {
	defer func() {
		if r := recover(); r != nil {
			bootLog.Warn(fmt.Sprintf("boot bootstrap failed (continuing boot): %v", r))
		}
	}()
	if _, err := runBootBootstrapFn(ctx, projectRoot); err != nil {
		bootLog.Warn(fmt.Sprintf("boot bootstrap failed (continuing boot): %s", errorText(err)))
	}
}

// Message of an error without a Go wrapper prefix
func errorText(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return err.Error()
	}
	return err.Error()
}
