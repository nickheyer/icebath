package files

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/unicode/norm"
)

const (
	textMaxBytes    = 4 * 1024 * 1024
	dataURLMaxBytes = 16 * 1024 * 1024
	listMaxEntries  = 10_000
	readChunkBytes  = 1024 * 1024
)

var sensitiveDirs = map[string]bool{".aws": true, ".gnupg": true, ".ssh": true}

var imageMIME = map[string]string{
	".avif": "image/avif", ".bmp": "image/bmp", ".gif": "image/gif", ".ico": "image/x-icon", ".jpeg": "image/jpeg",
	".jpg": "image/jpeg", ".png": "image/png", ".svg": "image/svg+xml", ".webp": "image/webp",
}

// File system calls of the plugin file door, the real os calls in production
var pfs = struct {
	lstat    func(string) (fs.FileInfo, error)
	stat     func(string) (fs.FileInfo, error)
	realpath func(string) (string, error)
	rename   func(string, string) error
	readFile func(string) ([]byte, error)
	statfs   func(string) (int64, error)
}{os.Lstat, os.Stat, realpath, os.Rename, os.ReadFile, statfsType}

// Refusal is a plugin file failure carried as an error until it becomes the answer
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(code, message string) error { return &Refusal{Code: code, Message: message} }

func fail(code, message string) map[string]any {
	return map[string]any{"ok": false, "error": code, "message": message}
}

func lstatOrNil(target string) (fs.FileInfo, error) {
	st, err := pfs.lstat(target)
	if err != nil {
		if ErrorCode(err) == "ENOENT" {
			return nil, nil
		}
		return nil, err
	}
	return st, nil
}

// Realpath of target, empty for a missing path, a non folder parent or a link loop
func realpathOrEmpty(target string) (string, error) {
	real, err := pfs.realpath(target)
	if err != nil {
		switch ErrorCode(err) {
		case "ENOENT", "ENOTDIR", "ELOOP":
			return "", nil
		}
		return "", err
	}
	return real, nil
}

var ignorableRanges = [][2]rune{
	{0xad, 0xad}, {0x34f, 0x34f}, {0x115f, 0x1160}, {0x17b4, 0x17b5}, {0x180b, 0x180f}, {0x200b, 0x200f},
	{0x202a, 0x202e}, {0x2060, 0x206f}, {0x3164, 0x3164}, {0xfe00, 0xfe0f}, {0xfeff, 0xfeff}, {0xffa0, 0xffa0},
}

