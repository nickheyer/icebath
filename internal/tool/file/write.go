package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/tool/env"
)

const (
	bodyNone = iota
	bodyCat
	bodySample
)

func (o *Operations) hostPath(path string) string {
	if runtime.GOOS != "windows" {
		return o.nativePath(path)
	}
	style := pathStyle{windows: true}
	if style.isAbs(path) {
		return path
	}
	return style.join(env.MsysToWindowsPath(o.liveCwd()), path)
}

func (o *Operations) atomicWrite(ctx context.Context, path, content string) (execResult, error) {
	if o.nativeEnabled() {
		if msg := atomicWriteNative(o.nativePath(path), []byte(content)); msg != "" {
			return execResult{stdout: msg, exitCode: 1}, nil
		}
		return execResult{}, nil
	}
	parent := o.paths().dir(path)
	if parent == "" {
		parent = "."
	}
	script := "set -e; " +
		"d=" + shellArg(parent) + "; t=" + shellArg(path) + "; " +
		`if [ -L "$t" ]; then ` +
		`rt="$(readlink -f "$t" 2>/dev/null || realpath "$t" 2>/dev/null || true)"; ` +
		`[ -n "$rt" ] && { t="$rt"; d="$(dirname "$t")"; }; ` +
		"fi; " +
		`mkdir -p "$d"; ` +
		`tmp="$(mktemp -p "$d" ` + shellArg(tempPrefix+"XXXXXX") + ` 2>/dev/null ` +
		`|| mktemp "$d/` + tempPrefix + `$$.XXXXXX" 2>/dev/null ` +
		`|| { tmp="$d/` + tempPrefix + `$$"; : > "$tmp" && echo "$tmp"; })"; ` +
		`[ -n "$tmp" ] || { echo "atomic write: could not create temp file" >&2; exit 1; }; ` +
		"trap 'rm -f \\\"$tmp\\\"' EXIT; " +
		`if [ -e "$t" ]; then ` +
		`m="$(stat -c%a "$t" 2>/dev/null || stat -f%Lp "$t" 2>/dev/null || true)"; ` +
		`[ -n "$m" ] && chmod "$m" "$tmp" 2>/dev/null || true; ` +
		"fi; " +
		`cat > "$tmp"; ` +
		`if [ ! -e "$t" ]; then chmod "=rw" "$tmp" 2>/dev/null || true; fi; ` +
		`mv -f "$tmp" "$t"; ` +
		"trap - EXIT"
	return o.exec(ctx, script, &content)
}

func (o *Operations) fileHasBOM(ctx context.Context, path string) (bool, error) {
	res, err := o.head(ctx, path, 3)
	if err != nil {
		return false, err
	}
	return res.exitCode == 0 && hasBOM(res.stdout), nil
}

func writeProbeCommand(path, sentinel string, body int) string {
	arg := shellArg(path)
	bodyCmd := ":"
	switch body {
	case bodyCat:
		bodyCmd = "cat " + arg + " 2>/dev/null"
	case bodySample:
		bodyCmd = "head -c 4096 " + arg + " 2>/dev/null"
	}
	return "if [ -f " + arg + " ]; then " +
		"head -c 3 " + arg + " 2>/dev/null | base64 2>/dev/null; echo " + sentinel + "; " +
		bodyCmd + "; " +
		"else echo " + missingSentinel + "; fi"
}

type writeTarget struct {
	hasBOM bool
	pre    *string
	ending string
}

func nonEmpty(s *string) bool {
	return s != nil && *s != ""
}

