package pm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/klauspost/compress/zstd"
)

// InstallError names the package that failed, why, and what the user can do about it
type InstallError struct {
	Package string
	Cause   string
	Remedy  string
}

// Builds an install error with the default remedy when none is given
func NewInstallError(pkg, cause, remedy string) *InstallError {
	if remedy == "" {
		remedy = "retry, or run `icebath pm doctor`"
	}
	return &InstallError{Package: pkg, Cause: cause, Remedy: remedy}
}

func (e *InstallError) Error() string {
	return fmt.Sprintf("%s: %s — %s", e.Package, e.Cause, e.Remedy)
}

// EnvDiff is one package's environment contribution, PATH holding a list of directories to prepend
type EnvDiff map[string]any

// Composes package env diffs over base, dependents winning and PATH dirs prepended in their order
func ComposeEnv(diffs []EnvDiff, base map[string]string) map[string]string {
	env := map[string]string{}
	if base == nil {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	} else {
		for k, v := range base {
			env[k] = v
		}
	}
	var pathDirs []string
	for i := len(diffs) - 1; i >= 0; i-- {
		for _, key := range sortedKeys(diffs[i]) {
			if key != "PATH" {
				continue
			}
			for _, d := range envDirs(diffs[i][key]) {
				if n := Native(d); !slices.Contains(pathDirs, n) {
					pathDirs = append(pathDirs, n)
				}
			}
		}
	}
	for _, diff := range diffs {
		for key, value := range diff {
			if key != "PATH" {
				env[key] = Native(pyrepr.Str(value))
			}
		}
	}
	if len(pathDirs) > 0 {
		key := "PATH"
		for _, k := range sortedKeys(env) {
			if strings.ToUpper(k) == "PATH" {
				key = k
				break
			}
		}
		prefix := strings.Join(pathDirs, string(os.PathListSeparator))
		if existing := env[key]; existing != "" {
			env[key] = prefix + string(os.PathListSeparator) + existing
		} else {
			env[key] = prefix
		}
	}
	return env
}

// Directories of a PATH value, a scalar standing for a one item list
func envDirs(value any) []string {
	if list, ok := envList(value); ok {
		return list
	}
	return []string{pyrepr.Str(value)}
}

// Package is what a package IS, versions and hashes living in lock.json
type Package interface {
	Base() *BasePackage
	MissingReason(target string) string
	LatestVersions(target, locked string) ([]string, error)
	FetchURL(version, target string) (string, error)
	FetchURLs(version, target string) ([]string, error)
	StoreEntry(version, target string) string
	KnownSHA256(version, url string) (string, error)
	Unpack(archive, staged, target string) error
	Stage(store *Store, staged, version, target string) error
	Binary(entry, target string) string
	Verify(entry, target string) string
	RepairStagedVerification(entry, target, reason string) (string, string)
	Env(entry, target string) EnvDiff
}

// BasePackage carries the declarative fields and default behaviour every catalogue package embeds
type BasePackage struct {
	Name                string
	Deps                []string
	Optional            bool
	Default             bool
	Internal            bool
	OffPath             bool
	URL                 string
	Gaps                map[string]string
	VersionStyle        string
	EmulatedArchTargets []string
	self                Package
}

func (b *BasePackage) Base() *BasePackage { return b }

// Binds the concrete package so default methods dispatch to its overrides
func (b *BasePackage) bind(p Package) { b.self = p }

// Concrete package the defaults dispatch through, the base itself before registration
func (b *BasePackage) dispatch() Package {
	if b.self != nil {
		return b.self
	}
	return b
}

// Reports whether the package contributes PATH dirs
func (b *BasePackage) OnPath() bool { return !b.OffPath }

// Version labelling style of the lockfile version field, semver unless declared
func (b *BasePackage) Style() string {
	if b.VersionStyle == "" {
		return "semver"
	}
	return b.VersionStyle
}

func (b *BasePackage) MissingReason(target string) string { return b.Gaps[target] }

func (b *BasePackage) LatestVersions(target, locked string) ([]string, error) { return nil, nil }

