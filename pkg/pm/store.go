package pm

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"golang.org/x/text/encoding/charmap"
)

// JunkNames is inert desktop metadata that never decides a package's layout or digest
var JunkNames = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true, ".localized": true}

// AllTargets lists every platform target PM pins artifacts for
var AllTargets = []string{
	"win32-x64",
	"win32-arm64",
	"linux-x64",
	"linux-arm64",
	"linux-x64-musl",
	"linux-arm64-musl",
	"linux-arm64-bionic",
	"darwin-x64",
	"darwin-arm64",
}

// MuslTargets are the native musl userland targets
var MuslTargets = map[string]bool{"linux-x64-musl": true, "linux-arm64-musl": true}

// Native host architecture even under Rosetta or WOW64 emulation
var nativeMachine = func() string { return hostinfo.NativeArch() }

// Reports an Android bionic userland, a Go build for Android or a shell whose loader is the Android linker
var isBionicLibc = func() bool {
	if runtime.GOOS == "android" {
		return true
	}
	for _, candidate := range []string{"/system/bin/sh", "/data/data/com.termux/files/usr/bin/sh"} {
		if elfInterpreterContains(candidate, "/system/bin/linker") {
			return true
		}
	}
	return false
}

// Reports whether an ELF's first bytes name an interpreter containing marker
func elfInterpreterContains(binary, marker string) bool {
	head, ok := elfHead(binary)
	return ok && bytes.Contains(head, []byte(marker))
}

// First 8 KiB of a resolved ELF file, false when unreadable or not ELF
func elfHead(binary string) ([]byte, bool) {
	f, err := os.Open(resolvePath(binary))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if !bytes.HasPrefix(head, []byte("\x7fELF")) {
		return nil, false
	}
	return head, true
}

// Reads an ELF's loader string without executing it, known false when it names neither libc
func elfLoaderIsMusl(binary string) (musl bool, known bool) {
	head, ok := elfHead(binary)
	if !ok {
		return false, false
	}
	if bytes.Contains(head, []byte("ld-musl-")) {
		return true, true
	}
	if bytes.Contains(head, []byte("ld-linux")) {
		return false, true
	}
	return false, false
}

// Libc of the native userland judged from its shell and ls
var nativeLinuxUsesMusl = func() (bool, bool) {
	for _, candidate := range []string{"/bin/sh", "/bin/ls"} {
		if musl, known := elfLoaderIsMusl(candidate); known {
			return musl, true
		}
	}
	return false, false
}

// Reports a native musl userland, the running binary's own loader breaking ties before a musl loader on disk
var isMuslLibc = func() bool {
	if musl, known := nativeLinuxUsesMusl(); known {
		return musl
	}
	if exe, err := os.Executable(); err == nil {
		if musl, known := elfLoaderIsMusl(exe); known {
			return musl
		}
	}
	matches, _ := filepath.Glob("/lib/ld-musl-*.so.1")
	return len(matches) > 0
}

// Platform target of this host, the libc flavour part of the triple
func CurrentTarget() (string, error) {
	var arch string
	switch machine := strings.ToLower(nativeMachine()); machine {
	case "arm64", "aarch64":
		arch = "arm64"
	case "x86_64", "amd64", "x64":
		arch = "x64"
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
	switch sysPlatform() {
	case "win32":
		return "win32-" + arch, nil
	case "darwin":
		return "darwin-" + arch, nil
	}
	if isBionicLibc() {
		return "linux-" + arch + "-bionic", nil
	}
	if isMuslLibc() {
		return "linux-" + arch + "-musl", nil
	}
	return "linux-" + arch, nil
}

// Streams a URL's content into sha256, the way pm lock pins an artifact
func HashURL(rawURL string) (string, error) {
	return RetryNetwork(func() (string, error) {
		resp, err := openURL(rawURL, nil, 600*time.Second)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		declared, err := pyIntHeader(resp.Header.Get("Content-Length"))
		if err != nil {
			return "", err
		}
		h := sha256.New()
		size, err := io.CopyBuffer(h, resp.Body, make([]byte, downloadChunk))
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return "", err
		}
		if size < declared {
			return "", &IncompleteReadError{Partial: 0, Expected: declared - size}
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}, RetryOptions{})
}

// Integer value of a header the way int of the value or zero reads it, an empty header meaning zero
func pyIntHeader(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if !pyIntLiteral.MatchString(value) {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(value))
	}
	return strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(value), "_", ""), 10, 64)
}

