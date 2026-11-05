package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	modal "github.com/modal-labs/modal-client/go"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Modal SDK stdin chunk size, under the 2 MB legacy server cap and flushed per chunk
const modalStdinChunkSize = 1024 * 1024

// Per profile snapshot store, resolved per call so a multiplexed gateway keeps every profile's snapshots in its own home
func modalSnapshotStore(env *Env) string { return filepath.Join(env.Home, "modal_snapshots.json") }

// Snapshot id for a task and whether it came from the legacy bare key, the namespaced key winning
func modalSnapshotRestoreCandidate(env *Env, taskID string) (string, bool) {
	snapshots := envLoadJSONStore(modalSnapshotStore(env))
	for _, entry := range []struct {
		key    string
		legacy bool
	}{{"direct:" + taskID, false}, {taskID, true}} {
		if id, ok := snapshots[entry.key].(string); ok && id != "" {
			return id, entry.legacy
		}
	}
	return "", false
}

// Records the namespaced snapshot id and drops the legacy key
func modalStoreDirectSnapshot(env *Env, taskID, snapshotID string) error {
	return envUpdateJSONStore(modalSnapshotStore(env), func(snapshots map[string]any) {
		snapshots["direct:"+taskID] = snapshotID
		delete(snapshots, taskID)
	})
}

// Drops both keys of a task when they hold the snapshot id, or any id when none is given
func modalDeleteDirectSnapshot(env *Env, taskID, snapshotID string) error {
	return envUpdateJSONStore(modalSnapshotStore(env), func(snapshots map[string]any) {
		for _, key := range []string{"direct:" + taskID, taskID} {
			if current, ok := snapshots[key]; ok && current != nil && (snapshotID == "" || current == snapshotID) {
				delete(snapshots, key)
			}
		}
	})
}

// Registry references become images with pip repaired before bootstrap, snapshot ids load by id
func resolveModalImage(ctx context.Context, mc *modal.Client, spec string) (*modal.Image, error) {
	if strings.HasPrefix(spec, "im-") {
		return mc.Images.FromID(ctx, spec, nil)
	}
	setup := []string{"RUN rm -rf /usr/local/lib/python*/site-packages/pip* 2>/dev/null; " +
		"python -m ensurepip --upgrade --default-pip 2>/dev/null || true"}
	lower := strings.ToLower(spec)
	if strings.Contains(lower, "ubuntu") || strings.Contains(lower, "debian") {
		setup = append([]string{"RUN apt-get update -qq && apt-get install -y -qq python3 python3-venv > /dev/null 2>&1 || true"}, setup...)
	}
	return mc.Images.FromRegistry(spec, nil).DockerfileCommands(setup, nil), nil
}

// Writes the payload in chunks then closes stdin
func modalStreamStdin(w io.WriteCloser, data []byte) error {
	for offset := 0; offset < len(data); offset += modalStdinChunkSize {
		end := offset + modalStdinChunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[offset:end]); err != nil {
			return err
		}
	}
	return w.Close()
}

// modalEnv runs commands in a Modal sandbox created through the native SDK, with filesystem snapshots across sessions
type modalEnv struct {
	baseEnv
	client      *modal.Client
	app         *modal.App
	sandbox     *modal.Sandbox
	persistent  bool
	taskID      string
	syncManager *FileSyncManager
}

// ModalOptions are the direct Modal sandbox settings
type ModalOptions struct {
	Image         string
	Cwd           string
	Timeout       int
	SandboxKwargs map[string]any
	Persistent    bool
	TaskID        string
}

// Sandbox create params from the kwargs, cpu and memory only when positive, timeout defaulting to an hour
func modalCreateParams(kwargs map[string]any) *modal.SandboxCreateParams {
	params := &modal.SandboxCreateParams{Command: []string{"sleep", "infinity"}, Timeout: time.Hour}
	if v, ok := kwargs["cpu"]; ok {
		if f, ok := pyFloatValue(v); ok && f > 0 {
			params.CPU = f
		}
	}
	if v, ok := kwargs["memory"]; ok {
		if n, ok := pyIntValue(v); ok && n > 0 {
			params.MemoryMiB = n
		}
	}
	if v, ok := kwargs["timeout"]; ok {
		if n, ok := pyIntValue(v); ok {
			params.Timeout = time.Duration(n) * time.Second
		}
	}
	return params
}

