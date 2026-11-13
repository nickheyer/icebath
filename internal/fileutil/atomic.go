package fileutil

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Permission bits a write carries, the twelve POSIX bits including setuid, setgid and sticky
const permBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// Error for a second Commit, or a Commit after Discard, of one staged file
var ErrStagedFinished = errors.New("staged file already committed or discarded")

// Tunes how Stage and the write functions publish a file
type WriteOption func(*writeConfig)

// Settings the write options collect over the defaults of the function taking them
type writeConfig struct {
	perm       fs.FileMode
	permSet    bool
	createPerm fs.FileMode
	createSet  bool
	keepPerm   bool
	keepOwner  bool
	umaskNew   bool
	private    bool
	syncDir    bool
	prefix     string
	suffix     string
}

// Forces the published file to mode m whether or not the target existed before
func Perm(m fs.FileMode) WriteOption {
	return func(c *writeConfig) { c.perm, c.permSet = m&permBits, true }
}

// Mode a target that does not exist yet receives in place of the default for a new file
func CreatePerm(m fs.FileMode) WriteOption {
	return func(c *writeConfig) { c.createPerm, c.createSet = m&permBits, true }
}

// Carries the permission bits of an existing target over to its replacement
func KeepPerm() WriteOption { return func(c *writeConfig) { c.keepPerm = true } }

// Gives a replaced file back to the POSIX owner and group it had when the process may chown
func KeepOwner() WriteOption { return func(c *writeConfig) { c.keepOwner = true } }

// Publishes owner-only, mode 0600 on POSIX and an owner, SYSTEM and Administrators DACL on Windows from creation
func Private() WriteOption {
	return func(c *writeConfig) { c.perm, c.permSet, c.private = 0o600, true, true }
}

// Flushes the directory entry after the rename, a filesystem refusing the flush leaving the published file standing
func SyncDir() WriteOption { return func(c *writeConfig) { c.syncDir = true } }

// Name prefix of the temporary file staged beside the target
func TempPrefix(p string) WriteOption { return func(c *writeConfig) { c.prefix = p } }

// Name suffix of the temporary file staged beside the target
func TempSuffix(s string) WriteOption { return func(c *writeConfig) { c.suffix = s } }

// Mode and owner a published file must end with
type metaPlan struct {
	mode    fs.FileMode
	modeSet bool
	owner   *fileOwner
}

// Temporary file beside its target that Commit publishes atomically and Discard throws away
type Staged struct {
	file     *os.File
	target   string
	plan     metaPlan
	syncDir  bool
	private  bool
	finished bool
}

// Opens a temporary file beside target that publishes owner-only 0600 on POSIX unless an option sets the mode
func Stage(target string, opts ...WriteOption) (*Staged, error) {
	return stage(target, newWriteConfig(writeConfig{prefix: ".tmp_"}, opts))
}

// Option set of base with the .tmp temp suffix and opts applied over it
func newWriteConfig(base writeConfig, opts []WriteOption) writeConfig {
	base.suffix = ".tmp"
	for _, o := range opts {
		o(&base)
	}
	return base
}

// Creates missing parents, plans the final metadata and opens the temporary file with its mode applied
func stage(target string, cfg writeConfig) (*Staged, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return nil, err
	}
	plan, err := planMetadata(target, cfg)
	if err != nil {
		return nil, err
	}
	f, err := createTempBeside(target, cfg)
	if err != nil {
		return nil, err
	}
	if err := applyStagedMode(f, plan); err != nil {
		name := f.Name()
		return nil, joinErr(err, f.Close(), os.Remove(name))
	}
	return &Staged{file: f, target: target, plan: plan, syncDir: cfg.syncDir, private: cfg.private}, nil
}

// Final mode and owner for target under the options, from the existing file when there is one
func planMetadata(target string, cfg writeConfig) (metaPlan, error) {
	var plan metaPlan
	fi, err := os.Stat(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return plan, err
	}
	exists := err == nil
	switch {
	case cfg.permSet:
		plan.mode, plan.modeSet = cfg.perm, true
	case exists && cfg.keepPerm:
		plan.mode, plan.modeSet = fi.Mode()&permBits, true
	case exists:
		plan.mode, plan.modeSet = stagedTempMode()
	case cfg.createSet:
		plan.mode, plan.modeSet = cfg.createPerm, true
	case cfg.umaskNew:
		plan.mode, plan.modeSet = defaultNewFileMode()
	default:
		plan.mode, plan.modeSet = stagedTempMode()
	}
	if cfg.keepOwner && exists {
		plan.owner = ownerOf(fi)
	}
	return plan, nil
}