// Tar member the data containment policy refuses, with the refusal kind and the offending path
type TarFilterError struct {
	Kind   string
	Member string
	Path   string
}

func (e *TarFilterError) Error() string {
	switch e.Kind {
	case "AbsolutePathError":
		return fmt.Sprintf("member %s has an absolute path", tarRepr(e.Member))
	case "OutsideDestinationError":
		return fmt.Sprintf("%s would be extracted to %s, which is outside the destination", tarRepr(e.Member), tarRepr(e.Path))
	case "SpecialFileError":
		return fmt.Sprintf("%s is a special file", tarRepr(e.Member))
	case "AbsoluteLinkError":
		return fmt.Sprintf("%s is a link to an absolute path", tarRepr(e.Member))
	case "LinkFallbackError":
		return fmt.Sprintf("link %s would be extracted as a copy of %s, which was rejected", tarRepr(e.Member), tarRepr(e.Path))
	}
	return fmt.Sprintf("%s would link to %s, which is outside the destination", tarRepr(e.Member), tarRepr(e.Path))
}

// Member failure tarfile reports as an ExtractError and skips instead of raising
type tarExtractError struct{ message string }

func (e *tarExtractError) Error() string { return e.message }

// Creates symlinks for archive extraction, replaceable by tests modelling a host without symlink support
var symlinkFile = os.Symlink

// Reports whether path is dest or lies beneath it, both already resolved
func withinDest(path, dest string) bool {
	return isRelativeTo(path, dest)
}