func ignorable(r rune) bool {
	for _, rg := range ignorableRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// Sensitive file rule of a name as given and as macOS and Windows may match it after folding
func sensitiveReason(target string) string {
	var b strings.Builder
	for _, r := range norm.NFKC.String(target) {
		if !ignorable(r) {
			b.WriteRune(r)
		}
	}
	folded := jsToLower(b.String())
	if reason := SensitiveFileBlockReason(target); reason != "" {
		return reason
	}
	if reason := SensitiveFileBlockReason(folded); reason != "" {
		return reason
	}
	if sensitiveDirs[filepath.Base(folded)] {
		return "That folder holds keys and cannot be used by a plugin."
	}
	return ""
}

func refuseSensitive(targets ...string) error {
	for _, t := range targets {
		if reason := sensitiveReason(t); reason != "" {
			return refuse("sensitive-file", reason)
		}
	}
	return nil
}

func sameIdentity(a, b fs.FileInfo, pa, pb string) bool {
	da, ia, oka := fileIdentity(a, pa)
	db, ib, okb := fileIdentity(b, pb)
	return oka && okb && da == db && ia == ib
}

// Only a plain singly linked file, a FIFO or device has no size to cap and a hard link may name anything
func refuseNonPlain(st fs.FileInfo, p string) error {
	if !st.Mode().IsRegular() {
		return refuse("not-a-file", "That path is not a regular file.")
	}
	if fileLinks(st, p) > 1 {
		return refuse("protected", "That file is hard-linked elsewhere, so the app cannot tell what it belongs to.")
	}
	return nil
}

// Resolved is a target already resolved inside its folder
type Resolved struct {
	Root   string
	Parts  []string
	Parent string
	Entry  string
	Target string
	Stat   fs.FileInfo
}

// The opened file must be the checked one and still sit at the checked path
func refuseMoved(r Resolved, opened fs.FileInfo) error {
	real, err := realpathOrEmpty(r.Target)
	if err != nil {
		return err
	}
	var now fs.FileInfo
	if real == r.Target {
		if now, err = lstatOrNil(real); err != nil {
			return err
		}
	}
	if !sameIdentity(opened, r.Stat, r.Target, r.Target) || now == nil || !sameIdentity(now, opened, r.Target, r.Target) {
		return refuse("outside-folder", "The file changed while it was being opened.")
	}
	return nil
}

// Rename, trash and list re-check that the entry is still the checked one in the checked folder
func refuseChanged(r Resolved) error {
	parent := ""
	if r.Parent != "" {
		p, err := realpathOrEmpty(r.Parent)
		if err != nil {
			return err
		}
		parent = p
	}
	real, err := realpathOrEmpty(r.Entry)
	if err != nil {
		return err
	}
	var now fs.FileInfo
	if real != "" {
		if now, err = lstatOrNil(real); err != nil {
			return err
		}
	}
	if parent != r.Parent || real != r.Target || now == nil || !sameIdentity(now, r.Stat, real, r.Target) {
		return refuse("outside-folder", "The folder changed while it was being used.")
	}
	return nil
}

// Moves from back to to only when nothing is at to, a file by link and unlink
func restoreWithoutOverwrite(from, to string) bool {
	moved, err := lstatOrNil(from)
	if err != nil || moved == nil {
		return false
	}
	if existing, err := lstatOrNil(to); err != nil || existing != nil {
		return false
	}
	if moved.IsDir() {
		return pfs.rename(from, to) == nil
	}
	if os.Link(from, to) != nil {
		return false
	}
	return os.Remove(from) == nil
}

func capMiB(maxBytes int) string {
	return jsNumber(math.Round(float64(maxBytes) / (1024 * 1024)))
}

// Reads at most maxBytes through a handle that must still be the checked file
func readChecked(r Resolved, maxBytes int) ([]byte, error) {
	f, err := openNoFollow(r.Target, false)
	if err != nil {
		return nil, nodeFsError(err, "open", r.Target)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := refuseMoved(r, opened); err != nil {
		return nil, err
	}
	if err := refuseNonPlain(opened, r.Target); err != nil {
		return nil, err
	}
	var data []byte
	for {
		n := readChunkBytes
		if left := maxBytes + 1 - len(data); left < n {
			n = left
		}
		chunk := make([]byte, n)
		got, err := f.ReadAt(chunk, int64(len(data)))
		if got == 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, nodeFsError(err, "read", r.Target)
			}
			return data, nil
		}
		data = append(data, chunk[:got]...)
		if len(data) > maxBytes {
			return nil, refuse("too-large", "File is larger than "+capMiB(maxBytes)+" MiB.")
		}
	}
}

