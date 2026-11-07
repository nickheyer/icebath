package tools

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	fileSyncInterval     = 5 * time.Second
	forceFileSyncEnv     = "ICEBATH_FORCE_FILE_SYNC"
	racySyncKey          = "-1:-1:-1:-1"
	racyMtimeWindow      = 2 * time.Second
	syncBackMaxRetries   = 3
	syncBackMaxBytes     = int64(2 * 1024 * 1024 * 1024)
	syncBackMaxBytesKey  = "sync_back_max_bytes"
	syncBackTempPrefix   = "icebath-sync-back-"
	syncBackStaleSeconds = 30 * 60
)

var syncBackBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// syncFile is one host file and where it lands on the remote
type syncFile struct {
	host   string
	remote string
}

// Extraction cap, terminal.sync_back_max_bytes overriding the 2 GiB default
func syncBackMaxBytesFor(env *Env) int64 {
	if env == nil || env.Config == nil {
		return syncBackMaxBytes
	}
	raw, ok := env.Config.Lookup("terminal." + syncBackMaxBytesKey)
	if !ok || raw == nil {
		return syncBackMaxBytes
	}
	if n, ok := pyIntValue(raw); ok {
		if _, isString := raw.(string); !isString || pyIntLiteralRe.MatchString(strings.TrimSpace(raw.(string))) {
			return int64(n)
		}
	}
	toolsLogger(env).Warn("sync_back: ignoring non-integer terminal."+syncBackMaxBytesKey, "value", raw)
	return syncBackMaxBytes
}

// Temp prefix embedding the owning pid so the stale sweep tells a dead owner from a live transfer
func syncBackTempPrefixFor() string { return fmt.Sprintf("%s%d-", syncBackTempPrefix, os.Getpid()) }

// Whether the process that created a sync back temp entry may still be running, names without a pid count as alive
func syncTempEntryOwnerAlive(name string) bool {
	pidPart, _, _ := strings.Cut(strings.TrimPrefix(name, syncBackTempPrefix), "-")
	if !isDigits(pidPart) {
		return true
	}
	pid, _ := strconv.Atoi(pidPart)
	return syncPidExists(pid)
}

// Removes sync back tars and staging dirs left behind by a hard killed process, returning the count removed
func cleanupStaleSyncBackTemp(env *Env, tempDir string) int {
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	cutoff := time.Now().Add(-syncBackStaleSeconds * time.Second)
	candidates, err := filepath.Glob(filepath.Join(tempDir, syncBackTempPrefix+"*"))
	if err != nil {
		toolsLogger(env).Debug("sync_back: could not scan temporary directory", "dir", tempDir)
		return 0
	}
	removed := 0
	for _, candidate := range candidates {
		st, err := os.Lstat(candidate)
		if err != nil || st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if syncTempEntryOwnerAlive(filepath.Base(candidate)) && !st.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(candidate); err != nil {
			toolsLogger(env).Debug("sync_back: could not remove stale temporary entry", "path", candidate)
			continue
		}
		removed++
		toolsLogger(env).Debug("sync_back: removed stale temporary entry", "path", candidate)
	}
	return removed
}

// Every host and remote pair to sync, credential paths remapped from the container base to the remote base
func iterSyncFiles(env *Env, containerBase string) []syncFile {
	var files []syncFile
	for _, m := range GetCredentialFileMounts(env) {
		files = append(files, syncFile{m.HostPath, strings.Replace(m.ContainerPath, DefaultContainerBase, containerBase, 1)})
	}
	for _, m := range IterSkillsFiles(env, containerBase) {
		files = append(files, syncFile{m.HostPath, m.ContainerPath})
	}
	for _, m := range IterCacheFiles(env, containerBase) {
		files = append(files, syncFile{m.HostPath, m.ContainerPath})
	}
	return files
}