func (o *Operations) probeWriteTarget(ctx context.Context, path string, pre *string, wantPre bool) (writeTarget, error) {
	body := bodyNone
	switch {
	case wantPre && pre == nil:
		body = bodyCat
	case !nonEmpty(pre):
		body = bodySample
	}
	if o.nativeEnabled() {
		if native, ok := probeNative(o.nativePath(path), body == bodyCat); ok {
			text := ""
			if len(native.body) > 0 {
				text = decodeLossy(native.body)
			}
			if body == bodyCat && text != "" {
				pre = &text
			}
			sample := ""
			switch {
			case nonEmpty(pre):
				sample = *pre
			case body == bodySample:
				sample = text
			}
			return writeTarget{hasBOM: native.hasBOM, pre: pre, ending: detectLineEnding(sample)}, nil
		}
	}
	sentinel, err := newSentinel(writeSentinelPrefix)
	if err != nil {
		return writeTarget{}, err
	}
	probe, err := o.exec(ctx, writeProbeCommand(path, sentinel, body), nil)
	if err != nil {
		return writeTarget{}, err
	}
	output := probe.stdout
	if !strings.Contains(output, sentinel) {
		if trimSpace(stripFenceLeaks(output)) == missingSentinel {
			ending := ""
			if nonEmpty(pre) {
				ending = detectLineEnding(*pre)
			}
			return writeTarget{pre: pre, ending: ending}, nil
		}
		slog.Debug("write_file: pre-write probe reply has no sentinel; using sequential probes",
			"path", path, "exit", probe.exitCode, "chars", charLen(output))
		return o.probeWriteTargetSequential(ctx, path, pre, wantPre)
	}
	segments := splitSegments(output, sentinel)
	if probe.exitCode != 0 || len(segments) != 2 {
		slog.Debug("write_file: pre-write probe returned an unexpected shape; using sequential probes",
			"path", path, "exit", probe.exitCode, "segments", len(segments))
		return o.probeWriteTargetSequential(ctx, path, pre, wantPre)
	}
	headSeg, bodyText := segments[0], segments[1]
	target := writeTarget{}
	if headBytes, ok := decodeBase64Sample(headSeg); ok {
		target.hasBOM = strings.HasPrefix(string(headBytes), bom)
	} else {
		slog.Debug("write_file: no usable base64 head; probing the byte order mark separately", "path", path)
		target.hasBOM, err = o.fileHasBOM(ctx, path)
		if err != nil {
			return writeTarget{}, err
		}
	}
	if body == bodyCat && bodyText != "" {
		pre = &bodyText
	}
	target.pre = pre
	switch {
	case nonEmpty(pre):
		target.ending = detectLineEnding(*pre)
	case body == bodySample && bodyText != "":
		target.ending = detectLineEnding(bodyText)
	}
	return target, nil
}

func (o *Operations) probeWriteTargetSequential(ctx context.Context, path string, pre *string, wantPre bool) (writeTarget, error) {
	if wantPre && pre == nil {
		res, err := o.cat(ctx, path)
		if err != nil {
			return writeTarget{}, err
		}
		if res.exitCode == 0 && res.stdout != "" {
			pre = &res.stdout
		}
	}
	target := writeTarget{pre: pre}
	if nonEmpty(pre) {
		target.ending = detectLineEnding(*pre)
	} else {
		head, err := o.head(ctx, path, 4096)
		if err != nil {
			return writeTarget{}, err
		}
		if head.exitCode == 0 && head.stdout != "" {
			target.ending = detectLineEnding(head.stdout)
		}
	}
	hasMark, err := o.fileHasBOM(ctx, path)
	if err != nil {
		return writeTarget{}, err
	}
	target.hasBOM = hasMark
	return target, nil
}

func (o *Operations) verifyWrittenHash(ctx context.Context, path string, want string) (bool, string) {
	diskSHA := ""
	if o.nativeEnabled() {
		diskSHA, _ = sha256Native(o.nativePath(path))
	}
	if diskSHA == "" {
		res, err := o.exec(ctx, "sha256sum "+shellArg(path)+" 2>/dev/null", nil)
		if err != nil {
			return false, ""
		}
		if tokens := spaceFields(res.stdout); res.exitCode == 0 && len(tokens) > 0 {
			diskSHA = tokens[0]
		}
	}
	if diskSHA == "" {
		return false, ""
	}
	if diskSHA != want {
		return false, "Post-write verification failed for " + path + ": on-disk " +
			"content hash differs from the intended write. The " +
			"write did not persist correctly — re-read the file " +
			"and retry."
	}
	return true, ""
}