// Reports an absolute path the way os.path.isabs judges it on this platform
func pyIsAbs(p string) bool {
	if runtime.GOOS != "windows" {
		return strings.HasPrefix(p, "/")
	}
	head := strings.ReplaceAll(p[:min(3, len(p))], "/", `\`)
	return strings.HasPrefix(head[min(1, len(head)):], `:\`) || strings.HasPrefix(head, `\\`)
}

// Resolves symlinks the way realpath does under strict ALLOW_MISSING, tolerating only a missing component
func realpathAllowMissing(path string) (string, error) {
	return realpathMode(path, true)
}

// Member as it will be extracted after the containment filter, a nil mode leaving permissions alone
type filteredMember struct {
	member     *tarInfo
	name       string
	mode       *fs.FileMode
	link       string
	linkTarget *string
}

// Applies the hermes filter, symlinks judged from their own directory and every other member by the data filter
func tarFilter(m *tarInfo, dest string) (*filteredMember, error) {
	if !m.issym() {
		return tarDataFilter(m, dest)
	}
	if pyIsAbs(m.linkname) {
		return nil, &TarFilterError{Kind: "AbsoluteLinkError", Member: m.name}
	}
	name := strings.TrimRight(m.name, "/")
	placed, err := tarRealpath(pypath.Join(dest, name), false)
	if err != nil {
		return nil, err
	}
	target, err := tarRealpath(pypath.Join(pypath.Join(dest, pypath.Dirname(name)), m.linkname), false)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{placed, target} {
		if !withinDest(p, dest) {
			return nil, &TarFilterError{Kind: "LinkOutsideDestinationError", Member: m.name, Path: p}
		}
	}
	return &filteredMember{member: m, name: m.name, link: m.linkname}, nil
}

// Resolves a path for the containment checks, a NUL byte raising the ValueError Python lstat gives it
func tarRealpath(path string, allowMissing bool) (string, error) {
	nul := &TarReadError{Class: "ValueError", Message: "lstat: embedded null character in path"}
	if !allowMissing {
		if strings.IndexByte(path, 0) >= 0 {
			return "", nul
		}
		return realpath(path), nil
	}
	resolved, err := realpathAllowMissing(path)
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && strings.IndexByte(pathErr.Path, 0) >= 0 {
		return "", nul
	}
	return resolved, err
}

// The stdlib data filter for a non symlink member, normalizing names and links and refusing escapes and special files
func tarDataFilter(m *tarInfo, dest string) (*filteredMember, error) {
	name := m.name
	seps := "/" + string(filepath.Separator)
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, string(filepath.Separator)) {
		name = strings.TrimLeft(name, seps)
	}
	if pyIsAbs(name) {
		return nil, &TarFilterError{Kind: "AbsolutePathError", Member: m.name}
	}
	if slices.Contains(strings.Split(strings.ReplaceAll(name, string(filepath.Separator), "/"), "/"), "..") {
		name = pypath.Normpath(name)
	}
	targetPath, err := tarRealpath(pypath.Join(dest, name), true)
	if err != nil {
		return nil, err
	}
	if !withinDest(targetPath, dest) {
		return nil, &TarFilterError{Kind: "OutsideDestinationError", Member: m.name, Path: targetPath}
	}
	f := &filteredMember{member: m, name: name}
	mode := fs.FileMode(m.mode) & 0o755
	switch {
	case m.isreg() || m.islnk():
		if mode&0o100 == 0 {
			mode &^= 0o111
		}
		mode |= 0o600
		f.mode = &mode
	case m.isdir():
	default:
		return nil, &TarFilterError{Kind: "SpecialFileError", Member: m.name}
	}
	if !m.islnk() {
		return f, nil
	}
	if pyIsAbs(m.linkname) {
		return nil, &TarFilterError{Kind: "AbsoluteLinkError", Member: m.name}
	}
	if targetPath == dest {
		return nil, &TarFilterError{Kind: "OutsideDestinationError", Member: m.name, Path: targetPath}
	}
	f.link = pypath.Normpath(m.linkname)
	linked, err := tarRealpath(pypath.Join(dest, f.link), true)
	if err != nil {
		return nil, err
	}
	if !withinDest(linked, dest) {
		return nil, &TarFilterError{Kind: "LinkOutsideDestinationError", Member: m.name, Path: linked}
	}
	return f, nil
}

// Extracts a tarball file under the one containment policy every PM tar consumer shares
func ExtractTarFile(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return ExtractTar(f, dest)
}

// Extracts a seekable tar stream the way tarfile extractall does under the hermes filter, unsafe members refused
func ExtractTar(r io.ReadSeeker, dest string) error {
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	root := realpath(dest)
	tf, err := openTarFile(r)
	if err != nil {
		return err
	}
	defer tf.close()
	x := &tarExtraction{tf: tf, dest: dest, root: root}
	return x.extractAll()
}

// One extraction of an opened archive into dest, the resolved root judging containment
type tarExtraction struct {
	tf   *tarFile
	dest string
	root string
}

// Orders member names by code point the way Python compares the str names surrogateescape decoded
func tarNameLess(a, b string) bool {
	if utf8.ValidString(a) && utf8.ValidString(b) {
		return a < b
	}
	return slices.Compare(tarCodePoints(a), tarCodePoints(b)) < 0
}

// Extracts every member in order then restores directory times deepest name first
func (x *tarExtraction) extractAll() error {
	var dirs []*tarInfo
	err := x.tf.each(func(m *tarInfo) error {
		f, err := tarFilter(m, x.root)
		if err != nil {
			return err
		}
		if m.islnk() {
			target := pypath.Join(x.dest, f.link)
			f.linkTarget = &target
		}
		if m.isdir() {
			dirs = append(dirs, m)
		}
		return x.extractOne(f, !m.isdir())
	})
	if err != nil {
		return err
	}
	sort.SliceStable(dirs, func(i, j int) bool { return tarNameLess(dirs[j].name, dirs[i].name) })
	for _, d := range dirs {
		f, err := tarFilter(d, x.root)
		var uncaught *TarReadError
		if errors.As(err, &uncaught) {
			return err
		}
		if err != nil {
			continue
		}
		dirpath := pypath.Join(x.dest, f.name)
		st, err := os.Lstat(dirpath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() {
			continue
		}
		var skipped *tarExtractError
		if err := tarUtime(d.mtime, dirpath); err != nil && !errors.As(err, &skipped) {
			return err
		}
	}
	return nil
}

// Extracts one filtered member, dropping a skippable ExtractError and raising every other failure
func (x *tarExtraction) extractOne(f *filteredMember, setAttrs bool) error {
	err := x.extractMember(f, pypath.Join(x.dest, f.name), setAttrs)
	var skipped *tarExtractError
	if errors.As(err, &skipped) {
		return nil
	}
	return err
}

// Writes a filtered member at targetpath creating missing parents, then applies its mode and time
func (x *tarExtraction) extractMember(f *filteredMember, targetpath string, setAttrs bool) error {
	targetpath = strings.ReplaceAll(strings.TrimRight(targetpath, "/"), "/", string(filepath.Separator))
	if upper := pypath.Dirname(targetpath); upper != "" {
		if _, err := os.Stat(upper); err != nil {
			if err := os.MkdirAll(upper, 0o777); err != nil {
				return err
			}
		}
	}
	m := f.member
	switch {
	case m.isreg():
		if err := x.makefile(m, targetpath); err != nil {
			return err
		}
	case m.isdir():
		if err := os.Mkdir(targetpath, 0o777); err != nil {
			if st, serr := os.Stat(targetpath); !errors.Is(err, fs.ErrExist) || serr != nil || !st.IsDir() {
				return err
			}
		}
	default:
		if err := x.makeLink(f, targetpath); err != nil {
			return err
		}
	}
	if !setAttrs || m.issym() {
		return nil
	}
	if f.mode != nil {
		if err := os.Chmod(targetpath, *f.mode); err != nil {
			return &tarExtractError{message: "could not change mode"}
		}
	}
	return tarUtime(m.mtime, targetpath)
}

// Writes member data from its data offset, sparse runs placed at their offsets and the file cut to size
func (x *tarExtraction) makefile(m *tarInfo, targetpath string) error {
	if err := x.tf.stream.seek(m.offsetData); err != nil {
		return err
	}
	out, err := os.OpenFile(targetpath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	err = x.writeData(m, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Copies plain data, or each sparse run at its own offset before truncating to the restored size
func (x *tarExtraction) writeData(m *tarInfo, out *os.File) error {
	if m.sparse == nil {
		return x.copyData(out, m.size)
	}
	for _, run := range m.sparse {
		if err := tarSeekFile(out, run.offset); err != nil {
			return err
		}
		if err := x.copyData(out, run.numbytes); err != nil {
			return err
		}
	}
	if err := tarSeekFile(out, m.size); err != nil {
		return err
	}
	return out.Truncate(m.size.Int64())
}

// Moves a file being written to an absolute offset, one past the C offset range a ValueError as in Python
func tarSeekFile(out *os.File, pos *big.Int) error {
	if !pos.IsInt64() {
		return &TarReadError{Class: "ValueError", Message: "cannot fit 'int' into an offset-sized integer"}
	}
	_, err := out.Seek(pos.Int64(), io.SeekStart)
	return err
}

// Copies length bytes in copyfileobj blocks, a short read a ReadError and a negative length its floor remainder
func (x *tarExtraction) copyData(dst io.Writer, length *big.Int) error {
	if length.Sign() == 0 {
		return nil
	}
	blocks, remainder := new(big.Int).DivMod(length, big.NewInt(tarCopyBufSize), new(big.Int))
	step := func(n int) error {
		buf, err := x.tf.stream.read(n)
		if err != nil {
			return err
		}
		if len(buf) < n {
			return tarReadError("unexpected end of data")
		}
		_, err = dst.Write(buf)
		return err
	}
	for i := new(big.Int); i.Cmp(blocks) < 0; i.Add(i, big.NewInt(1)) {
		if err := step(tarCopyBufSize); err != nil {
			return err
		}
	}
	if remainder.Sign() != 0 {
		return step(int(remainder.Int64()))
	}
	return nil
}

// Seconds and nanoseconds os.utime derives from a member time, flooring floats and refusing NaN and out of range values
func tarTimespec(m tarMtime) (int64, int64, error) {
	overflow := &TarReadError{Class: "OverflowError", Message: "timestamp out of range for platform time_t"}
	if !m.isFloat {
		if !m.i.IsInt64() {
			return 0, 0, overflow
		}
		return m.i.Int64(), 0, nil
	}
	if math.IsNaN(m.f) {
		return 0, 0, &TarReadError{Class: "ValueError", Message: "Invalid value NaN (not a number)"}
	}
	whole, frac := math.Modf(m.f)
	frac = math.Floor(frac * 1e9)
	if frac >= 1e9 {
		frac -= 1e9
		whole++
	} else if frac < 0 {
		frac += 1e9
		whole--
	}
	if !(whole >= -9223372036854775808.0 && whole <= 9223372036854775808.0) {
		return 0, 0, overflow
	}
	return int64(whole), int64(frac), nil
}

// Sets both times to the member's archived mtime, a failure to set them being a skippable ExtractError
func tarUtime(mtime tarMtime, path string) error {
	sec, nsec, err := tarTimespec(mtime)
	if err != nil {
		return err
	}
	if err := tarSetFileTimes(path, sec, nsec); err != nil {
		var refused *TarReadError
		if errors.As(err, &refused) {
			return err
		}
		return &tarExtractError{message: "could not change modification time"}
	}
	return nil
}

// Places a symlink or hard link, else a filtered copy of the member it names the way makelink_with_filter does
func (x *tarExtraction) makeLink(f *filteredMember, targetpath string) error {
	failed := false
	switch {
	case f.member.issym():
		if err := placeSymlink(f.link, targetpath); err == nil {
			return nil
		}
		failed = true
	case f.linkTarget == nil:
		failed = true
	default:
		if _, err := os.Stat(*f.linkTarget); err == nil {
			if err := placeHardlink(*f.linkTarget, targetpath); err == nil {
				return nil
			}
			failed = true
		}
	}
	unfiltered, linkname, err := x.findLinkTarget(f)
	if err != nil {
		return err
	}
	if unfiltered == nil {
		if failed {
			return &tarExtractError{message: "unable to resolve link inside archive"}
		}
		return &TarReadError{Class: "KeyError", Message: tarRepr(fmt.Sprintf("linkname %s not found", tarRepr(linkname)))}
	}
	renamed := *unfiltered
	renamed.name = f.name
	var filtered *filteredMember
	if _, err = tarFilter(&renamed, x.root); err == nil {
		filtered, err = tarFilter(unfiltered, x.root)
	}
	var uncaught *TarReadError
	if errors.As(err, &uncaught) {
		return err
	}
	if err != nil {
		return &TarFilterError{Kind: "LinkFallbackError", Member: f.name, Path: unfiltered.name}
	}
	return x.extractMember(filtered, targetpath, true)
}

// Last member whose normalized name a link names, a symlink searching the whole archive and a hard link earlier members
func (x *tarExtraction) findLinkTarget(f *filteredMember) (*tarInfo, string, error) {
	name, limit := f.link, f.member
	if f.member.issym() {
		var parts []string
		for _, p := range []string{pypath.Dirname(f.name), f.link} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		name, limit = strings.Join(parts, "/"), nil
	}
	members, err := x.tf.getmembers()
	if err != nil {
		return nil, name, err
	}
	want, skipping := pypath.Normpath(name), limit != nil
	for i := len(members) - 1; i >= 0; i-- {
		if skipping {
			skipping = members[i].offset.Cmp(limit.offset) != 0
			continue
		}
		if pypath.Normpath(members[i].name) == want {
			return members[i], name, nil
		}
	}
	return nil, name, nil
}

// Replaces whatever non directory sits at path with a symlink to target
func placeSymlink(target, path string) error {
	if _, err := os.Lstat(path); err == nil {
		if err := unlinkPath(path); err != nil {
			return err
		}
	}
	return symlinkFile(target, path)
}

// Replaces whatever non directory sits at path with a hard link to the resolved existing file
func placeHardlink(existing, path string) error {
	if _, err := os.Lstat(path); err == nil {
		if err := unlinkPath(path); err != nil {
			return err
		}
	}
	return os.Link(realpath(existing), path)
}

// Empties dest and extracts a tar or zip archive chosen by its file name
func Extract(archive, dest string) error {
	removeTreeQuiet(dest)
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	name := strings.ToLower(filepath.Base(archive))
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2"} {
		if strings.HasSuffix(name, suffix) {
			return ExtractTarFile(archive, dest)
		}
	}
	if strings.HasSuffix(name, ".zip") {
		return extractZip(archive, dest)
	}
	return fmt.Errorf("unsupported archive: %s", filepath.Base(archive))
}

// Zip member name decoded the way zipfile does, CP437 unless the UTF-8 flag is set, cut at a NUL
func zipMemberName(f *zip.File) string {
	name := f.Name
	if f.NonUTF8 {
		if decoded, err := charmap.CodePage437.NewDecoder().String(name); err == nil {
			name = decoded
		}
	}
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	if filepath.Separator != '/' {
		name = strings.ReplaceAll(name, string(filepath.Separator), "/")
	}
	return name
}

// Sanitized extraction path of a zip member the way zipfile extract computes it
func zipTargetPath(member, dest string, isDir bool) (string, error) {
	arcname := strings.ReplaceAll(member, "/", string(filepath.Separator))
	arcname = arcname[len(filepath.VolumeName(arcname)):]
	var parts []string
	for _, part := range strings.Split(arcname, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			continue
		}
		if runtime.GOOS == "windows" {
			part = strings.Map(func(r rune) rune {
				if strings.ContainsRune(`:<>|"?*`, r) {
					return '_'
				}
				return r
			}, part)
			part = strings.TrimRight(part, ". ")
			if part == "" {
				continue
			}
		}
		parts = append(parts, part)
	}
	arcname = strings.Join(parts, string(filepath.Separator))
	if arcname == "" && !isDir {
		return "", errors.New("Empty filename.")
	}
	return filepath.Clean(pypath.Join(dest, arcname)), nil
}

