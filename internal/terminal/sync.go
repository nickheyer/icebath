package terminal

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	syncInterval = 5 * time.Second
	// Forces every sync to run, rate limit or not
	forceSyncEnv = "ICEBATH_FORCE_FILE_SYNC"
	// Change key recorded for a file whose mtime is too recent to trust
	racyKey    = "-1:-1:-1:-1"
	racyWindow = 2 * time.Second
	// Copies back from a sandbox: attempts, temp file names, and how old a left over temp file must be to go
	syncBackAttempts = 3
	syncBackPrefix   = "icebath-sync-back-"
	syncBackStale    = 30 * time.Minute
)

var syncBackBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// A host file and where a sandbox keeps it
type SyncFile struct {
	Host   string
	Remote string
	// A credential file, which goes up but never comes back
	UploadOnly bool
}

// How a remote backend moves files: one at a time, in bulk, and back as a tar
type Transport struct {
	Upload func(host, remote string) error
	Delete func(remotes []string) error
	// Uploads many at once, nil to upload one by one
	BulkUpload func(files []SyncFile) error
	// Writes a tar of the sandbox's copies to the path, nil for a backend that copies nothing back
	BulkDownload func(tar string) error
}

// Keeps a remote sandbox's copies of host files up to date, and copies the sandbox's changes back when it goes
type FileSync struct {
	config     *config.Config
	files      func() []SyncFile
	transport  Transport
	mu         sync.Mutex
	synced     map[string]string
	pushed     map[string]string
	uploadOnly map[string]bool
	last       time.Time
}

// Sync of the files the source lists, through the transport; credential files go up but never come back
func NewFileSync(cfg *config.Config, files func() []SyncFile, transport Transport) *FileSync {
	return &FileSync{config: cfg, files: files, transport: transport, synced: map[string]string{}, pushed: map[string]string{}, uploadOnly: map[string]bool{}}
}

// Change key of a file from its mtime, size, inode and change time, false when it can't be read
func fileKey(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	inode, ctime := fileIdentity(path)
	return fmt.Sprintf("%d:%d:%d:%d", st.ModTime().UnixNano(), st.Size(), inode, ctime), true
}

func racy(key string) bool {
	first, _, _ := strings.Cut(key, ":")
	mtime, err := strconv.ParseInt(first, 10, 64)
	return err == nil && time.Now().UnixNano()-mtime < int64(racyWindow)
}

// Host path resolved through links
func resolved(path string) string {
	path = fsx.ExpandHome(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Uploads changed files and deletes removed ones, at most every few seconds unless forced, its state rolled back when it fails
func (s *FileSync) Sync(force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && os.Getenv(forceSyncEnv) == "" && !s.last.IsZero() && time.Since(s.last) < syncInterval {
		return
	}
	current := s.files()
	for _, mount := range CredentialFiles(s.config) {
		s.uploadOnly[resolved(mount.Host)] = true
	}
	for _, f := range current {
		if f.UploadOnly {
			s.uploadOnly[resolved(f.Host)] = true
		}
	}
	previousSynced, previousPushed := maps.Clone(s.synced), maps.Clone(s.pushed)
	if err := s.sync(current); err != nil {
		s.synced, s.pushed = previousSynced, previousPushed
		slog.Warn("file_sync: sync failed, rolled back state", "error", err)
	}
}

func (s *FileSync) sync(current []SyncFile) error {
	upload, synced, deleted, err := s.plan(current)
	if err != nil {
		return err
	}
	if len(upload) == 0 && len(deleted) == 0 {
		s.synced, s.last = synced, time.Now()
		return nil
	}
	staging, err := os.MkdirTemp("", "icebath-sync-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	var staged []SyncFile
	pushed := map[string]string{}
	for i, f := range upload {
		local := filepath.Join(staging, strconv.Itoa(i))
		if err := copyFile(f.Host, local); err != nil {
			return err
		}
		digest, err := fsx.Digest(local)
		if err != nil {
			return err
		}
		pushed[f.Remote] = digest
		staged = append(staged, SyncFile{Host: local, Remote: f.Remote})
	}
	if err := s.push(staged, deleted); err != nil {
		return err
	}
	maps.Copy(s.pushed, pushed)
	for _, remote := range deleted {
		delete(synced, remote)
		delete(s.pushed, remote)
	}
	s.synced, s.last = synced, time.Now()
	return nil
}

// Files to upload, the synced state after, and remote files to delete
func (s *FileSync) plan(current []SyncFile) ([]SyncFile, map[string]string, []string, error) {
	var upload []SyncFile
	synced := maps.Clone(s.synced)
	for _, f := range current {
		key, ok := fileKey(f.Host)
		if !ok || s.synced[f.Remote] == key {
			continue
		}
		synced[f.Remote] = key
		if racy(key) {
			synced[f.Remote] = racyKey
		}
		if pushed, ok := s.pushed[f.Remote]; ok {
			digest, err := fsx.Digest(f.Host)
			if err != nil {
				return nil, nil, nil, err
			}
			if digest == pushed {
				continue
			}
		}
		upload = append(upload, f)
	}
	present := map[string]bool{}
	for _, f := range current {
		present[f.Remote] = true
	}
	var deleted []string
	for remote := range s.synced {
		if !present[remote] {
			deleted = append(deleted, remote)
		}
	}
	slices.Sort(deleted)
	return upload, synced, deleted, nil
}

func (s *FileSync) push(upload []SyncFile, deleted []string) error {
	if len(upload) > 0 {
		if s.transport.BulkUpload != nil {
			if err := s.transport.BulkUpload(upload); err != nil {
				return err
			}
		} else {
			for _, f := range upload {
				if err := s.transport.Upload(f.Host, f.Remote); err != nil {
					return err
				}
			}
		}
		slog.Debug("file_sync: uploaded files", "count", len(upload))
	}
	if len(deleted) > 0 {
		if err := s.transport.Delete(deleted); err != nil {
			return err
		}
		slog.Debug("file_sync: deleted", "paths", deleted)
	}
	return nil
}

// Copies a file keeping its mode and modification time
func copyFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, time.Now(), st.ModTime())
}

// Shell commands removing and making a batch of remote paths
func RemoveCommand(remotes []string) string { return "rm -f " + quoteAll(remotes) }
func MkdirCommand(dirs []string) string     { return "mkdir -p " + quoteAll(dirs) }

func quoteAll(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = Quote(word)
	}
	return strings.Join(quoted, " ")
}