func (o *Operations) Write(ctx context.Context, path, content string, pre *string) (WriteResult, error) {
	started := time.Now()
	path, err := o.expandPath(ctx, path)
	if err != nil {
		return WriteResult{}, err
	}
	denied, err := o.guard.WriteDenied(ctx, path, "Write", false)
	if err != nil {
		return WriteResult{}, err
	}
	if denied != "" {
		return WriteResult{Error: denied}, nil
	}
	if modelSurrogates(ctx) {
		if lone, found := loneSurrogate(content); found {
			return WriteResult{Error: fmt.Sprintf("Refusing to write '%s': content contains a lone "+
				"surrogate character ('\\u%04x') that cannot be "+
				"encoded as UTF-8. The file was NOT created or modified.", path, lone)}, nil
		}
	}
	ext := strings.ToLower(o.paths().ext(path))
	if detail, refused := o.lint.SyntaxProblem(ext, content); refused {
		return WriteResult{Error: "Refusing to write '" + path + "': candidate content fails " +
			ext + " syntax validation (" + detail + "). The file was " +
			"NOT created or modified. Fix the content and retry."}, nil
	}
	wantPre, err := o.lint.WantsBaseline(ctx, o, ext)
	if err != nil {
		return WriteResult{}, err
	}
	target, err := o.probeWriteTarget(ctx, path, pre, wantPre)
	if err != nil {
		return WriteResult{}, err
	}
	pre = target.pre
	if ext == ".json" {
		if refusal := refuseIntroducedJSONConstant(path, content, pre); refusal != "" {
			return WriteResult{Error: refusal}, nil
		}
	}
	if target.ending == "\r\n" {
		content = normalizeLineEndings(content, "\r\n")
	}
	if target.hasBOM && !hasBOM(content) {
		content = bom + content
	}
	dirsCreated := o.paths().dir(path) != ""
	digest := sha256.Sum256([]byte(content))
	contentSHA := hex.EncodeToString(digest[:])
	written, err := o.atomicWrite(ctx, path, content)
	if err != nil {
		return WriteResult{}, err
	}
	if written.exitCode != 0 {
		return WriteResult{Error: "Failed to write file: " + written.stdout}, nil
	}
	verified, mismatch := o.verifyWrittenHash(ctx, path, contentSHA)
	if mismatch != "" {
		return WriteResult{Error: mismatch}, nil
	}
	lint, err := o.lint.Delta(ctx, o, path, pre, content)
	if err != nil {
		return WriteResult{}, err
	}
	diagnostics := ""
	if lint.Success || lint.Skipped {
		diagnostics, err = o.lint.Diagnostics(ctx, o, path, pre, content, started)
		if err != nil {
			return WriteResult{}, err
		}
	}
	return WriteResult{
		BytesWritten:   len(content),
		DirsCreated:    dirsCreated,
		Verified:       verified,
		ContentSHA256:  contentSHA,
		Lint:           &lint,
		LSPDiagnostics: diagnostics,
	}, nil
}

func (o *Operations) Delete(ctx context.Context, path string) (WriteResult, error) {
	path, err := o.expandPath(ctx, path)
	if err != nil {
		return WriteResult{}, err
	}
	denied, err := o.guard.WriteDenied(ctx, path, "Delete", true)
	if err != nil {
		return WriteResult{}, err
	}
	if denied != "" {
		return WriteResult{Error: denied}, nil
	}
	failure := ""
	if o.env.HostFilesystem() {
		full := o.hostPath(path)
		if fi, statErr := os.Lstat(full); statErr == nil && fi.IsDir() {
			failure = "is a directory: " + path
		} else if rmErr := os.Remove(full); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			failure = osErrorText(rmErr, full)
		}
	} else {
		res, err := o.exec(ctx, "p="+shellArg(path)+`; if [ -d "$p" ] && [ ! -L "$p" ]; then `+
			`echo "is a directory: $p" >&2; exit 2; fi; rm -f -- "$p"`, nil)
		if err != nil {
			return WriteResult{}, err
		}
		if res.exitCode != 0 {
			failure = trimSpace(res.stdout)
			if failure == "" {
				failure = "unknown error"
			}
		}
	}
	if failure != "" {
		return WriteResult{Error: "Failed to delete " + path + ": " + failure}, nil
	}
	return WriteResult{}, nil
}

func (o *Operations) Move(ctx context.Context, src, dst string) (WriteResult, error) {
	src, err := o.expandPath(ctx, src)
	if err != nil {
		return WriteResult{}, err
	}
	dst, err = o.expandPath(ctx, dst)
	if err != nil {
		return WriteResult{}, err
	}
	for _, p := range [...]string{src, dst} {
		denied, err := o.guard.WriteDenied(ctx, p, "Move", true)
		if err != nil {
			return WriteResult{}, err
		}
		if denied != "" {
			return WriteResult{Error: denied}, nil
		}
	}
	res, err := o.exec(ctx, "mv "+shellArg(src)+" "+shellArg(dst), nil)
	if err != nil {
		return WriteResult{}, err
	}
	if res.exitCode != 0 {
		return WriteResult{Error: "Failed to move " + src + " -> " + dst + ": " + res.stdout}, nil
	}
	return WriteResult{}, nil
}
