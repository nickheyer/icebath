package tools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/internal/tools/fuzzy"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Line ending of the first 4096 characters: CRLF when any is there, LF when a line ends, none for one line
func lineEnding(sample string) string {
	head := strs.Head(sample, 4096)
	switch {
	case strings.Contains(head, "\r\n"):
		return "\r\n"
	case strings.Contains(head, "\n"):
		return "\n"
	}
	return ""
}

// Every CRLF, lone CR and LF made the ending
func normalizeEndings(text, ending string) string {
	lf := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if ending == "\r\n" {
		return strings.ReplaceAll(lf, "\n", "\r\n")
	}
	return lf
}

// Text of a failed operating system call as strerror then the file, the fallback file when the error names none
func osErrorText(err error, fallback string) string {
	message, name := err.Error(), fallback
	var errno syscall.Errno
	if errors.As(err, &errno) {
		message = errno.Error()
		if !windows && message != "" {
			message = strings.ToUpper(message[:1]) + message[1:]
		}
	}
	var pathErr *os.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		if errno == 0 {
			message = pathErr.Err.Error()
		}
		if pathErr.Path != "" {
			name = pathErr.Path
		}
	case errors.As(err, &linkErr):
		if errno == 0 {
			message = linkErr.Err.Error()
		}
		if linkErr.Old != "" {
			name = linkErr.Old
		}
	}
	return message + ": " + name
}

// Replaces a file through a temp file in its folder and a rename, following a link to its target and keeping the target's mode; the
// error text when it fails
func nativeWrite(target string, data []byte) string {
	if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink != 0 {
		target = realpath(target)
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return osErrorText(err, target)
	}
	var mode *os.FileMode
	if st, err := os.Stat(target); err == nil {
		m := st.Mode().Perm() | st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		mode = &m
	} else if !os.IsNotExist(err) {
		return osErrorText(err, target)
	}
	token := make([]byte, 6)
	rand.Read(token)
	tmp := filepath.Join(parent, ".icebath-tmp."+hex.EncodeToString(token))
	perm := os.FileMode(0o666)
	if mode != nil {
		perm = 0o600
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return osErrorText(err, target)
	}
	if mode != nil {
		f.Chmod(*mode)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return osErrorText(err, target)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return osErrorText(err, target)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return osErrorText(err, target)
	}
	return ""
}

// Writes the content atomically: directly on this machine's files, else through a script that writes a temp file beside the target and
// moves it over it
func (o *ops) atomicWrite(path, content string) run {
	if o.native {
		if message := nativeWrite(o.full(path), []byte(content)); message != "" {
			return run{output: message, code: 1}
		}
		return run{}
	}
	parent := dirname(path)
	if parent == "" {
		parent = "."
	}
	script := "set -e; d=" + o.path(parent) + "; t=" + o.path(path) + "; " +
		`if [ -L "$t" ]; then rt="$(readlink -f "$t" 2>/dev/null || realpath "$t" 2>/dev/null || true)"; ` +
		`[ -n "$rt" ] && { t="$rt"; d="$(dirname "$t")"; }; fi; ` +
		`mkdir -p "$d"; ` +
		`tmp="$(mktemp -p "$d" ` + quote(".icebath-tmp.XXXXXX") + ` 2>/dev/null || mktemp "$d/.icebath-tmp.$$.XXXXXX" 2>/dev/null || ` +
		`{ tmp="$d/.icebath-tmp.$$"; : > "$tmp" && echo "$tmp"; })"; ` +
		`[ -n "$tmp" ] || { echo "atomic write: could not create temp file" >&2; exit 1; }; ` +
		`trap 'rm -f \"$tmp\"' EXIT; ` +
		`if [ -e "$t" ]; then m="$(stat -c%a "$t" 2>/dev/null || stat -f%Lp "$t" 2>/dev/null || true)"; [ -n "$m" ] && chmod "$m" "$tmp" 2>/dev/null || true; fi; ` +
		`cat > "$tmp"; ` +
		`if [ ! -e "$t" ]; then chmod "=rw" "$tmp" 2>/dev/null || true; fi; ` +
		`mv -f "$tmp" "$t"; trap - EXIT`
	return o.execStdin(script, "", 0, &content)
}