// Canonical string form of a host path, symlinks resolved when possible
func resolveHostPathString(hostPath string) string {
	expanded := expandTilde(hostPath)
	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return abs
		}
		return resolved
	}
	if abs, err := filepath.Abs(expanded); err == nil {
		return abs
	}
	return expanded
}

// Credential files that are upload only for remote sandboxes
func credentialHostPaths(env *Env) map[string]bool {
	out := map[string]bool{}
	for _, m := range GetCredentialFileMounts(env) {
		if m.HostPath != "" {
			out[resolveHostPathString(m.HostPath)] = true
		}
	}
	return out
}

// Shell rm -f command for a batch of remote paths
func quotedRmCommand(remotePaths []string) string {
	quoted := make([]string, len(remotePaths))
	for i, p := range remotePaths {
		quoted[i] = shQuote(p)
	}
	return "rm -f " + strings.Join(quoted, " ")
}

// Shell mkdir -p command for a batch of directories
func quotedMkdirCommand(dirs []string) string {
	quoted := make([]string, len(dirs))
	for i, d := range dirs {
		quoted[i] = shQuote(d)
	}
	return "mkdir -p " + strings.Join(quoted, " ")
}

// Sorted unique remote parent directories of the pairs
func uniqueParentDirs(files []syncFile) []string {
	set := map[string]bool{}
	for _, f := range files {
		set[path.Dir(f.remote)] = true
	}
	return sortedSetKeys(set)
}

// Hex SHA-256 digest of a file
func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Reports whether the mtime is too recent to rule out a same tick write after the stat
func mtimeIsRacy(key string) bool {
	first, _, _ := strings.Cut(key, ":")
	mtime, err := strconv.ParseInt(first, 10, 64)
	return err == nil && time.Now().UnixNano()-mtime < int64(racyMtimeWindow)
}

// FileSyncManager tracks host file changes and syncs them to a remote through backend transport callbacks
type FileSyncManager struct {
	env          *Env
	getFiles     func() []syncFile
	upload       func(hostPath, remotePath string) error
	bulkUpload   func(files []syncFile) error
	bulkDownload func(destTar string) error
	delete       func(remotePaths []string) error
	mu           sync.Mutex
	syncedFiles  map[string]string
	pushedHashes map[string]string
	uploadOnly   map[string]bool
	lastSync     time.Time
	syncInterval time.Duration
	now          func() time.Time
	sleep        func(time.Duration)
}

// File sync manager of a remote backend environment, nil for backends that keep none
func EnvironmentSyncManager(e Environment) *FileSyncManager {
	switch b := e.(type) {
	case *sshEnv:
		return b.syncManager
	case *modalEnv:
		return b.syncManager
	case *daytonaEnv:
		b.lock.Lock()
		defer b.lock.Unlock()
		return b.syncManager
	case *vercelEnv:
		b.lock.Lock()
		defer b.lock.Unlock()
		return b.syncManager
	}
	return nil
}

// NewFileSyncManager wires the transport callbacks, bulk callbacks being optional
func NewFileSyncManager(env *Env, getFiles func() []syncFile, upload func(string, string) error, delete func([]string) error,
	bulkUpload func([]syncFile) error, bulkDownload func(string) error) *FileSyncManager {
	return &FileSyncManager{env: env, getFiles: getFiles, upload: upload, delete: delete, bulkUpload: bulkUpload, bulkDownload: bulkDownload,
		syncedFiles: map[string]string{}, pushedHashes: map[string]string{}, uploadOnly: map[string]bool{}, syncInterval: fileSyncInterval,
		now: time.Now, sleep: time.Sleep}
}

// Sync uploads changed files and deletes removed ones, rate limited unless forced, rolling back state on any failure
func (m *FileSyncManager) Sync(force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncTransaction(force)
}

