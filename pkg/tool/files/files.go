// File tools that read, write, edit, glob and grep inside an optional root
package files

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tool"
)

const (
	ReadName  = "Read"
	WriteName = "Write"
	EditName  = "Edit"
	GlobName  = "Glob"
	GrepName  = "Grep"
)

// Directory names skipped by Glob and Grep when Ignore is nil
var DefaultIgnore = []string{".git", ".hg", ".svn", "node_modules", ".venv", "venv", "__pycache__", ".cache", ".next", ".turbo"}

// Shared settings of every file tool
type Options struct {
	// Directory relative paths resolve against, Root when set and otherwise the process working directory
	Dir string
	// Directory every path must stay inside after symlink resolution, unrestricted when empty
	Root string
	// Directory names Glob and Grep skip, DefaultIgnore when nil
	Ignore []string
	// Walks files that .gitignore rules would hide
	NoGitIgnore bool
	// Lines one Read returns, 2000 when zero
	MaxLines int
	// Bytes kept of one line before it is cut, 2000 when zero
	MaxLineBytes int
	// Largest file Read loads whole and Edit rewrites, 8 MiB when zero
	MaxFileBytes int
	// Entries Glob and Grep list, 1000 when zero
	MaxResults int
	// Bytes of Grep output, 50000 when zero
	MaxOutput int
}

// Every file tool sharing one set of options
func All(o Options) []tool.Tool {
	return []tool.Tool{Read(o), Write(o), Edit(o), Glob(o), Grep(o)}
}

// Content replaced by Write or Edit, with the whole file before and after
type Change struct {
	Path     string
	Old      string
	New      string
	Created  bool
	Replaced int
}

// One path found by Glob
type Match struct {
	Path    string
	Dir     bool
	ModTime time.Time
}

// Structured outcome of Glob
type GlobData struct {
	Dir     string
	Pattern string
	Matches []Match
	Total   int
}

func (o Options) maxLines() int     { return or(o.MaxLines, 2000) }
func (o Options) maxLineBytes() int { return or(o.MaxLineBytes, 2000) }
func (o Options) maxFileBytes() int { return or(o.MaxFileBytes, 8<<20) }
func (o Options) maxResults() int   { return or(o.MaxResults, 1000) }
func (o Options) maxOutput() int    { return or(o.MaxOutput, 50000) }

func (o Options) ignore() []string {
	if o.Ignore == nil {
		return DefaultIgnore
	}
	return o.Ignore
}

func or(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func (o Options) dir() (string, error) {
	switch {
	case o.Dir != "":
		return filepath.Abs(o.Dir)
	case o.Root != "":
		return filepath.Abs(o.Root)
	}
	return os.Getwd()
}

// Turns a user path into a clean absolute path that lies inside the root
func (o Options) resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		base, err := o.dir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	if o.Root == "" {
		return p, nil
	}
	rootAbs, err := filepath.Abs(o.Root)
	if err != nil {
		return "", fmt.Errorf("root %s: %w", o.Root, err)
	}
	root, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("root %s: %w", o.Root, err)
	}
	real, err := realPath(p)
	if err != nil {
		return "", err
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s", p, rootAbs)
	}
	return p, nil
}

// Resolves symlinks in the existing part of p and keeps the missing tail as given
func realPath(p string) (string, error) {
	rest := ""
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, v)
}

// Reports whether the first bytes contain a NUL, the mark of a binary file
func isBinary(head []byte) bool {
	return bytes.IndexByte(head, 0) >= 0
}

// Reads up to 8 KiB from the start of the file for the binary check
func peek(f *os.File) ([]byte, error) {
	head := make([]byte, 8192)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return head[:n], nil
}

// Yields every line of r without its line ending
func lines(r io.Reader, fn func(line string) bool) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if line == "" && err == io.EOF {
			return nil
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if !fn(line) || err == io.EOF {
			return nil
		}
	}
}

// Cuts a line at the byte budget on a rune boundary and marks how much was dropped
func (o Options) clip(line string) string {
	limit := o.maxLineBytes()
	if len(line) <= limit {
		return line
	}
	for limit > 0 && !isRuneStart(line[limit]) {
		limit--
	}
	return fmt.Sprintf("%s [+%d bytes]", line[:limit], len(line)-limit)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Renders numbered lines in cat -n form starting at the given line number
func (o Options) numbered(start int, ls []string) string {
	var sb strings.Builder
	for i, l := range ls {
		fmt.Fprintf(&sb, "%6d\t%s\n", start+i, o.clip(l))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// Writes data to p through a temporary file, following a symlink and keeping the mode of an existing file
func writeFile(p string, data []byte) (created bool, err error) {
	mode := fs.FileMode(0o644)
	st, err := os.Lstat(p)
	switch {
	case err == nil && st.Mode()&fs.ModeSymlink != 0:
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			return false, err
		}
		p = target
		st, err = os.Stat(p)
		if err != nil {
			return false, err
		}
		if st.IsDir() {
			return false, fmt.Errorf("%s is a directory", p)
		}
		mode = st.Mode().Perm()
	case err == nil:
		if st.IsDir() {
			return false, fmt.Errorf("%s is a directory", p)
		}
		mode = st.Mode().Perm()
	case errors.Is(err, fs.ErrNotExist):
		created = true
	default:
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	return created, nil
}

// Reads the current content of p for a change record, empty when it does not exist
func current(p string) (string, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type found struct {
	path string
	mod  time.Time
}

// Visits every entry below dir in lexical order, skipping ignored directory names and gitignored paths
func (o Options) walk(dir string, dirs bool, visit func(path string, d fs.DirEntry) error) error {
	ignored := map[string]bool{}
	for _, name := range o.ignore() {
		ignored[name] = true
	}
	var rules ignoreStack
	if !o.NoGitIgnore {
		rules = loadIgnoreStack(dir)
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		if p == dir {
			return nil
		}
		isDir := d.IsDir()
		if isDir && ignored[d.Name()] {
			return fs.SkipDir
		}
		if !o.NoGitIgnore && rules.ignored(p, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if isDir {
			if !o.NoGitIgnore {
				rules.push(p)
			}
			if !dirs {
				return nil
			}
		}
		return visit(p, d)
	})
}

// Orders newest first, then by path for equal times
func byNewest(fs []found) {
	sort.SliceStable(fs, func(i, j int) bool {
		if !fs[i].mod.Equal(fs[j].mod) {
			return fs[i].mod.After(fs[j].mod)
		}
		return fs[i].path < fs[j].path
	})
}