// Whether the file on disk starts with a byte order mark
func (o *ops) hasBOM(path string) bool {
	head, ok := o.sample(path, 3)
	return ok && strings.HasPrefix(string(head), utf8BOM)
}

// What a write needs of its target in one look: whether it starts with a byte order mark, the content it holds when the write's check
// wants it and the caller has none, and the line ending of that content or of its first 4 KB
func (o *ops) probeTarget(path string, before *string, wantBefore bool) (bool, *string, string) {
	body := ""
	switch {
	case wantBefore && before == nil:
		body = "cat"
	case before == nil || *before == "":
		body = "sample"
	}
	if o.native {
		if f, err := openRegular(o.full(path)); err == nil {
			if f == nil {
				ending := ""
				if before != nil {
					ending = lineEnding(*before)
				}
				return false, before, ending
			}
			defer f.Close()
			var data []byte
			if body == "cat" {
				data, err = io.ReadAll(f)
			} else {
				buffer := make([]byte, 4096)
				var n int
				n, err = io.ReadFull(f, buffer)
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					err = nil
				}
				data = buffer[:n]
			}
			if err == nil {
				text := decode(data)
				if body == "cat" && text != "" {
					before = &text
				}
				sample := ""
				if before != nil && *before != "" {
					sample = *before
				} else if body == "sample" {
					sample = text
				}
				return strings.HasPrefix(string(data), utf8BOM), before, lineEnding(sample)
			}
		}
	}
	mark := sentinel("__ICEBATH_WF_")
	arg := o.path(path)
	command := ":"
	switch body {
	case "cat":
		command = "cat " + arg + " 2>/dev/null"
	case "sample":
		command = "head -c 4096 " + arg + " 2>/dev/null"
	}
	reply := o.exec("if [ -f "+arg+" ]; then head -c 3 "+arg+" 2>/dev/null | base64 2>/dev/null; echo "+mark+"; "+command+"; else echo "+missingMark+"; fi", "", 0)
	if !strings.Contains(reply.output, mark) {
		if strings.TrimSpace(clean(reply.output)) == missingMark {
			ending := ""
			if before != nil && *before != "" {
				ending = lineEnding(*before)
			}
			return false, before, ending
		}
		return o.probeTargetSequential(path, before, wantBefore)
	}
	segments := strings.Split(reply.output, mark+"\n")
	if reply.code != 0 || len(segments) != 2 {
		return o.probeTargetSequential(path, before, wantBefore)
	}
	bom := false
	if head, ok := unbase64(segments[0]); ok {
		bom = strings.HasPrefix(string(head), utf8BOM)
	} else {
		bom = o.hasBOM(path)
	}
	if body == "cat" && segments[1] != "" {
		content := segments[1]
		before = &content
	}
	ending := ""
	switch {
	case before != nil && *before != "":
		ending = lineEnding(*before)
	case body == "sample" && segments[1] != "":
		ending = lineEnding(segments[1])
	}
	return bom, before, ending
}

// The look at a write's target one command per question
func (o *ops) probeTargetSequential(path string, before *string, wantBefore bool) (bool, *string, string) {
	if wantBefore && before == nil {
		if read := o.exec("cat "+o.path(path)+" 2>/dev/null", "", 0); read.code == 0 && read.output != "" {
			before = &read.output
		}
	}
	ending := ""
	if before != nil && *before != "" {
		ending = lineEnding(*before)
	} else if head := o.exec("head -c 4096 "+o.path(path)+" 2>/dev/null", "", 0); head.code == 0 && head.output != "" {
		ending = lineEnding(head.output)
	}
	return o.hasBOM(path), before, ending
}

// Hex sha256 of a file on disk, empty when it could not be taken
func (o *ops) diskSha(path string) string {
	if o.native {
		if data, ok := nativeRead(o.full(path)); ok {
			return digest(data)
		}
	}
	if result := o.exec("sha256sum "+o.path(path)+" 2>/dev/null", "", 0); result.code == 0 {
		if words := fields(result.output); len(words) > 0 {
			return words[0]
		}
	}
	return ""
}