func (b *BasePackage) FetchURL(version, target string) (string, error) {
	if b.URL == "" {
		return "", NewInstallError(b.Name, "package has no download url", "")
	}
	return formatTemplate(b.URL, map[string]string{"version": version, "target": target})
}

func (b *BasePackage) FetchURLs(version, target string) ([]string, error) {
	url, err := b.dispatch().FetchURL(version, target)
	if err != nil {
		return nil, err
	}
	return []string{url}, nil
}

func (b *BasePackage) StoreEntry(version, target string) string {
	return fmt.Sprintf("%s-%s-%s", b.Name, version, target)
}

func (b *BasePackage) KnownSHA256(version, url string) (string, error) { return "", nil }

func (b *BasePackage) Unpack(archive, staged, target string) error { return Extract(archive, staged) }

func (b *BasePackage) Stage(store *Store, staged, version, target string) error { return nil }

func (b *BasePackage) Binary(entry, target string) string { return "" }

func (b *BasePackage) Verify(entry, target string) string {
	bin := b.dispatch().Binary(entry, target)
	if bin == "" {
		return ""
	}
	return b.BinaryReason(bin, entry, target)
}

func (b *BasePackage) RepairStagedVerification(entry, target, reason string) (string, string) {
	return reason, ""
}

// Empty when the binary is present and arch plausible on target, else the diagnosis
func (b *BasePackage) BinaryReason(bin, entry, target string) string {
	if st, err := os.Stat(bin); err != nil || !st.Mode().IsRegular() {
		return missingReason(bin, entry)
	}
	if match, known := MachineMatchesBinary(bin, target); known && !match && !slices.Contains(b.EmulatedArchTargets, target) {
		return fmt.Sprintf("%s is not a %s binary", filepath.Base(bin), target)
	}
	return ""
}

func (b *BasePackage) Env(entry, target string) EnvDiff {
	diff := EnvDiff{}
	if b.OnPath() {
		if bin := b.dispatch().Binary(entry, target); bin != "" {
			diff["PATH"] = []string{filepath.Dir(bin)}
		}
	}
	return diff
}

// Fills named fields of a Python format template, doubled braces standing for literal ones
func formatTemplate(template string, fields map[string]string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(template); i++ {
		c := template[i]
		switch {
		case c == '{' && i+1 < len(template) && template[i+1] == '{':
			out.WriteByte('{')
			i++
		case c == '}' && i+1 < len(template) && template[i+1] == '}':
			out.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("single '{' encountered in format string")
			}
			name := template[i+1 : i+end]
			value, ok := fields[name]
			if !ok {
				return "", fmt.Errorf("KeyError: %s", pyrepr.StrRepr(name))
			}
			out.WriteString(value)
			i += end
		case c == '}':
			return "", fmt.Errorf("single '}' encountered in format string")
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), nil
}

// DebMain names the main file a deb package must stage, relative to its prefix
type DebMain interface {
	MainRel(target string) string
}

// DebPackage stages a deb by ar and tar extraction, never dpkg and never package scripts
type DebPackage struct {
	BasePackage
	DebName   string
	PrefixRel string
}

// Prefix the staged tree keeps the payload under, the Termux usr prefix unless overridden
func (d *DebPackage) Prefix() string {
	if d.PrefixRel == "" {
		return "data/data/com.termux/files/usr"
	}
	return d.PrefixRel
}

// Control field name the deb must declare, the package name unless overridden
func (d *DebPackage) DebPackageName() string {
	if d.DebName == "" {
		return d.Name
	}
	return d.DebName
}

func (d *DebPackage) Unpack(archive, staged, target string) error {
	return UnpackDeb(d.Name, archive, staged)
}

func (d *DebPackage) Verify(entry, target string) string {
	main := d.dispatch().(DebMain).MainRel(target)
	expected := filepath.Join(entry, filepath.FromSlash(d.Prefix()), filepath.FromSlash(main))
	st, err := os.Stat(expected)
	isFile := err == nil && st.Mode().IsRegular()
	lst, lerr := os.Lstat(expected)
	isLink := lerr == nil && lst.Mode()&fs.ModeSymlink != 0
	if !isFile && !isLink {
		rel, _ := filepath.Rel(entry, expected)
		return fmt.Sprintf("%s missing under %s", rel, Native(entry))
	}
	return ""
}

