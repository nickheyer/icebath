package terminal

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
)

const (
	// Stdin goes in chunks under the 2 MB cap of Modal's older servers
	stdinChunk = 1024 * 1024
	// Longest a sandbox lives on Modal, which ends it then
	lifetime = time.Hour
	// Longest the app lookup and the sandbox's creation may take
	createTimeout = 300 * time.Second
	// Snapshots of sandbox files
	snapshotTimeout = 60 * time.Second
	terminateWait   = 15 * time.Second
)

// Commands run in a Modal sandbox made through Modal's SDK, its filesystem snapshotted between sessions when sandbox files are kept
type environment struct {
	*terminal.Shell
	runner *runner
}

// Pushes changed host files into the sandbox now
func (e *environment) SyncFiles() {
	if _, files := e.runner.current(); files != nil {
		files.Sync(true)
	}
}

type runner struct {
	spec   *terminal.Spec
	skills *skills.Registry
	shell  *terminal.Shell
	client *modal.Client
	image  string
	params modal.SandboxCreateParams
	// Snapshots are taken and restored, for the environment's key
	persistent bool
	task       string
	snapshots  terminal.Snapshots
	// Serializes the check that the sandbox still runs, its making again and its removal
	ready sync.Mutex

	mu      sync.Mutex
	sandbox *modal.Sandbox
	sync    *terminal.FileSync
}

// Makes the sandbox, from the key's snapshot when sandbox files are kept, puts the profile's files in it and takes the session's snapshot
func create(ctx context.Context, spec *terminal.Spec, skillset *skills.Registry) (terminal.Environment, error) {
	cfg := spec.Config
	if !credentialed(cfg) {
		return nil, errors.New(unavailable)
	}
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return nil, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	r := &runner{
		spec:       spec,
		skills:     skillset,
		client:     client,
		image:      image(cfg),
		params:     modal.SandboxCreateParams{Command: []string{"sleep", "infinity"}, Timeout: lifetime},
		persistent: resources.Persistent && !spec.Probe,
		task:       cmp.Or(terminal.Task(cfg, "modal", spec.Key), "default"),
		snapshots:  terminal.NewSnapshots(cfg, "modal_snapshots.json"),
	}
	if resources.CPU > 0 {
		r.params.CPU = resources.CPU
	}
	if resources.Memory > 0 {
		r.params.MemoryMiB = resources.Memory
	}
	r.shell = terminal.NewShell(spec, r, terminal.ShellOptions{
		Backend:         "modal",
		Cwd:             cmp.Or(spec.Cwd, "/root"),
		Timeout:         spec.Timeout,
		Persist:         true,
		Persistent:      r.persistent,
		SnapshotTimeout: 60 * time.Second,
		Forwarded:       r.forwarded,
	})
	if err := r.start(ctx); err != nil {
		client.Close()
		return nil, err
	}
	slog.Info("Modal: sandbox created", "task", r.task)
	if !spec.Probe {
		r.shell.Init(ctx)
	}
	return &environment{Shell: r.shell, runner: r}, nil
}

// First characters of an id, as logs name it
func short(id string) string { return id[:min(20, len(id))] }

// Makes the sandbox and its file sync, from the stored snapshot when there is one and the image when it fails
func (r *runner) start(ctx context.Context) error {
	restored := ""
	if r.persistent {
		restored = r.snapshots.Get(r.task)
	}
	if restored != "" {
		slog.Info("Modal: restoring from snapshot", "snapshot", short(restored))
	}
	if err := r.make(ctx, cmp.Or(restored, r.image)); err != nil {
		if restored == "" {
			return err
		}
		slog.Warn("Modal: failed to restore snapshot, retrying with base image", "snapshot", short(restored), "error", err)
		r.snapshots.Delete(r.task, restored)
		if err := r.make(ctx, r.image); err != nil {
			return err
		}
	}
	if r.spec.Probe {
		return nil
	}
	cfg := r.spec.Config
	files := terminal.NewFileSync(cfg, func() []terminal.SyncFile {
		needs, _ := r.spec.Skills(r.skills)
		return terminal.SyncFiles(cfg, terminal.SandboxProfile, needs)
	}, terminal.Transport{Delete: r.remove, BulkUpload: r.upload, BulkDownload: r.download})
	r.mu.Lock()
	r.sync = files
	r.mu.Unlock()
	files.Sync(true)
	return nil
}