// Extracts a zip, restoring executable modes and placing in root symlinks last
func extractZip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	type link struct{ member, target string }
	var symlinks []link
	for _, f := range zr.File {
		mode := fs.FileMode(f.ExternalAttrs >> 16)
		member := zipMemberName(f)
		if mode&0o170000 == 0o120000 {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			symlinks = append(symlinks, link{member, string(data)})
			continue
		}
		isDir := strings.HasSuffix(member, "/")
		written, err := zipTargetPath(member, dest, isDir)
		if err != nil {
			return err
		}
		if parent := filepath.Dir(written); parent != "" {
			if _, err := os.Stat(parent); err != nil {
				if err := os.MkdirAll(parent, 0o777); err != nil {
					return err
				}
			}
		}
		if isDir {
			if st, err := os.Stat(written); err != nil || !st.IsDir() {
				if err := os.Mkdir(written, 0o777); err != nil {
					if st, serr := os.Stat(written); serr != nil || !st.IsDir() {
						return err
					}
				}
			}
			continue
		}
		if err := writeZipMember(f, written); err != nil {
			return err
		}
		if mode&0o111 != 0 {
			if st, err := os.Stat(written); err == nil && st.Mode().IsRegular() {
				if err := os.Chmod(written, mode&0o777); err != nil {
					return err
				}
			}
		}
	}
	for _, l := range symlinks {
		if err := zipSymlink(l.member, l.target, dest); err != nil {
			return err
		}
	}
	return nil
}