// The data tar of a deb, zstd payloads decompressed and lzma refused
func DebDataTar(name, archive string) ([]byte, error) {
	raw, err := os.ReadFile(archive)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(archive)
	if len(raw) < 8 || string(raw[:8]) != "!<arch>\n" {
		return nil, NewInstallError(name, fmt.Sprintf("not an ar archive: %s", base), "")
	}
	offset := 8
	for offset+60 <= len(raw) {
		hdr := raw[offset : offset+60]
		member := strings.TrimRight(string(bytes.ToValidUTF8(hdr[0:16], []byte("�"))), " \t\n\r\x0b\x0c")
		size, err := strconv.Atoi(strings.TrimSpace(string(bytes.ToValidUTF8(hdr[48:58], []byte("�")))))
		if err != nil {
			return nil, NewInstallError(name, fmt.Sprintf("bad ar member size in %s", base), "")
		}
		start := offset + 60
		end := min(start+size, len(raw))
		if size < 0 {
			end = start
		}
		data := raw[start:end]
		if strings.HasPrefix(member, "data.tar") {
			if strings.HasSuffix(member, ".lzma") {
				return nil, NewInstallError(name, fmt.Sprintf("unsupported data compression %s in %s", member, base), "")
			}
			if strings.HasSuffix(member, ".zst") {
				dec, err := zstd.NewReader(nil)
				if err != nil {
					return nil, err
				}
				defer dec.Close()
				return dec.DecodeAll(data, nil)
			}
			return data, nil
		}
		offset = start + size + size%2
	}
	return nil, NewInstallError(name, fmt.Sprintf("no data.tar member in %s", base), "")
}

// Stages a deb by ar and tar extraction under the one tar containment policy
func UnpackDeb(name, archive, staged string) error {
	payload, err := DebDataTar(name, archive)
	if err != nil {
		return err
	}
	return untarDebPayload(name, payload, staged)
}

// Extracts the payload then normalizes modes so non owner consumers can read and exec the tree
func untarDebPayload(name string, payload []byte, staged string) error {
	if err := ExtractTar(bytes.NewReader(payload), staged); err != nil {
		var filterErr *TarFilterError
		if errors.As(err, &filterErr) {
			return NewInstallError(name, fmt.Sprintf("unsafe member %s: %s", tarRepr(filterErr.Member), filterErr.Error()), "")
		}
		return err
	}
	realStaged := resolvePath(staged)
	return walkNoSymlinkDirs(staged, func(path string) {
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&fs.ModeSymlink != 0 {
			return
		}
		if !strings.HasPrefix(resolvePath(path), realStaged+string(filepath.Separator)) {
			return
		}
		wanted := fs.FileMode(0o644)
		if st.Mode().Perm()&0o111 != 0 {
			wanted |= 0o111
		}
		_ = os.Chmod(path, wanted)
	})
}

// Visits every non directory entry top down without descending into directory symlinks
func walkNoSymlinkDirs(root string, visit func(path string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		isDir := e.IsDir()
		isLink := e.Type()&fs.ModeSymlink != 0
		if isLink {
			if st, err := os.Stat(path); err == nil && st.IsDir() {
				continue
			}
		}
		if isDir && !isLink {
			if err := walkNoSymlinkDirs(path, visit); err != nil {
				return err
			}
			continue
		}
		visit(path)
	}
	return nil
}

// StatePackage is a state of this install verified by a stamp and made true by Apply
type StatePackage interface {
	Package
	ExpectedStamp(extras []string) (string, error)
	Apply(extras []string, explicit bool) error
}