// Makes a sandbox under the app from the image, or the snapshot, the setting names
func (r *runner) make(ctx context.Context, spec string) error {
	image, err := resolveImage(ctx, r.client, spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	application, err := r.client.Apps.FromName(ctx, app, &modal.AppFromNameParams{CreateIfMissing: true})
	if err != nil {
		return err
	}
	params := r.params
	sandbox, err := r.client.Sandboxes.Create(ctx, application, image, &params)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.sandbox = sandbox
	r.mu.Unlock()
	return nil
}

// Sandbox and file sync in use, nil once cleaned up
func (r *runner) current() (*modal.Sandbox, *terminal.FileSync) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sandbox, r.sync
}

// Error of a command after the environment was cleaned up
var cleaned = errors.New("The Modal sandbox was removed. Run the command again.")

// Writes the payload to stdin in chunks, then closes it
func streamStdin(w io.WriteCloser, data []byte) error {
	for offset := 0; offset < len(data); offset += stdinChunk {
		if _, err := w.Write(data[offset:min(offset+stdinChunk, len(data))]); err != nil {
			return err
		}
	}
	return w.Close()
}

// Runs bash -c in the sandbox, stdin streamed when given, answering stdout when capturing; a non-zero exit fails when it has a label
func (r *runner) exec(cmd string, timeout time.Duration, stdin []byte, label string, capture bool) ([]byte, error) {
	sandbox, _ := r.current()
	if sandbox == nil {
		return nil, cleaned
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	proc, err := sandbox.Exec(ctx, []string{"bash", "-c", cmd}, &modal.SandboxExecParams{Stdout: modal.Pipe, Stderr: modal.Pipe})
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		if err := streamStdin(proc.Stdin, stdin); err != nil {
			return nil, err
		}
	} else {
		proc.Stdin.Close()
	}
	var data []byte
	if capture {
		if data, err = io.ReadAll(proc.Stdout); err != nil {
			return nil, err
		}
	}
	code, err := proc.Wait(ctx, nil)
	if err != nil {
		return nil, err
	}
	if label != "" && code != 0 {
		detail := ""
		if !capture {
			stderr, _ := io.ReadAll(proc.Stderr)
			detail = ": " + string(stderr)
		}
		return nil, fmt.Errorf("Modal %s failed (exit %d)%s", label, code, detail)
	}
	return data, nil
}

// Uploads many files as one gzipped tar streamed through stdin and unpacked at the root
func (r *runner) upload(files []terminal.SyncFile) error {
	if len(files) == 0 {
		return nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := terminal.TarFile(tw, f.Host, strings.TrimLeft(f.Remote, "/")); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	payload := []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))
	_, err := r.exec(terminal.MkdirCommand(terminal.Parents(files))+" && base64 -d | tar xzf - -C /", 120*time.Second, payload, "bulk upload", false)
	return err
}

// Downloads the sandbox's icebath folder as a tar
func (r *runner) download(dest string) error {
	data, err := r.exec("tar cf - --exclude='*.sock' -C / "+terminal.Quote(strings.TrimLeft(terminal.SandboxProfile, "/")), 120*time.Second, nil, "bulk download", true)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}

func (r *runner) remove(remotes []string) error {
	_, err := r.exec(terminal.RemoveCommand(remotes), 15*time.Second, nil, "", false)
	return err
}

// Variables handed each command and the names unset for it: those the profile passes through and those its skills need
func (r *runner) forward() (map[string]string, []string) {
	_, extra := r.spec.Skills(r.skills)
	return r.spec.Terminal.Forward(r.spec.Config, extra)
}

// Names of the variables handed each command, kept out of the snapshot
func (r *runner) forwarded() []string {
	values, unset := r.forward()
	return append(slices.Collect(maps.Keys(values)), unset...)
}