// Remote folders the files go in, sorted and once each
func Parents(files []SyncFile) []string {
	var dirs []string
	for _, f := range files {
		if dir := path.Dir(f.Remote); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// Copies the sandbox's changes back to the host as it goes, retried, serialized across processes by a lock in the profile folder
func (s *FileSync) SyncBack() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transport.BulkDownload == nil {
		return
	}
	if len(s.pushed) == 0 && len(s.synced) == 0 {
		slog.Debug("sync_back: no prior push state, skipping")
		return
	}
	lock := filepath.Join(s.config.ProfileDir(s.config.Profile()), ".sync.lock")
	os.MkdirAll(filepath.Dir(lock), 0o755)
	var last error
	for attempt := range syncBackAttempts {
		if last = s.syncBackOnce(lock); last == nil {
			return
		}
		if attempt < syncBackAttempts-1 {
			slog.Warn("sync_back: attempt failed, retrying", "attempt", attempt+1, "error", last, "delay", syncBackBackoff[attempt])
			time.Sleep(syncBackBackoff[attempt])
		}
	}
	slog.Warn("sync_back: all attempts failed", "attempts", syncBackAttempts, "error", last)
}

func (s *FileSync) syncBackOnce(lock string) error {
	unlock, err := fsx.Lock(context.Background(), lock, 24*time.Hour)
	if err != nil {
		return err
	}
	defer unlock()
	mapping := s.files()
	removeStaleSyncTemp()
	archive, err := os.CreateTemp("", fmt.Sprintf("%s%d-*.tar", syncBackPrefix, os.Getpid()))
	if err != nil {
		return err
	}
	archive.Close()
	defer os.Remove(archive.Name())
	if err := s.transport.BulkDownload(archive.Name()); err != nil {
		return err
	}
	limit := cast.ToInt64(s.config.Get(terminalconfig.SyncBackMax))
	if st, err := os.Stat(archive.Name()); err == nil && st.Size() > limit {
		slog.Warn("sync_back: remote tar exceeds the extraction cap, skipping extraction", "bytes", st.Size(), "cap", limit, "override", terminalconfig.SyncBackMax)
		return nil
	}
	staging, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", syncBackPrefix, os.Getpid()))
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extract(archive.Name(), staging); err != nil {
		return err
	}
	uploadOnly := maps.Clone(s.uploadOnly)
	for _, mount := range CredentialFiles(s.config) {
		uploadOnly[resolved(mount.Host)] = true
	}
	for _, f := range mapping {
		if f.UploadOnly {
			uploadOnly[resolved(f.Host)] = true
		}
	}
	applied := 0
	err = filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		n, err := s.apply(p, "/"+filepath.ToSlash(rel), mapping, uploadOnly)
		applied += n
		return err
	})
	if err != nil {
		return err
	}
	if applied > 0 {
		slog.Info("sync_back: applied changed files", "count", applied)
	}
	return nil
}