// Reports whether the executable's architecture matches target, known false for scripts and shims
func MachineMatchesBinary(bin, target string) (match bool, known bool) {
	parts := strings.Split(target, "-")
	if len(parts) < 2 {
		return false, true
	}
	arch := parts[1]
	f, err := os.Open(bin)
	if err != nil {
		return false, false
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case len(head) >= 2 && string(head[:2]) == "MZ":
		var offset uint32
		for i := min(len(head), 64) - 1; i >= 60; i-- {
			offset = offset<<8 | uint32(head[i])
		}
		if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
			return false, false
		}
		sig := make([]byte, 6)
		m, _ := io.ReadFull(f, sig)
		if m < 4 || string(sig[:4]) != "PE\x00\x00" {
			return false, false
		}
		want, ok := map[string]uint16{"x64": 0x8664, "arm64": 0xAA64}[arch]
		if m < 6 {
			return false, true
		}
		return ok && binary.LittleEndian.Uint16(sig[4:6]) == want, true
	case len(head) >= 4 && string(head[:4]) == "\x7fELF":
		want, ok := map[string]uint16{"x64": 0x3E, "arm64": 0xB7}[arch]
		var machine uint16
		for i := min(len(head), 20) - 1; i >= 18; i-- {
			machine = machine<<8 | uint16(head[i])
		}
		return ok && machine == want, true
	case len(head) >= 4 && (string(head[:4]) == "\xcf\xfa\xed\xfe" || string(head[:4]) == "\xfe\xed\xfa\xcf"):
		want, ok := map[string]uint32{"x64": 0x01000007, "arm64": 0x0100000C}[arch]
		return ok && len(head) >= 8 && binary.LittleEndian.Uint32(head[4:8]) == want, true
	case len(head) >= 4 && (string(head[:4]) == "\xca\xfe\xba\xbe" || string(head[:4]) == "\xbe\xba\xfe\xca"):
		return true, true
	}
	return false, false
}

// Top level names of a store entry for verification diagnoses
func entryListing(entry string, limit int) string {
	st, err := os.Stat(entry)
	if err != nil || !st.IsDir() {
		return "store entry does not exist"
	}
	items, _ := os.ReadDir(entry)
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name())
	}
	sort.Strings(names)
	shown := strings.Join(names[:min(limit, len(names))], ", ")
	if len(names) > limit {
		shown += fmt.Sprintf(", … (%d entries)", len(names))
	}
	return shown
}

// Why an expected binary is not where the pin's layout puts it
func missingReason(bin, entry string) string {
	rel, err := filepath.Rel(entry, bin)
	if err != nil {
		rel = bin
	}
	return fmt.Sprintf("%s missing under %s; %s", filepath.ToSlash(rel), Native(entry), entryListing(entry, 12))
}

// Why a version probe failed, its exit code plus the tail of its output
func ProbeReason(bin string, exitCode int, stdout, stderr []byte) string {
	out := append(append([]byte{}, stdout...), stderr...)
	tail := strings.TrimSpace(strings.ToValidUTF8(string(out), "�"))
	if r := []rune(tail); len(r) > 300 {
		tail = string(r[len(r)-300:])
	}
	reason := fmt.Sprintf("%s --version exited %d", Native(bin), exitCode)
	if tail != "" {
		reason += ": " + tail
	}
	return reason
}

// Runner is what ensure hands back, a composed environment and a command mirror
type Runner struct {
	Name string
	Env  map[string]string
}

// Command with the runner environment, a bare name searched on the runner PATH like Python subprocess on POSIX
func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if runtime.GOOS != "windows" && !strings.Contains(name, "/") {
		search, set := r.Env["PATH"]
		if !set {
			search = defaultExecPath()
		}
		switch found := whichOn(name, &search); {
		case found != "":
			cmd.Path, cmd.Err = found, nil
		case search == "":
			cmd.Path, cmd.Err = "./"+name, nil
		default:
			cmd.Err = &exec.Error{Name: name, Err: exec.ErrNotFound}
		}
	}
	cmd.Env = EnvList(r.Env)
	return cmd
}

// Runs a command with inherited stdio under the runner's environment and returns its exit code
func (r *Runner) Run(ctx context.Context, name string, args ...string) (int, error) {
	cmd := r.Command(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// Environment map as sorted KEY=VALUE entries for exec
func EnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range sortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}