// Streams one zip member's bytes to its extraction path
func writeZipMember(f *zip.File, path string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Places a zip symlink whose path and target both stay inside dest, else skips it
func zipSymlink(member, target, dest string) error {
	root := realpath(dest)
	link := realpath(pypath.Join(root, filepath.FromSlash(member)))
	if !isRelativeTo(link, root) {
		return nil
	}
	if pyIsAbs(target) || filepath.IsAbs(target) {
		return nil
	}
	resolved := realpath(pypath.Join(filepath.Dir(link), target))
	if !isRelativeTo(resolved, root) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o777); err != nil {
		return err
	}
	if err := symlinkFile(target, link); err != nil {
		return os.WriteFile(link, []byte(target), 0o666)
	}
	return nil
}

// Reports inert desktop metadata, only a regular non executable file carrying a junk name
func isOSMetadata(path string) (bool, error) {
	name := filepath.Base(path)
	if !JunkNames[name] && !strings.HasPrefix(name, "._") {
		return false, nil
	}
	st, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return st.Mode().IsRegular() && st.Mode().Perm()&0o111 == 0, nil
}

// Unlinks the duplicate metadata a flatten replaces, replaceable by tests modelling a held file
var unlinkFile = unlinkPath

// Hoists a lone top level directory's contents unless it is the layout, refusing on any collision
func FlattenSingleDir(dest string) error {
	keep := map[string]bool{"bin": true, "cmd": true, "lib": true, "libexec": true, "share": true, "etc": true, "usr": true}
	all, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	var entries []string
	for _, e := range all {
		path := filepath.Join(dest, e.Name())
		meta, err := isOSMetadata(path)
		if err != nil {
			return err
		}
		if !meta {
			entries = append(entries, path)
		}
	}
	if len(entries) != 1 || keep[filepath.Base(entries[0])] {
		return nil
	}
	if st, err := os.Stat(entries[0]); err != nil || !st.IsDir() {
		return nil
	}
	inner := entries[0]
	items, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	for _, item := range items {
		target := filepath.Join(dest, item.Name())
		if _, err := os.Lstat(target); err == nil {
			itemMeta, err := isOSMetadata(filepath.Join(inner, item.Name()))
			if err != nil {
				return err
			}
			targetMeta, err := isOSMetadata(target)
			if err != nil {
				return err
			}
			if !(itemMeta && targetMeta) {
				return nil
			}
		}
	}
	for _, item := range items {
		source := filepath.Join(inner, item.Name())
		target := filepath.Join(dest, item.Name())
		if _, err := os.Lstat(target); err == nil {
			if err := RetryHeld(func() error { return unlinkFile(target) }); err != nil {
				return err
			}
		}
		if err := RetryHeld(func() error { return renameFile(source, target) }); err != nil {
			return err
		}
	}
	return RetryHeld(func() error { return os.Remove(inner) })
}