// One sync cycle under the manager lock
func (m *FileSyncManager) syncTransaction(force bool) {
	if !force && os.Getenv(forceFileSyncEnv) == "" && !m.lastSync.IsZero() && m.now().Sub(m.lastSync) < m.syncInterval {
		return
	}
	current := m.getFiles()
	for p := range credentialHostPaths(m.env) {
		m.uploadOnly[p] = true
	}
	prevFiles, prevHashes := copyStringMap(m.syncedFiles), copyStringMap(m.pushedHashes)
	err := func() error {
		toUpload, newFiles, toDelete, err := m.planSync(current)
		if err != nil {
			return err
		}
		if len(toUpload) == 0 && len(toDelete) == 0 {
			m.syncedFiles = newFiles
			m.lastSync = m.now()
			return nil
		}
		staging, err := os.MkdirTemp("", "icebath-sync-push-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		var staged []syncFile
		pushed := map[string]string{}
		for i, f := range toUpload {
			stagedPath := filepath.Join(staging, strconv.Itoa(i))
			if err := copyFileWithTimes(f.host, stagedPath); err != nil {
				return err
			}
			digest, err := sha256File(stagedPath)
			if err != nil {
				return err
			}
			pushed[f.remote] = digest
			staged = append(staged, syncFile{stagedPath, f.remote})
		}
		if err := m.push(staged, toDelete); err != nil {
			return err
		}
		for k, v := range pushed {
			m.pushedHashes[k] = v
		}
		for _, p := range toDelete {
			delete(newFiles, p)
			delete(m.pushedHashes, p)
		}
		m.syncedFiles = newFiles
		m.lastSync = m.now()
		return nil
	}()
	if err != nil {
		m.syncedFiles, m.pushedHashes = prevFiles, prevHashes
		toolsLogger(m.env).Warn("file_sync: sync failed, rolled back state", "error", err)
	}
}

// Copy of a string map
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Copies a file keeping its mode and modification time
func copyFileWithTimes(src, dst string) error {
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

// Diffs the current pairs against the synced state into uploads, the new state, and deletions
func (m *FileSyncManager) planSync(current []syncFile) ([]syncFile, map[string]string, []string, error) {
	var toUpload []syncFile
	newFiles := copyStringMap(m.syncedFiles)
	for _, f := range current {
		key, ok := fileMtimeKey(f.host)
		if !ok || m.syncedFiles[f.remote] == key {
			continue
		}
		recorded := key
		if mtimeIsRacy(key) {
			recorded = racySyncKey
		}
		newFiles[f.remote] = recorded
		if pushed, ok := m.pushedHashes[f.remote]; ok {
			digest, err := sha256File(f.host)
			if err != nil {
				return nil, nil, nil, err
			}
			if digest == pushed {
				continue
			}
		}
		toUpload = append(toUpload, f)
	}
	currentRemote := map[string]bool{}
	for _, f := range current {
		currentRemote[f.remote] = true
	}
	var toDelete []string
	for p := range m.syncedFiles {
		if !currentRemote[p] {
			toDelete = append(toDelete, p)
		}
	}
	sort.Strings(toDelete)
	return toUpload, newFiles, toDelete, nil
}

// Runs the transport calls for one cycle, bulk upload when available
func (m *FileSyncManager) push(toUpload []syncFile, toDelete []string) error {
	log := toolsLogger(m.env)
	if len(toUpload) > 0 {
		log.Debug("file_sync: uploading files", "count", len(toUpload))
	}
	if len(toDelete) > 0 {
		log.Debug("file_sync: deleting stale remote files", "count", len(toDelete))
	}
	if len(toUpload) > 0 && m.bulkUpload != nil {
		if err := m.bulkUpload(toUpload); err != nil {
			return err
		}
		log.Debug("file_sync: bulk-uploaded files", "count", len(toUpload))
	} else {
		for _, f := range toUpload {
			if err := m.upload(f.host, f.remote); err != nil {
				return err
			}
			log.Debug("file_sync: uploaded", "host", f.host, "remote", f.remote)
		}
	}
	if len(toDelete) > 0 {
		if err := m.delete(toDelete); err != nil {
			return err
		}
		log.Debug("file_sync: deleted", "paths", toDelete)
	}
	return nil
}

// SyncBack pulls remote changes to the host on teardown, SIGINT deferred and concurrent gateways serialized by a file lock
func (m *FileSyncManager) SyncBack(icebathHome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncBackTransaction(icebathHome)
}

// Sync back with retries against the manager state
func (m *FileSyncManager) syncBackTransaction(icebathHome string) {
	if m.bulkDownload == nil {
		return
	}
	log := toolsLogger(m.env)
	if len(m.pushedHashes) == 0 && len(m.syncedFiles) == 0 {
		log.Debug("sync_back: no prior push state, skipping")
		return
	}
	if icebathHome == "" {
		icebathHome = m.env.Home
	}
	lockPath := filepath.Join(icebathHome, ".sync.lock")
	os.MkdirAll(filepath.Dir(lockPath), 0o755)
	var lastErr error
	for attempt := 0; attempt < syncBackMaxRetries; attempt++ {
		if err := m.syncBackOnce(lockPath); err == nil {
			return
		} else {
			lastErr = err
			if attempt < syncBackMaxRetries-1 {
				delay := syncBackBackoff[attempt]
				log.Warn("sync_back: attempt failed, retrying", "attempt", attempt+1, "error", err, "delay", delay)
				m.sleep(delay)
			}
		}
	}
	log.Warn("sync_back: all attempts failed", "attempts", syncBackMaxRetries, "error", lastErr)
}

// Single sync back attempt with SIGINT deferral and the file lock
func (m *FileSyncManager) syncBackOnce(lockPath string) error {
	restore := deferInterruptSignal(m.env)
	defer restore()
	unlock, err := lockStoreFile(lockPath, 24*time.Hour)
	if err != nil {
		return err
	}
	defer unlock()
	return m.syncBackImpl()
}

// Downloads, diffs, and applies remote changes to the host
func (m *FileSyncManager) syncBackImpl() (err error) {
	log := toolsLogger(m.env)
	mapping := m.getFiles()
	cleanupStaleSyncBackTemp(m.env, "")
	tarFile, err := os.CreateTemp("", syncBackTempPrefixFor()+"*.tar")
	if err != nil {
		return err
	}
	tarPath := tarFile.Name()
	tarFile.Close()
	defer os.Remove(tarPath)
	if err := m.bulkDownload(tarPath); err != nil {
		return err
	}
	var tarSize int64
	if st, err := os.Stat(tarPath); err == nil {
		tarSize = st.Size()
	}
	maxBytes := syncBackMaxBytesFor(m.env)
	if tarSize > maxBytes {
		log.Warn("sync_back: remote tar exceeds the extraction cap, skipping extraction", "bytes", tarSize, "cap", maxBytes, "override", "terminal."+syncBackMaxBytesKey)
		return nil
	}
	staging, err := os.MkdirTemp("", syncBackTempPrefixFor())
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractTarData(tarPath, staging); err != nil {
		return err
	}
	uploadOnly := copyBoolMap(m.uploadOnly)
	for p := range credentialHostPaths(m.env) {
		uploadOnly[p] = true
	}
	applied := 0
	walkErr := filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		remote := "/" + filepath.ToSlash(rel)
		n, err := m.applyStagedFile(p, remote, mapping, uploadOnly)
		if err != nil {
			return err
		}
		applied += n
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if applied > 0 {
		log.Info("sync_back: applied changed files", "count", applied)
	} else {
		log.Debug("sync_back: no remote changes detected")
	}
	return nil
}

// Copy of a bool set
func copyBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Extracts a tar into dest with the Python data filter rules, refusing links and paths that escape dest
func extractTarData(tarPath, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := tar.NewReader(f)
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		hdr, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimLeft(filepath.ToSlash(hdr.Name), "/")
		if name == "" {
			continue
		}
		target := filepath.Join(destAbs, filepath.FromSlash(name))
		if !insideDir(destAbs, target) {
			return fmt.Errorf("tar member %q escapes the extraction directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode).Perm() | 0o600
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
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
			if !hdr.ModTime.IsZero() {
				os.Chtimes(target, time.Now(), hdr.ModTime)
			}
		case tar.TypeSymlink:
			if tarLinkRooted(hdr.Linkname) || !insideDir(destAbs, filepath.Join(filepath.Dir(target), filepath.FromSlash(hdr.Linkname))) {
				return fmt.Errorf("tar link %q points outside the extraction directory", hdr.Name)
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Symlink(filepath.FromSlash(hdr.Linkname), target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkTarget := filepath.Join(destAbs, filepath.FromSlash(strings.TrimLeft(filepath.ToSlash(hdr.Linkname), "/")))
			if !insideDir(destAbs, linkTarget) {
				return fmt.Errorf("tar link %q points outside the extraction directory", hdr.Name)
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tar member %q has an unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

// True when the cleaned absolute path is dir itself or below it
func insideDir(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// True for symlink targets the host resolves from a filesystem root or drive rather than the link's directory
func tarLinkRooted(linkname string) bool {
	return filepath.IsAbs(linkname) || filepath.VolumeName(linkname) != "" || strings.HasPrefix(linkname, "/") || strings.HasPrefix(linkname, `\`)
}

// Copies one extracted remote file onto the host when it changed since the push, 1 when applied
func (m *FileSyncManager) applyStagedFile(stagedFile, remotePath string, mapping []syncFile, uploadOnly map[string]bool) (int, error) {
	log := toolsLogger(m.env)
	pushed, hasPushed := m.pushedHashes[remotePath]
	if hasPushed {
		digest, err := sha256File(stagedFile)
		if err != nil {
			return 0, err
		}
		if digest == pushed {
			return 0, nil
		}
	}
	hostPath := m.resolveHostPath(remotePath, mapping)
	if hostPath == "" {
		hostPath = m.inferHostPath(remotePath, mapping, uploadOnly)
		if hostPath == "" {
			log.Debug("sync_back: skipping remote file with no host mapping", "remote", remotePath)
			return 0, nil
		}
	}
	if uploadOnly[resolveHostPathString(hostPath)] {
		log.Debug("sync_back: skipping upload-only credential file", "remote", remotePath)
		return 0, nil
	}
	if hasPushed {
		if _, err := os.Stat(hostPath); err == nil {
			if digest, err := sha256File(hostPath); err == nil && digest != pushed {
				log.Warn("sync_back: conflict, host modified since push and remote also changed, applying the remote version", "remote", remotePath)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return 0, err
	}
	if err := copyFileWithTimes(stagedFile, hostPath); err != nil {
		return 0, err
	}
	return 1, nil
}

// Host path of a known remote path from the mapping
func (m *FileSyncManager) resolveHostPath(remotePath string, mapping []syncFile) string {
	for _, f := range mapping {
		if f.remote == remotePath {
			return f.host
		}
	}
	return ""
}

// Host path for a new remote file by prefix match on an existing pair's parent directory
func (m *FileSyncManager) inferHostPath(remotePath string, mapping []syncFile, uploadOnly map[string]bool) string {
	for _, f := range mapping {
		if uploadOnly[resolveHostPathString(f.host)] {
			continue
		}
		remoteDir := path.Dir(f.remote)
		if strings.HasPrefix(remotePath, remoteDir+"/") {
			return filepath.Join(filepath.Dir(f.host), filepath.FromSlash(remotePath[len(remoteDir)+1:]))
		}
	}
	return ""
}

// Home of the profile the manager serves, for the sync lock
func (m *FileSyncManager) homeDir() string {
	if m.env != nil && m.env.Home != "" {
		return m.env.Home
	}
	return home.Process()
}