// Whether the file on disk holds the bytes meant, nil when no hash could be taken
func (o *ops) verify(path string, data []byte) (*bool, string) {
	sha := o.diskSha(path)
	if sha == "" {
		return nil, ""
	}
	ok := sha == digest(data)
	if !ok {
		return &ok, "Post-write verification failed for " + path + ": on-disk content hash differs from the intended write. The write did not " +
			"persist correctly — re-read the file and retry."
	}
	return &ok, ""
}

// Writes the whole content atomically, keeping the target's CRLF line endings and byte order mark and refusing structured content that
// does not parse, then checks the bytes landed and runs the syntax check and the language server
func (o *ops) writeFile(path, content string, before *string) *writeResult {
	started := time.Now()
	path = o.expand(path)
	if denied := o.denied(path, "Write", false); denied != "" {
		return &writeResult{Error: denied}
	}
	extension := ext(path)
	if refused := refuseUnparsed(path, content); refused != "" {
		return &writeResult{Error: refused}
	}
	bom, before, ending := o.probeTarget(path, before, contentChecked(extension) || o.serverHandles(extension))
	if extension == ".json" {
		if refused := refuseConstant(path, content, before); refused != "" {
			return &writeResult{Error: refused}
		}
	}
	if ending == "\r\n" {
		content = normalizeEndings(content, "\r\n")
	}
	if bom && !strings.HasPrefix(content, utf8BOM) {
		content = utf8BOM + content
	}
	data := []byte(content)
	if written := o.atomicWrite(path, content); written.code != 0 {
		return &writeResult{Error: "Failed to write file: " + written.output}
	}
	verified, failed := o.verify(path, data)
	if failed != "" {
		return &writeResult{Error: failed}
	}
	lint := o.lintDelta(path, before, &content)
	diagnostics := ""
	if lint.ok || lint.skipped {
		diagnostics = o.diagnostics(path, before, &content, started)
	}
	return &writeResult{BytesWritten: len(data), DirsCreated: dirname(path) != "", Verified: verified, Lint: lint, LSPDiagnostics: diagnostics,
		sha256: digest(data)}
}

// Result of a replace that found nothing: a success with no change when the edit already landed, else the error with the closest lines
func noMatch(path, content, old, new string, count int, reason string) *patchResult {
	if fuzzy.Applied(content, old, new) {
		return &patchResult{Success: true, NoChange: true, Note: "File already contains the target text — the edit appears to be already applied " +
			"to " + path + ". No write performed; do not re-send this patch."}
	}
	if reason == "" {
		reason = fuzzy.NoMatchIn(path)
	}
	return &patchResult{Error: reason + fuzzy.NoMatchHint(reason, count, old, content)}
}

// Error when the file read back differs from what was meant once line endings are made the same
func (o *ops) persisted(path, content string) *patchResult {
	data, failed := o.exactBytes(path)
	if failed != nil {
		return &patchResult{Error: "Post-write verification failed: could not re-read " + path}
	}
	onDisk := normalizeEndings(strings.TrimPrefix(string(data), utf8BOM), "\n")
	meant := normalizeEndings(content, "\n")
	if onDisk == meant {
		return nil
	}
	return &patchResult{Error: fmt.Sprintf("Post-write verification failed for %s: on-disk content differs from intended write (wrote %d chars, "+
		"read back %d chars after normalizing line endings). The patch did not persist. Re-read the file and try again.", path,
		utf8.RuneCountInString(meant), utf8.RuneCountInString(onDisk))}
}