// Writes text to the checked target, an existing file through a no follow handle and a new one created exclusively
func writeChecked(r Resolved, text string) error {
	if r.Stat != nil {
		if err := refuseNonPlain(r.Stat, r.Target); err != nil {
			return err
		}
		f, err := openNoFollow(r.Target, true)
		if err != nil {
			return nodeFsError(err, "open", r.Target)
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if err := refuseMoved(r, opened); err != nil {
			return err
		}
		if err := refuseNonPlain(opened, r.Target); err != nil {
			return err
		}
		if err := f.Truncate(0); err != nil {
			return nodeFsError(err, "ftruncate", r.Target)
		}
		if _, err := f.WriteAt([]byte(text), 0); err != nil {
			return nodeFsError(err, "write", r.Target)
		}
		return nil
	}
	f, err := os.OpenFile(r.Entry, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0o666)
	if err != nil {
		return nodeFsError(err, "open", r.Entry)
	}
	defer f.Close()
	created, err := f.Stat()
	if err != nil {
		return err
	}
	real, err := realpathOrEmpty(r.Entry)
	if err != nil {
		return err
	}
	var landed fs.FileInfo
	if real != "" {
		if landed, err = lstatOrNil(real); err != nil {
			return err
		}
	}
	if real != r.Entry || landed == nil || !sameIdentity(landed, created, real, r.Entry) {
		if landed != nil && sameIdentity(landed, created, real, r.Entry) {
			os.Remove(real)
		}
		return refuse("outside-folder", "The folder changed while the file was being created.")
	}
	if _, err := f.Write([]byte(text)); err != nil {
		return nodeFsError(err, "write", r.Entry)
	}
	return nil
}

func mimeFor(target string) string {
	if m, ok := imageMIME[strings.ToLower(filepath.Ext(target))]; ok {
		return m
	}
	return "application/octet-stream"
}

func toRel(parts []string) string { return strings.Join(parts, "/") }

type pluginOp func(pf *PluginFiles, pluginID string, req map[string]any) (map[string]any, error)

func requireFile(r Resolved, maxBytes int) error {
	if r.Stat == nil {
		return refuse("not-found", "No such file.")
	}
	if r.Stat.IsDir() {
		return refuse("is-directory", "That path is a folder.")
	}
	if err := refuseNonPlain(r.Stat, r.Target); err != nil {
		return err
	}
	if r.Stat.Size() > int64(maxBytes) {
		return refuse("too-large", "File is larger than "+capMiB(maxBytes)+" MiB.")
	}
	return refuseSensitive(r.Entry, r.Target)
}

func requireChild(r Resolved) error {
	if r.Parent == "" {
		return refuse("invalid-path", "This operation needs a path inside the folder, not the folder itself.")
	}
	return nil
}

var pluginOps = map[string]pluginOp{
	"list": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if r.Stat == nil || !r.Stat.IsDir() {
			if r.Stat != nil {
				return fail("not-a-directory", "That path is not a folder."), nil
			}
			return fail("not-found", "That path is not a folder."), nil
		}
		if err := refuseChanged(r); err != nil {
			return nil, err
		}
		dirents, err := os.ReadDir(r.Target)
		if err != nil {
			return nil, nodeFsError(err, "scandir", r.Target)
		}
		if err := refuseChanged(r); err != nil {
			return nil, err
		}
		entries := []any{}
		for i, d := range dirents {
			if i >= listMaxEntries {
				break
			}
			full := filepath.Join(r.Target, d.Name())
			isDir := d.IsDir()
			if isLinkMode(d.Type()) {
				real, err := realpathOrEmpty(full)
				if err != nil {
					return nil, err
				}
				if real == "" || !pf.tools.inside(real, r.Root) {
					continue
				}
				st, err := pfs.stat(real)
				if err != nil {
					return nil, nodeFsError(err, "stat", real)
				}
				isDir = st.IsDir()
			}
			entries = append(entries, map[string]any{"name": d.Name(), "path": toRel(append(append([]string{}, r.Parts...), d.Name())), "isDirectory": isDir})
		}
		return map[string]any{"ok": true, "entries": entries, "truncated": len(dirents) > listMaxEntries}, nil
	},
	"stat": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if r.Stat == nil {
			return map[string]any{"ok": true, "exists": false}, nil
		}
		return map[string]any{"ok": true, "exists": true, "isDirectory": r.Stat.IsDir(), "size": r.Stat.Size(), "mtimeMs": mtimeMs(r.Stat)}, nil
	},
	"readText": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if err := requireFile(r, textMaxBytes); err != nil {
			return nil, err
		}
		data, err := readChecked(r, textMaxBytes)
		if err != nil {
			return nil, err
		}
		head := data
		if len(head) > 8000 {
			head = head[:8000]
		}
		if strings.IndexByte(string(head), 0) >= 0 {
			return fail("binary", "That file is binary; read it as a data URL."), nil
		}
		return map[string]any{"ok": true, "text": utf8Lossy(data), "size": len(data), "mtimeMs": mtimeMs(r.Stat)}, nil
	},
	"readDataUrl": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if err := requireFile(r, dataURLMaxBytes); err != nil {
			return nil, err
		}
		data, err := readChecked(r, dataURLMaxBytes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "url": "data:" + mimeFor(r.Target) + ";base64," + base64.StdEncoding.EncodeToString(data)}, nil
	},
	"writeText": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		text, _ := req["text"].(string)
		if len(text) > textMaxBytes {
			return fail("too-large", "Text is larger than 4 MiB."), nil
		}
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if err := requireChild(r); err != nil {
			return nil, err
		}
		if r.Stat != nil && r.Stat.IsDir() {
			return fail("is-directory", "That path is a folder."), nil
		}
		if r.Stat != nil && jsTruthy(req["exclusive"]) {
			return fail("exists", "A file with that name already exists."), nil
		}
		if err := refuseSensitive(r.Entry, r.Target); err != nil {
			return nil, err
		}
		if err := writeChecked(r, text); err != nil {
			return nil, err
		}
		st, err := pfs.stat(r.Target)
		if err != nil {
			return nil, nodeFsError(err, "stat", r.Target)
		}
		return map[string]any{"ok": true, "path": toRel(r.Parts), "size": st.Size(), "mtimeMs": mtimeMs(st)}, nil
	},
	"rename": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		name := ""
		if s, ok := req["name"].(string); ok {
			name = jsTrim(s)
		}
		segs, err := pf.tools.segments(name)
		if err != nil {
			return nil, err
		}
		if len(segs) != 1 || strings.ContainsAny(name, `\/`) || name == "." {
			return fail("invalid-path", "A new name is one plain file or folder name."), nil
		}
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if err := requireChild(r); err != nil {
			return nil, err
		}
		if r.Stat == nil {
			return fail("not-found", "No such file or folder."), nil
		}
		destination := filepath.Join(r.Parent, name)
		if err := refuseSensitive(r.Entry, r.Target, destination); err != nil {
			return nil, err
		}
		if existing, err := lstatOrNil(destination); err != nil {
			return nil, err
		} else if existing != nil {
			return fail("exists", fmt.Sprintf("\"%s\" already exists.", name)), nil
		}
		if err := refuseChanged(r); err != nil {
			return nil, err
		}
		if err := pfs.rename(r.Entry, destination); err != nil {
			return nil, nodeFsError(err, "rename", r.Entry, destination)
		}
		landedIn, err := realpathOrEmpty(filepath.Dir(destination))
		if err != nil {
			return nil, err
		}
		landed, statErr := pfs.stat(destination)
		if landedIn != r.Parent || statErr != nil || !sameIdentity(landed, r.Stat, destination, r.Target) {
			if restoreWithoutOverwrite(destination, r.Entry) {
				return nil, refuse("outside-folder", "The folder changed during the rename, so it was undone.")
			}
			return nil, refuse("outside-folder", "The folder changed during the rename and it could not be undone safely.")
		}
		parts := append(append([]string{}, r.Parts[:len(r.Parts)-1]...), name)
		return map[string]any{"ok": true, "path": toRel(parts)}, nil
	},
	"trash": func(pf *PluginFiles, id string, req map[string]any) (map[string]any, error) {
		r, err := pf.resolve(id, req["folder"], req["path"])
		if err != nil {
			return nil, err
		}
		if err := requireChild(r); err != nil {
			return nil, err
		}
		if r.Stat == nil {
			return fail("not-found", "No such file or folder."), nil
		}
		if err := refuseSensitive(r.Entry, r.Target); err != nil {
			return nil, err
		}
		if err := refuseChanged(r); err != nil {
			return nil, err
		}
		if err := pf.deps.TrashItem(r.Entry); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	},
}

// Buffer.toString utf8, invalid sequences replaced
func utf8Lossy(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out, _ := xunicode.UTF8.NewDecoder().Bytes(b)
	return string(out)
}