// Removes copy back temp files a killed process left, those of a live process kept until they are stale
func removeStaleSyncTemp() {
	candidates, _ := filepath.Glob(filepath.Join(os.TempDir(), syncBackPrefix+"*"))
	for _, candidate := range candidates {
		st, err := os.Lstat(candidate)
		if err != nil || st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		owner, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(candidate), syncBackPrefix), "-")
		pid, err := strconv.Atoi(owner)
		if (err != nil || Running(pid)) && time.Since(st.ModTime()) < syncBackStale {
			continue
		}
		os.RemoveAll(candidate)
	}
}

// Extracts a tar into dest, refusing links and paths that leave it
func extract(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}
	within := func(p string) bool { return p == dest || strings.HasPrefix(p, dest+string(filepath.Separator)) }
	reader := tar.NewReader(f)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimLeft(filepath.ToSlash(header.Name), "/")
		if name == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if !within(target) {
			return fmt.Errorf("tar member %q escapes the extraction directory", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, reader); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			if !header.ModTime.IsZero() {
				os.Chtimes(target, time.Now(), header.ModTime)
			}
		case tar.TypeSymlink:
			link := filepath.FromSlash(header.Linkname)
			if filepath.IsAbs(link) || filepath.VolumeName(link) != "" || strings.HasPrefix(header.Linkname, "/") || strings.HasPrefix(header.Linkname, `\`) ||
				!within(filepath.Join(filepath.Dir(target), link)) {
				return fmt.Errorf("tar link %q points outside the extraction directory", header.Name)
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case tar.TypeLink:
			link := filepath.Join(dest, filepath.FromSlash(strings.TrimLeft(filepath.ToSlash(header.Linkname), "/")))
			if !within(link) {
				return fmt.Errorf("tar link %q points outside the extraction directory", header.Name)
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Link(link, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tar member %q has an unsupported type %q", header.Name, header.Typeflag)
		}
	}
}

// Copies one file of the sandbox onto the host when it changed since it went up, answering 1 when it did
func (s *FileSync) apply(staged, remote string, mapping []SyncFile, uploadOnly map[string]bool) (int, error) {
	pushed, hasPushed := s.pushed[remote]
	if hasPushed {
		digest, err := fsx.Digest(staged)
		if err != nil {
			return 0, err
		}
		if digest == pushed {
			return 0, nil
		}
	}
	host := ""
	for _, f := range mapping {
		if f.Remote == remote {
			host = f.Host
		}
	}
	if host == "" {
		host = inferHost(remote, mapping, uploadOnly)
	}
	if host == "" {
		slog.Debug("sync_back: skipping remote file with no host mapping", "remote", remote)
		return 0, nil
	}
	if uploadOnly[resolved(host)] {
		slog.Debug("sync_back: skipping upload-only credential file", "remote", remote)
		return 0, nil
	}
	if hasPushed {
		if digest, err := fsx.Digest(host); err == nil && digest != pushed {
			slog.Warn("sync_back: conflict, host modified since push and remote also changed, applying the remote version", "remote", remote)
		}
	}
	if err := copyFile(staged, host); err != nil {
		return 0, err
	}
	return 1, nil
}

// Host path of a file new in the sandbox, placed beside a known file of the same remote folder
func inferHost(remote string, mapping []SyncFile, uploadOnly map[string]bool) string {
	for _, f := range mapping {
		if uploadOnly[resolved(f.Host)] {
			continue
		}
		if dir := path.Dir(f.Remote); strings.HasPrefix(remote, dir+"/") {
			return filepath.Join(filepath.Dir(f.Host), filepath.FromSlash(remote[len(dir)+1:]))
		}
	}
	return ""
}

// Adds one host file to a tar a sync sends under the name, its mode and time kept
func TarFile(tw *tar.Writer, host, name string) error {
	st, err := os.Stat(host)
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(st, "")
	if err != nil {
		return err
	}
	header.Name = name
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	f, err := os.Open(host)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}