// Replaces old with new in a file through the fuzzy matcher, writing it with write_file's checks and answering with the diff
func (o *ops) patchReplace(path, old, new string, all bool) *patchResult {
	path = o.expand(path)
	if denied := o.denied(path, "Write", false); denied != "" {
		return &patchResult{Error: denied}
	}
	data, failed := o.exactBytes(path)
	if failed != nil {
		message := failed.cwdError
		if message == "" {
			message = "Failed to read file: " + path
		}
		return &patchResult{Error: message}
	}
	raw := string(data)
	content := strings.TrimPrefix(raw, utf8BOM)
	replaced, err := fuzzy.Replace(content, old, new, all)
	if err != nil || replaced.Count == 0 {
		reason := ""
		if err != nil {
			reason = err.Error()
		}
		return noMatch(path, content, old, new, replaced.Count, reason)
	}
	updated := replaced.Content
	if ending := lineEnding(content); ending != "" {
		updated = normalizeEndings(updated, ending)
	}
	written := o.writeFile(path, updated, &raw)
	if written.Error != "" {
		return &patchResult{Error: "Failed to write changes: " + written.Error}
	}
	if broken := o.persisted(path, updated); broken != nil {
		return broken
	}
	var notes []string
	for _, note := range []string{replaced.Note, fuzzy.BoundaryNote([]string{replaced.Strategy}, "old_string's", "")} {
		if note != "" {
			notes = append(notes, note)
		}
	}
	w := patchWrite{path: path, read: digest(data), readKnown: true}
	if written.sha256 != "" {
		w.written, w.writtenKnown = written.sha256, true
	}
	return &patchResult{Success: true, Diff: diff.Files(content, updated, path), FilesModified: []string{path}, Note: strings.Join(notes, "\n"),
		LSPDiagnostics: written.LSPDiagnostics, Lint: written.Lint, writes: []patchWrite{w}}
}

// Path as a pure POSIX path prints, empty and dot elements dropped and two leading slashes kept
func pure(p string) string {
	if p == "" {
		return "."
	}
	root := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root = "//"
	case strings.HasPrefix(p, "/"):
		root = "/"
	}
	var parts []string
	for _, part := range strings.Split(p[len(root):], "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if out := root + strings.Join(parts, "/"); out != "" {
		return out
	}
	return "."
}

// Deletes one file, never a folder: directly on this machine's files, else through Python where commands run
func (o *ops) remove(target string) string {
	target = o.expand(target)
	if denied := o.denied(target, "Delete", true); denied != "" {
		return denied
	}
	var output string
	var code int
	if o.native {
		shown := pure(target)
		full := o.full(shown)
		if st, err := os.Stat(full); err == nil && st.IsDir() {
			if lst, err := os.Lstat(full); err != nil || lst.Mode()&os.ModeSymlink == 0 {
				output, code = "is a directory: "+shown, 2
			}
		}
		if code == 0 {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				var errno syscall.Errno
				output, code = err.Error(), 1
				if errors.As(err, &errno) {
					output = errno.Error() + ": " + shown
				}
			}
		}
	} else {
		result := o.snippet("import os, pathlib, sys\n" +
			"p = pathlib.Path(" + pyString(target) + ")\n" +
			"try:\n" +
			"    if p.is_dir() and not p.is_symlink():\n" +
			"        print('is a directory: ' + str(p), file=sys.stderr); sys.exit(2)\n" +
			"    p.unlink()\n" +
			"except FileNotFoundError:\n" +
			"    pass\n" +
			"except OSError as exc:\n" +
			"    if exc.errno is None:\n" +
			"        print(str(exc), file=sys.stderr); sys.exit(1)\n" +
			"    reason = os.strerror(exc.errno)\n" +
			"    print(reason[:1].lower() + reason[1:] + ': ' + str(p), file=sys.stderr); sys.exit(1)\n" +
			"except Exception as exc:\n" +
			"    print(str(exc), file=sys.stderr); sys.exit(1)\n")
		output, code = result.output, result.code
	}
	if code == 0 {
		return ""
	}
	message := strings.TrimSpace(output)
	if message == "" {
		message = "unknown error"
	}
	return "Failed to delete " + target + ": " + message
}

// Moves or renames an entry after checking both ends may be written
func (o *ops) move(from, to string) string {
	from, to = o.expand(from), o.expand(to)
	for _, p := range []string{from, to} {
		if denied := o.denied(p, "Move", true); denied != "" {
			return denied
		}
	}
	if result := o.exec("mv "+o.path(from)+" "+o.path(to), "", 0); result.code != 0 {
		return "Failed to move " + from + " -> " + to + ": " + result.output
	}
	return ""
}