// Float the way Python float() reads a JSON value
func pyFloatValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// NewModalEnvironment creates the sandbox, restoring a stored snapshot when persistent, and seeds the sync
func NewModalEnvironment(ctx context.Context, env *Env, o ModalOptions) (*modalEnv, error) {
	if o.Cwd == "" {
		o.Cwd = "/root"
	}
	if o.Timeout == 0 {
		o.Timeout = 60
	}
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	m := &modalEnv{persistent: o.Persistent, taskID: o.TaskID}
	m.stdinMode = "payload"
	m.snapshotTimeout = 60
	initBaseEnv(&m.baseEnv, m, env, o.Cwd, o.Timeout, nil, true)
	restored, legacy := "", false
	if o.Persistent {
		restored, legacy = modalSnapshotRestoreCandidate(env, o.TaskID)
	}
	if restored != "" {
		envLog(env).Info("Modal: restoring from snapshot", "snapshot", restored[:min(20, len(restored))])
	}
	mc, err := modal.NewClient()
	if err != nil {
		return nil, err
	}
	m.client = mc
	create := func(spec string) error {
		image, err := resolveModalImage(ctx, mc, spec)
		if err != nil {
			return err
		}
		createCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
		defer cancel()
		app, err := mc.Apps.FromName(createCtx, "icebath-agent", &modal.AppFromNameParams{CreateIfMissing: true})
		if err != nil {
			return err
		}
		sb, err := mc.Sandboxes.Create(createCtx, app, image, modalCreateParams(o.SandboxKwargs))
		if err != nil {
			return err
		}
		m.app, m.sandbox = app, sb
		return nil
	}
	spec := o.Image
	if restored != "" {
		spec = restored
	}
	if err := create(spec); err != nil {
		if restored == "" {
			mc.Close()
			return nil, err
		}
		envLog(env).Warn("Modal: failed to restore snapshot, retrying with base image", "snapshot", restored[:min(20, len(restored))], "error", err)
		modalDeleteDirectSnapshot(env, o.TaskID, restored)
		if err := create(o.Image); err != nil {
			mc.Close()
			return nil, err
		}
	} else if restored != "" && legacy {
		modalStoreDirectSnapshot(env, o.TaskID, restored)
	}
	envLog(env).Info("Modal: sandbox created", "task", o.TaskID)
	m.syncManager = NewFileSyncManager(env, func() []syncFile { return iterSyncFiles(env, "/root/.icebath") },
		func(h, r string) error { return m.modalUpload(ctx, h, r) }, func(paths []string) error { return m.modalDelete(ctx, paths) },
		func(files []syncFile) error { return m.modalBulkUpload(ctx, files) }, func(dest string) error { return m.modalBulkDownload(ctx, dest) })
	m.syncManager.Sync(true)
	m.initSession(ctx)
	return m, nil
}

func (m *modalEnv) EnvType() string { return "modal" }

// Runs bash -c cmd in the sandbox with optional streamed stdin, returning stdout when capturing
func (m *modalEnv) exec(ctx context.Context, cmd string, timeout time.Duration, stdin *string, failLabel string, capture bool) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	proc, err := m.sandbox.Exec(runCtx, []string{"bash", "-c", cmd}, &modal.SandboxExecParams{Stdout: modal.Pipe, Stderr: modal.Pipe})
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		payload, err := pystr.SurrogateEscapeEncode(*stdin)
		if err != nil {
			return nil, err
		}
		if err := modalStreamStdin(proc.Stdin, payload); err != nil {
			return nil, err
		}
	} else {
		proc.Stdin.Close()
	}
	var data []byte
	if capture {
		data, err = io.ReadAll(proc.Stdout)
		if err != nil {
			return nil, err
		}
	}
	code, err := proc.Wait(runCtx, nil)
	if err != nil {
		return nil, err
	}
	if failLabel != "" && code != 0 {
		detail := ""
		if !capture {
			stderr, _ := io.ReadAll(proc.Stderr)
			detail = ": " + string(stderr)
		}
		return nil, fmt.Errorf("Modal %s failed (exit %d)%s", failLabel, code, detail)
	}
	return data, nil
}

// Uploads a single file as base64 piped through stdin
func (m *modalEnv) modalUpload(ctx context.Context, hostPath, remotePath string) error {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	payload := base64.StdEncoding.EncodeToString(data)
	cmd := "mkdir -p " + shQuote(path.Dir(remotePath)) + " && base64 -d > " + shQuote(remotePath)
	_, err = m.exec(ctx, cmd, 30*time.Second, &payload, "", false)
	return err
}

