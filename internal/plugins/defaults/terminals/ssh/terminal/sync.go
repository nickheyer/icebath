package terminal

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/terminal"
)

// Longest a bulk transfer over SSH may take
const transferTimeout = 120 * time.Second

// Uploads many files as one tar streamed into tar on the remote machine, after one mkdir of their folders
func (r *runner) upload(files []terminal.SyncFile) error {
	if len(files) == 0 {
		return nil
	}
	base := r.base()
	if parents := terminal.Parents(files); len(parents) > 0 {
		if err := r.checked(terminal.MkdirCommand(parents), 30*time.Second, "remote mkdir failed", "Remote directory setup on "+r.host); err != nil {
			return err
		}
	}
	names := make([]string, len(files))
	for i, f := range files {
		rel, under := strings.CutPrefix(path.Clean(f.Remote), base+"/")
		if !under {
			return fmt.Errorf("remote path %s escapes sync base %s", strconv.Quote(f.Remote), strconv.Quote(base))
		}
		names[i] = rel
	}
	ctx, cancel := context.WithTimeout(context.Background(), transferTimeout)
	defer cancel()
	argv := append(r.command(nil), "tar xf - --no-overwrite-dir -C "+terminal.Quote(base))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	written := make(chan error, 1)
	go func() {
		written <- writeTar(stdin, files, names)
	}()
	waited := cmd.Wait()
	packed := <-written
	if ctx.Err() != nil {
		return terminal.NewConnectionError("SSH bulk upload timed out", fmt.Sprintf("Bulk file sync to %s timed out — check the connection and retry.", r.host))
	}
	if waited != nil {
		return syncError(fmt.Sprintf("tar extract over SSH failed (rc=%d): %s", cmd.ProcessState.ExitCode(), strings.TrimSpace(stderr.String())), "File sync over SSH to "+r.host, "the connection")
	}
	if packed != nil {
		return fmt.Errorf("tar create failed: %w", packed)
	}
	slog.Debug("SSH: bulk-uploaded files via tar pipe", "count", len(files))
	return nil
}

// Writes the files to w as a tar under their names, then closes it
func writeTar(w io.WriteCloser, files []terminal.SyncFile, names []string) error {
	tw := tar.NewWriter(w)
	err := func() error {
		for i, f := range files {
			if err := terminal.TarFile(tw, f.Host, names[i]); err != nil {
				return err
			}
		}
		return tw.Close()
	}()
	if closed := w.Close(); err == nil {
		err = closed
	}
	return err
}

// Downloads the remote icebath folder as a tar, tolerating only tar's notes about sockets it skipped
func (r *runner) download(dest string) error {
	rel := strings.TrimLeft(r.base(), "/")
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), transferTimeout)
	defer cancel()
	argv := append(r.command(nil), "tar cf - --exclude='*.sock' -C / "+terminal.Quote(rel))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = out
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return syncError("SSH bulk download failed: timed out", "File sync from "+r.host, "")
	}
	text := strings.TrimSpace(stderr.String())
	var diagnostics []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			diagnostics = append(diagnostics, line)
		}
	}
	tolerated := cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 2 && len(diagnostics) > 0
	for _, line := range diagnostics {
		if !strings.HasSuffix(line, ": socket ignored") {
			tolerated = false
		}
	}
	if tolerated {
		return nil
	}
	return syncError("SSH bulk download failed: "+text, "File sync from "+r.host, "")
}

func (r *runner) remove(remotes []string) error {
	return r.checked(terminal.RemoveCommand(remotes), 10*time.Second, "remote rm failed", "Remote file cleanup on "+r.host)
}