// Temporary file in the directory the replace publishes into, beside the link when that one refuses
func createTempBeside(target string, cfg writeConfig) (*os.File, error) {
	linkDir := filepath.Dir(target)
	published, err := publishPath(target)
	if err != nil {
		return nil, err
	}
	stageDir := filepath.Dir(published)
	f, err := createTemp(stageDir, cfg)
	if err != nil && errors.Is(err, fs.ErrPermission) && !samePathText(stageDir, linkDir) {
		return createTemp(linkDir, cfg)
	}
	return f, err
}

// Open handle the staged bytes go to
func (s *Staged) File() *os.File { return s.file }

// Appends p to the staged file
func (s *Staged) Write(p []byte) (int, error) { return s.file.Write(p) }

// Flushes the staged bytes to stable storage, publishes them over the target and returns the path written
func (s *Staged) Commit(ctx context.Context) (string, error) {
	if s.finished {
		return "", ErrStagedFinished
	}
	s.finished = true
	name := s.file.Name()
	if err := s.file.Sync(); err != nil {
		return "", joinErr(err, s.file.Close(), os.Remove(name))
	}
	if err := s.file.Close(); err != nil {
		return "", joinErr(err, os.Remove(name))
	}
	published, inPlace, err := replace(ctx, name, s.target, s.syncDir, s.private)
	if err != nil {
		if rmErr := os.Remove(name); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return "", joinErr(err, rmErr)
		}
		return "", err
	}
	s.plan.restore(published, inPlace)
	if s.syncDir && (inPlace || !renameWritesThrough) {
		flushPublishedDir(filepath.Dir(published))
	}
	return published, nil
}

// Flushes the directory a commit published into, the commit standing whatever the filesystem answers
func flushPublishedDir(dir string) { _ = FsyncDir(dir) }

// Closes and deletes the staged file, leaving the target untouched
func (s *Staged) Discard() error {
	if s.finished {
		return nil
	}
	s.finished = true
	name := s.file.Name()
	return errors.Join(s.file.Close(), os.Remove(name))
}

// Reapplies owner then mode after publishing, restoring special bits a write cleared, a refusal leaving the bytes published
func (p metaPlan) restore(path string, inPlace bool) {
	restoreOwner(path, p.owner)
	if p.modeSet && (!inPlace || mayChmod(path)) {
		_ = os.Chmod(path, p.mode)
	}
}

// Atomically replaces path with data, an existing mode kept and a new file given the umask default
func WriteFile(ctx context.Context, path string, data []byte, opts ...WriteOption) error {
	return writeStaged(ctx, path, data, newWriteConfig(writeConfig{prefix: ".tmp_", keepPerm: true, umaskNew: true}, opts))
}

// Stages data under cfg and commits it, discarding the temporary file when the write fails
func writeStaged(ctx context.Context, path string, data []byte, cfg writeConfig) error {
	s, err := stage(path, cfg)
	if err != nil {
		return err
	}
	if _, err := s.file.Write(data); err != nil {
		return joinErr(err, s.Discard())
	}
	_, err = s.Commit(ctx)
	return err
}

// Moves a finished temporary file over target the way Commit does and returns the path written
func Replace(ctx context.Context, tmp, target string) (string, error) {
	published, _, err := replace(ctx, tmp, target, false, false)
	return published, err
}

// Pushes bytes rewritten in place toward stable storage, the bytes already visible whatever the device answers
func flushKept(f *os.File) { _ = f.Sync() }

// Copies tmp over target through the target inode secured like tmp before any byte lands, then unlinks tmp
func copyOver(tmp, target string, private bool) error {
	src, err := os.Open(tmp)
	if err != nil {
		return err
	}
	info, err := src.Stat()
	if err != nil {
		return joinErr(err, src.Close())
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE, 0o666)
	if err != nil {
		return joinErr(err, src.Close())
	}
	if err := secureKeptInode(dst, target, info.Mode()&permBits, private); err != nil {
		return joinErr(err, dst.Close(), src.Close())
	}
	if err := dst.Truncate(0); err != nil {
		return joinErr(err, dst.Close(), src.Close())
	}
	if _, err := io.Copy(dst, src); err != nil {
		return joinErr(err, dst.Close(), src.Close())
	}
	flushKept(dst)
	if err := errors.Join(dst.Close(), src.Close()); err != nil {
		return err
	}
	return os.Remove(tmp)
}