// Moves src's files into dst keeping both layouts, a file present in both failing loudly
func MergeTree(src, dst string) error {
	var items []string
	if err := walkNoFollow(src, func(path string) { items = append(items, path) }); err != nil {
		return err
	}
	sort.Strings(items)
	for _, item := range items {
		if st, err := os.Stat(item); err == nil && st.IsDir() {
			continue
		}
		rel, err := filepath.Rel(src, item)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("archives disagree about %s", rel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		if err := RetryHeld(func() error { return os.Rename(item, target) }); err != nil {
			return err
		}
	}
	return nil
}

// Collects every path under root recursively without descending into directory symlinks
func walkNoFollow(root string, visit func(string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		visit(path)
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 && !IsJunction(path) {
			if err := walkNoFollow(path, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// Deterministic sha256 over a tree, skipping desktop metadata and bytecode caches
func TreeDigest(root string) (string, error) {
	return treeDigest(root, true)
}

// Reports whether a tree matches a recorded digest, including one recorded before metadata was skipped
func DigestMatches(root string, recorded *string) (bool, error) {
	if recorded == nil {
		return false, nil
	}
	current, err := TreeDigest(root)
	if err != nil {
		return false, err
	}
	if current == *recorded {
		return true, nil
	}
	legacy, err := treeDigest(root, false)
	if err != nil {
		return false, err
	}
	return legacy == *recorded, nil
}

// Hashes sorted relpath NUL content records, symlinks and junctions contributing their link text
func treeDigest(root string, skipMetadata bool) (string, error) {
	type item struct{ rel, path string }
	var files []item
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var dirNames, fileNames []string
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			isDir := e.IsDir()
			if e.Type()&fs.ModeSymlink != 0 {
				if st, err := os.Stat(path); err == nil && st.IsDir() {
					isDir = true
				}
			}
			if isDir {
				dirNames = append(dirNames, e.Name())
			} else {
				fileNames = append(fileNames, e.Name())
			}
		}
		sort.Strings(dirNames)
		var descend []string
		for _, name := range dirNames {
			path := filepath.Join(dir, name)
			if st, err := os.Lstat(path); (err == nil && st.Mode()&fs.ModeSymlink != 0) || IsJunction(path) {
				rel, _ := filepath.Rel(root, path)
				files = append(files, item{filepath.ToSlash(rel), path})
			} else if name != "__pycache__" {
				descend = append(descend, path)
			}
		}
		for _, name := range fileNames {
			path := filepath.Join(dir, name)
			if skipMetadata {
				if meta, err := isOSMetadata(path); err == nil && meta {
					continue
				}
			}
			rel, _ := filepath.Rel(root, path)
			files = append(files, item{filepath.ToSlash(rel), path})
		}
		for _, d := range descend {
			walk(d)
		}
	}
	walk(root)
	sort.SliceStable(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	h := sha256.New()
	buf := make([]byte, downloadChunk)
	for _, f := range files {
		h.Write([]byte(f.rel))
		h.Write([]byte{0})
		st, err := os.Lstat(f.path)
		if (err == nil && st.Mode()&fs.ModeSymlink != 0) || IsJunction(f.path) {
			target, err := os.Readlink(f.path)
			if err != nil {
				return "", err
			}
			h.Write([]byte(target))
			continue
		}
		in, err := os.Open(f.path)
		if err != nil {
			return "", err
		}
		_, err = io.CopyBuffer(h, in, buf)
		in.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Store is one directory of immutable published entries plus a scratch area
type Store struct {
	Root string
}

// Store rooted at root, spelled long on Windows
func NewStore(root string) *Store {
	return &Store{Root: LongRoot(root)}
}

// Path of a named entry
func (s *Store) Entry(name string) string { return filepath.Join(s.Root, name) }

// Reports whether a named entry is published
func (s *Store) Published(name string) bool {
	st, err := os.Stat(s.Entry(name))
	return err == nil && st.IsDir()
}

// Artifact is one pinned archive of a fetch plan
type Artifact struct {
	URL    string
	SHA256 string
}

// Fetches one pinned archive into the hash keyed cache
func (s *Store) Fetch(rawURL, sha, scratch string, progress func(done, total int64) error, pause *PauseEvent) (string, error) {
	var tick ProgressFunc
	if progress != nil {
		tick = func(done, total int64, _ []RangeSet) error { return progress(done, total) }
	}
	paths, err := s.FetchMany([]Artifact{{URL: rawURL, SHA256: sha}}, scratch, tick, pause)
	if err != nil {
		return "", err
	}
	return paths[0], nil
}

// Fetches a pinned plan with one aggregate progress stream, completed archives entering the cache even on a later pause
func (s *Store) FetchMany(artifacts []Artifact, scratch string, progress ProgressFunc, pause *PauseEvent) ([]string, error) {
	var sources []Source
	for _, artifact := range artifacts {
		if pause != nil && pause.IsSet() {
			return nil, downloadError(DownloadWasPaused, "download paused")
		}
		entryName := "fetch-" + artifact.SHA256
		entry := s.Entry(entryName)
		var files []os.DirEntry
		if st, err := os.Stat(entry); err == nil && st.IsDir() {
			files, err = os.ReadDir(entry)
			if err != nil {
				return nil, err
			}
		}
		var destination string
		if len(files) == 1 && isRegularFile(filepath.Join(entry, files[0].Name())) {
			destination = filepath.Join(entry, files[0].Name())
		} else {
			if _, err := os.Stat(entry); err == nil {
				if err := RemoveTree(entry); err != nil {
					return nil, err
				}
			}
			base := artifact.URL[strings.LastIndex(artifact.URL, "/")+1:]
			destination = filepath.Join(scratch, entryName, base)
		}
		source, err := PinnedSource(artifact.URL, destination, artifact.SHA256)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	urls := map[string]string{}
	for i, source := range sources {
		urls[source.Dest] = artifacts[i].URL
	}
	tick := func(done, total int64, ranges []RangeSet) error {
		if progress == nil {
			return nil
		}
		mapped := make([]RangeSet, len(ranges))
		for i, rs := range ranges {
			mapped[i] = RangeSet{Key: urls[rs.Key], Ranges: rs.Ranges}
		}
		return progress(done, total, mapped)
	}
	_, runErr := NewDownload(sources, DownloadOptions{Pause: pause}).Run(tick)
	for _, source := range sources {
		if isRelativeTo(source.Dest, scratch) && isRegularFile(source.Dest) {
			if _, err := s.Publish(filepath.Dir(source.Dest), "fetch-"+source.SHA256); err != nil {
				return nil, err
			}
		}
	}
	if runErr != nil {
		return nil, runErr
	}
	out := make([]string, len(sources))
	for i, source := range sources {
		out[i] = filepath.Join(s.Entry("fetch-"+source.SHA256), filepath.Base(source.Dest))
	}
	return out, nil
}

// Reports whether path is a regular file, following symlinks
func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Creates a fresh staging directory inside the store and returns it with its cleanup
func (s *Store) Scratch() (string, func(), error) {
	if err := os.MkdirAll(s.Root, 0o777); err != nil {
		return "", nil, err
	}
	path, err := os.MkdirTemp(s.Root, ".staging-")
	if err != nil {
		return "", nil, err
	}
	return path, func() { removeTreeQuiet(path) }, nil
}

// Renames a staged tree into place atomically, keeping a concurrent winner's entry
func (s *Store) Publish(staged, name string) (string, error) {
	target := s.Entry(name)
	delay := 500 * time.Millisecond
	for i := 0; i < 5; i++ {
		if err := renameFile(staged, target); err == nil {
			return target, nil
		}
		if st, err := os.Stat(target); err == nil && st.IsDir() {
			return target, nil
		}
		time.Sleep(delay)
		delay *= 2
	}
	if err := renameFile(staged, target); err != nil {
		if st, serr := os.Stat(target); serr != nil || !st.IsDir() {
			return "", err
		}
	}
	return target, nil
}

// Message printed while a second PM operation waits on the store's install lock
var installLockWaitOutput io.Writer = os.Stderr

// Serializes writers on the store's advisory lock, saying what it waits on after two seconds
func (s *Store) InstallLock() (func(), error) {
	if err := os.MkdirAll(s.Root, 0o777); err != nil {
		return nil, err
	}
	lock := filepath.Join(s.Root, ".install.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := LockFD(f, true, 2*time.Second)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !held {
		fmt.Fprintf(installLockWaitOutput, "waiting for %s (another PM operation holds it)\n", lock)
		if _, err := LockFD(f, true, WaitForever); err != nil {
			f.Close()
			return nil, err
		}
	}
	return func() { f.Close() }, nil
}