// Uploads many files as one in memory gzipped tar streamed through stdin
func (m *modalEnv) modalBulkUpload(ctx context.Context, files []syncFile) error {
	if len(files) == 0 {
		return nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := addFileToTar(tw, f.host, strings.TrimLeft(f.remote, "/")); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	payload := base64.StdEncoding.EncodeToString(buf.Bytes())
	cmd := quotedMkdirCommand(uniqueParentDirs(files)) + " && base64 -d | tar xzf - -C /"
	_, err := m.exec(ctx, cmd, 120*time.Second, &payload, "bulk upload", false)
	return err
}

// Adds one host file to a tar under the archive name
func addFileToTar(tw *tar.Writer, hostPath, arcname string) error {
	st, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(st, "")
	if err != nil {
		return err
	}
	hdr.Name = arcname
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// Downloads the remote icebath tree as a tar archive, sandboxes running as root
func (m *modalEnv) modalBulkDownload(ctx context.Context, dest string) error {
	data, err := m.exec(ctx, "tar cf - --exclude='*.sock' -C / root/.icebath", 120*time.Second, nil, "bulk download", true)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}

func (m *modalEnv) modalDelete(ctx context.Context, remotePaths []string) error {
	_, err := m.exec(ctx, quotedRmCommand(remotePaths), 15*time.Second, nil, "", false)
	return err
}

func (m *modalEnv) beforeExecute(ctx context.Context) error {
	m.syncManager.Sync(false)
	return nil
}

// Spawn per call through a threaded handle whose cancel terminates the sandbox
func (m *modalEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	sandbox := m.sandbox
	cancel := func() {
		termCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		sandbox.Terminate(termCtx, nil)
	}
	execFn := func() (string, int) {
		runCtx, done := context.WithTimeout(context.Background(), time.Duration(timeout+30)*time.Second)
		defer done()
		proc, err := sandbox.Exec(runCtx, bashArgv(cmd, login), &modal.SandboxExecParams{Stdout: modal.Pipe, Stderr: modal.Pipe, Timeout: time.Duration(timeout) * time.Second})
		if err != nil {
			return "Modal exec failed: " + err.Error(), 1
		}
		if stdinData != nil {
			payload, err := pystr.SurrogateEscapeEncode(*stdinData)
			if err != nil {
				return "", 1
			}
			if err := modalStreamStdin(proc.Stdin, payload); err != nil {
				return "Modal stdin failed: " + err.Error(), 1
			}
		} else {
			proc.Stdin.Close()
		}
		stdout, _ := io.ReadAll(proc.Stdout)
		stderr, _ := io.ReadAll(proc.Stderr)
		code, err := proc.Wait(runCtx, nil)
		if err != nil {
			return "Modal exec failed: " + err.Error(), 1
		}
		var parts []string
		for _, part := range []string{strings.ToValidUTF8(string(stdout), "�"), strings.ToValidUTF8(string(stderr), "�")} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n"), code
	}
	return NewThreadedProcessHandle(execFn, cancel)
}

// Cleanup snapshots the filesystem when persistent then stops the sandbox
func (m *modalEnv) Cleanup(forceRemove bool) error {
	if m.sandbox == nil {
		return nil
	}
	if m.syncManager != nil {
		envLog(m.env).Info("Modal: syncing files from sandbox...")
		m.syncManager.SyncBack(m.env.Home)
	}
	if m.persistent {
		snapCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		image, err := m.sandbox.SnapshotFilesystem(snapCtx, &modal.SandboxSnapshotFilesystemParams{TTL: modal.NoExpiryTTL, Timeout: 60 * time.Second})
		cancel()
		if err == nil && image != nil && image.ImageID != "" {
			if err := modalStoreDirectSnapshot(m.env, m.taskID, image.ImageID); err != nil {
				envLog(m.env).Warn("Modal: filesystem snapshot failed", "error", err)
			} else {
				envLog(m.env).Info("Modal: saved filesystem snapshot", "snapshot", image.ImageID[:min(20, len(image.ImageID))], "task", m.taskID)
			}
		}
	}
	termCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	m.sandbox.Terminate(termCtx, nil)
	cancel()
	m.client.Close()
	m.sandbox, m.app = nil, nil
	return nil
}

// ---- provider -----------------------------------------------------------------------------------

type modalBackend struct{}

func (modalBackend) Name() string                           { return "modal" }
func (modalBackend) DisplayName() string                    { return "Modal" }
func (modalBackend) Description() string                    { return "Serverless cloud sandbox" }
func (modalBackend) EnvDescription() string                 { return "a Modal sandbox (Linux)" }
func (modalBackend) IsRemote() bool                         { return true }
func (modalBackend) IsContainer() bool                      { return true }
func (modalBackend) SkipContainerGuards() bool              { return true }
func (modalBackend) CachePathBase() string                  { return DefaultContainerBase }
func (modalBackend) DefaultCwd() string                     { return "" }
func (modalBackend) StripEnvKeys() []string                 { return nil }
func (modalBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (modalBackend) SupportsStreams() bool                  { return false }

// Modal backend state for the configured mode from the direct credentials and the managed gateway
func modalBackendState(env *Env, modalMode any) ModalBackendState {
	mode, _ := modalMode.(string)
	return ResolveModalBackendState(env, mode, HasDirectModalCredentials(env), IsManagedToolGatewayReady(env, "modal", BuildVendorGatewayURL, nil), nil)
}

// Log message and error message for a modal state with no selected backend
func modalUnavailableReason(env *Env, state ModalBackendState) (string, string) {
	gateway := nousToolGatewayUnavailableMessage(env, "managed Modal execution", false)
	if state.ManagedModeBlocked || state.Mode == "managed" {
		tail := "the managed tool gateway is unavailable. " + gateway
		if state.ManagedModeBlocked {
			tail = "Nous Tool Gateway access is not currently available and no direct Modal credentials/config " +
				"were found. " + gateway + " Choose TERMINAL_MODAL_MODE=direct/auto to use direct Modal credentials."
		}
		return "Modal backend selected with TERMINAL_MODAL_MODE=managed, but " + tail,
			"Modal backend is configured for managed mode, but " + tail
	}
	managed := managedNousToolsEnabled(env, false)
	if state.Mode == "direct" {
		modes := "auto"
		if managed {
			modes = "managed/auto"
		}
		return "Modal backend selected with TERMINAL_MODAL_MODE=direct, but no direct Modal credentials/config " +
				"were found. Configure Modal or choose TERMINAL_MODAL_MODE=" + modes + ".",
			"Modal backend is configured for direct mode, but no direct Modal credentials/config were found."
	}
	found, fix := "was found", " or"
	if managed {
		found, fix = "or managed tool gateway was found", ", set up the managed gateway, or"
	}
	return "Modal backend selected but no direct Modal credentials/config " + found + ". Configure Modal" + fix + " choose a different TERMINAL_ENV.",
		"Modal backend selected but no direct Modal credentials/config " + found + "."
}

// IsAvailable reports whether direct credentials or the managed gateway could serve commands
func (modalBackend) IsAvailable() bool { return modalBackendState(nil, "auto").SelectedBackend != "" }

// CheckRequirements resolves the direct or managed backend, rejecting with the hermes wording
func (modalBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	state := modalBackendState(env, cfg["modal_mode"])
	if state.SelectedBackend == "managed" || state.SelectedBackend == "direct" {
		return nil
	}
	logMsg, _ := modalUnavailableReason(env, state)
	return errors.New(logMsg)
}

// Probe reports which Modal backend would run
func (modalBackend) Probe() (string, string) {
	state := modalBackendState(nil, "auto")
	switch state.SelectedBackend {
	case "managed":
		return "ready", "managed Modal through the Nous tool gateway"
	case "direct":
		return "ready", "direct Modal credentials"
	}
	_, msg := modalUnavailableReason(nil, state)
	return "needs_setup", msg
}

// SetupInstructions names the two Modal routes
func (modalBackend) SetupInstructions(env *Env) []string {
	return []string{
		"Serverless cloud sandboxes. Each session gets its own container.",
		"Use your Nous subscription through the managed gateway (terminal.modal_mode: managed), or your own Modal account.",
		"For a Modal account set MODAL_TOKEN_ID and MODAL_TOKEN_SECRET from https://modal.com/settings.",
	}
}

// DoctorChecks reports the selected Modal route
func (modalBackend) DoctorChecks() []DoctorCheck {
	state := modalBackendState(nil, "auto")
	switch state.SelectedBackend {
	case "managed":
		return []DoctorCheck{{OK: true, Label: "Modal backend", Detail: "(managed gateway)"}}
	case "direct":
		return []DoctorCheck{{OK: true, Label: "Modal credentials", Detail: "(configured)"}}
	}
	return []DoctorCheck{{OK: false, Label: "Modal credentials missing", Detail: "(needed for the 'modal' terminal backend)"}}
}

// CreateEnvironment builds the direct or managed Modal environment from the shaped resources
func (modalBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	res := sandboxResources(opts.ContainerConfig)
	kwargs := map[string]any{}
	for _, key := range []string{"cpu", "memory"} {
		if f, ok := pyFloatValue(res[key]); ok && f > 0 {
			kwargs[key] = res[key]
		}
	}
	state := modalBackendState(env, opts.ContainerConfig["modal_mode"])
	if state.SelectedBackend != "managed" && state.SelectedBackend != "direct" {
		_, msg := modalUnavailableReason(env, state)
		return nil, errors.New(msg)
	}
	persistent := truthy(res["persistent_filesystem"])
	if state.SelectedBackend == "managed" {
		return NewManagedModalEnvironment(ctx, env, ModalOptions{Image: opts.Image, Cwd: opts.Cwd, Timeout: opts.Timeout, SandboxKwargs: kwargs, Persistent: persistent, TaskID: opts.TaskID})
	}
	return NewModalEnvironment(ctx, env, ModalOptions{Image: opts.Image, Cwd: opts.Cwd, Timeout: opts.Timeout, SandboxKwargs: kwargs, Persistent: persistent, TaskID: opts.TaskID})
}