// Makes the sandbox again when Modal ended it, at its lifetime or on an interrupt, then pushes changed host files
func (r *runner) Prepare(ctx context.Context) error {
	r.ready.Lock()
	defer r.ready.Unlock()
	sandbox, _ := r.current()
	if sandbox == nil {
		return cleaned
	}
	code, err := sandbox.Poll(ctx, nil)
	if _, gone := errors.AsType[modal.NotFoundError](err); err != nil && !gone {
		return terminal.NewConnectionError("Modal sandbox could not be reached: "+err.Error(), "")
	}
	if err != nil || code != nil {
		slog.Warn("Modal: sandbox ended; recreating", "task", r.task)
		r.shell.MarkRecreated()
		if err := r.start(ctx); err != nil {
			return err
		}
		r.shell.Init(ctx)
	}
	if _, files := r.current(); files != nil {
		files.Sync(false)
	}
	return nil
}

// Runs bash in the sandbox on a goroutine, the variables handed through the exec and the scoped ones unset first; stopping it ends the
// sandbox, which Modal offers no other way to stop a command by
func (r *runner) Run(_ context.Context, script string, login bool, timeout time.Duration, stdin *string) (terminal.Proc, error) {
	sandbox, _ := r.current()
	if sandbox == nil {
		return nil, cleaned
	}
	values, unset := r.forward()
	argv := []string{"bash", "-c", terminal.Unset(script, unset)}
	if login {
		argv = []string{"bash", "-l", "-c", terminal.Unset(script, unset)}
	}
	params := &modal.SandboxExecParams{Stdout: modal.Pipe, Stderr: modal.Pipe, Timeout: time.Duration(math.Ceil(timeout.Seconds())) * time.Second}
	if len(values) > 0 {
		params.Env = values
	}
	run := func() (string, int) {
		ctx, done := context.WithTimeout(context.Background(), timeout+30*time.Second)
		defer done()
		proc, err := sandbox.Exec(ctx, argv, params)
		if err != nil {
			return "Modal exec failed: " + err.Error(), 1
		}
		if stdin != nil {
			if err := streamStdin(proc.Stdin, []byte(*stdin)); err != nil {
				return "Modal stdin failed: " + err.Error(), 1
			}
		} else {
			proc.Stdin.Close()
		}
		var stderr []byte
		read := make(chan struct{})
		go func() {
			defer close(read)
			stderr, _ = io.ReadAll(proc.Stderr)
		}()
		stdout, _ := io.ReadAll(proc.Stdout)
		<-read
		code, err := proc.Wait(ctx, nil)
		if err != nil {
			return "Modal exec failed: " + err.Error(), 1
		}
		var parts []string
		for _, part := range [][]byte{stdout, stderr} {
			if text := strings.ToValidUTF8(string(part), "\ufffd"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n"), code
	}
	terminate := func() {
		ctx, done := context.WithTimeout(context.Background(), terminateWait)
		defer done()
		sandbox.Terminate(ctx, nil)
	}
	return terminal.NewFuncProc(run, terminate)
}

func (r *runner) Kill(p terminal.Proc)      { p.Kill() }
func (r *runner) ForceKill(p terminal.Proc) { p.Kill() }

// Copies the sandbox's changes back, snapshots its filesystem when sandbox files are kept and the profile stays, then ends it
func (r *runner) Cleanup(force bool) error {
	r.ready.Lock()
	defer r.ready.Unlock()
	sandbox, files := r.current()
	if sandbox == nil {
		return nil
	}
	if files != nil {
		slog.Info("Modal: syncing files from sandbox...")
		files.SyncBack()
	}
	if r.persistent && !force {
		ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
		image, err := sandbox.SnapshotFilesystem(ctx, &modal.SandboxSnapshotFilesystemParams{TTL: modal.NoExpiryTTL, Timeout: snapshotTimeout})
		cancel()
		switch {
		case err != nil:
			slog.Warn("Modal: filesystem snapshot failed", "error", err)
		case image != nil && image.ImageID != "":
			if err := r.snapshots.Set(r.task, image.ImageID); err != nil {
				slog.Warn("Modal: filesystem snapshot failed", "error", err)
			} else {
				slog.Info("Modal: saved filesystem snapshot", "snapshot", short(image.ImageID), "task", r.task)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminateWait)
	sandbox.Terminate(ctx, nil)
	cancel()
	r.client.Close()
	r.mu.Lock()
	r.sandbox, r.sync = nil, nil
	r.mu.Unlock()
	return nil
}
