package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Deletes one file, refusing directories, natively on a local POSIX host else through a backend snippet
func (s *shellFileOps) DeleteFile(path string) *WriteResult {
	path = s.expandPath(path)
	if denied := writeDeniedError(s.env, path, "Delete", true); denied != "" {
		return &WriteResult{Error: denied}
	}
	var output string
	var exitCode int
	if s.nativeReadEnabled() {
		output, exitCode = nativeDeleteEntry(path, s.nativePath(purePosixPathStr(path)))
	} else {
		snippet := "import shutil, pathlib, sys\n" +
			"p = pathlib.Path(" + pyJSONDumpsASCII(path) + ")\n" +
			"recursive = False\n" +
			"try:\n" +
			"    if p.is_dir() and not p.is_symlink():\n" +
			"        if recursive:\n" +
			"            shutil.rmtree(p)\n" +
			"        else:\n" +
			"            print('is a directory: ' + str(p), file=sys.stderr); sys.exit(2)\n" +
			"    else:\n" +
			"        p.unlink()\n" +
			"except FileNotFoundError:\n" +
			"    pass\n" +
			"except Exception as exc:\n" +
			"    print(str(exc), file=sys.stderr); sys.exit(1)\n"
		result := s.execPythonSnippet(snippet, "")
		if result.ExitCode != 0 && strings.Contains(result.Stdout, "python3") {
			result = s.execPythonSnippet(snippet, "python")
		}
		output, exitCode = result.Stdout, result.ExitCode
	}
	if exitCode != 0 {
		msg := pyStrip(output)
		if msg == "" {
			msg = "unknown error"
		}
		return &WriteResult{Error: "Failed to delete " + path + ": " + msg}
	}
	return &WriteResult{}
}

// Host side run of the delete snippet with the same output text and exit status
func nativeDeleteEntry(path, full string) (string, int) {
	shown := purePosixPathStr(path)
	if st, err := os.Stat(full); err == nil && st.IsDir() {
		if lst, lerr := os.Lstat(full); lerr != nil || lst.Mode()&os.ModeSymlink == 0 {
			return "is a directory: " + shown + "\n", 2
		}
	}
	if err := syscall.Unlink(full); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0
		}
		return pyErrnoStr(err, shown) + "\n", 1
	}
	return "", 0
}

// Python OSError str of an errno with the strerror text and the quoted filename
func pyErrnoStr(err error, filename string) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), nativeStrerror(errno), pyReprStr(filename))
	}
	return err.Error()
}

// str of a pathlib PurePosixPath, empty and dot components dropped and two leading slashes kept
func purePosixPathStr(p string) string {
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
	out := root + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

// Moves or renames an entry through the backend mv after vetting both entries
func (s *shellFileOps) MoveFile(src, dst string) *WriteResult {
	src = s.expandPath(src)
	dst = s.expandPath(dst)
	for _, p := range []string{src, dst} {
		if denied := writeDeniedError(s.env, p, "Move", true); denied != "" {
			return &WriteResult{Error: denied}
		}
	}
	result := s.exec("mv "+s.escapeShellArg(src, true)+" "+s.escapeShellArg(dst, true), "", 0)
	if result.ExitCode != 0 {
		return &WriteResult{Error: "Failed to move " + src + " -> " + dst + ": " + result.Stdout}
	}
	return &WriteResult{}
}

// Refusal before any subprocess of content holding a surrogate outside the surrogateescape range
func rejectUnencodable(path, content string) *WriteResult {
	r, found := pystr.FirstUnencodableSurrogate(content)
	if !found {
		return nil
	}
	return &WriteResult{Error: "Refusing to write '" + path + "': content contains a lone " +
		"surrogate character (" + pyReprStr(pystr.FromRunes([]rune{r})) + ") that cannot be " +
		"encoded as UTF-8. The file was NOT created or modified."}
}

// Pre write refusal of JSON, YAML or TOML candidate content that does not parse
func failClosedSyntaxError(path, ext, content string) *WriteResult {
	if !failClosedInprocExts[ext] {
		return nil
	}
	linter := lintersInproc[ext]
	ok, errText := linter(content)
	if ok || errText == lintSkip {
		return nil
	}
	return &WriteResult{Error: "Refusing to write '" + path + "': candidate content fails " +
		ext + " syntax validation (" + errText + "). The file was " +
		"NOT created or modified. Fix the content and retry."}
}

// One shell command returning the base64 BOM head then the full text, the 4 KB sample, or nothing
func (s *shellFileOps) writeProbeCmd(path, sentinel, body string) string {
	arg := s.escapeShellArg(path, true)
	bodyCmd := ":"
	switch body {
	case "cat":
		bodyCmd = "cat " + arg + " 2>/dev/null"
	case "sample":
		bodyCmd = "head -c 4096 " + arg + " 2>/dev/null"
	}
	return "if [ -f " + arg + " ]; then " +
		"head -c 3 " + arg + " 2>/dev/null | base64 2>/dev/null; echo " + sentinel + "; " +
		bodyCmd + "; " +
		"else echo " + missingSentinel + "; fi"
}

// BOM flag, pre content and original line ending of a write target in one round trip
func (s *shellFileOps) probeWriteTarget(path string, preContent *string, wantPre bool) (bool, *string, string) {
	bodyMode := ""
	switch {
	case wantPre && preContent == nil:
		bodyMode = "cat"
	case preContent == nil || *preContent == "":
		bodyMode = "sample"
	}
	if s.nativeReadEnabled() {
		if np, ok := nativeProbe(s.nativePath(path), bodyMode == "cat"); ok {
			body := ""
			if len(np.body) > 0 {
				body = decodeUTF8Replace(np.body)
			}
			if bodyMode == "cat" && body != "" {
				preContent = &body
			}
			sample := ""
			if preContent != nil && *preContent != "" {
				sample = *preContent
			} else if bodyMode == "sample" {
				sample = body
			}
			return np.hasBOM, preContent, detectLineEnding(sample)
		}
	}
	sentinel := newFileOpsSentinel(writeSentinelPrefix)
	probe := s.exec(s.writeProbeCmd(path, sentinel, bodyMode), "", 0)
	output := probe.Stdout
	if !strings.Contains(output, sentinel) {
		if pyStrip(stripTerminalFenceLeaks(output)) == missingSentinel {
			ending := ""
			if preContent != nil && *preContent != "" {
				ending = detectLineEnding(*preContent)
			}
			return false, preContent, ending
		}
		return s.probeWriteTargetSequential(path, preContent, wantPre)
	}
	segments := splitSegments(output, sentinel)
	if probe.ExitCode != 0 || len(segments) != 2 {
		return s.probeWriteTargetSequential(path, preContent, wantPre)
	}
	headSeg, body := segments[0], segments[1]
	var bom bool
	if headBytes, ok := decodeBase64Sample(headSeg); ok {
		bom = strings.HasPrefix(string(headBytes), utf8BOM)
	} else {
		bom = s.fileHasBOM(path)
	}
	if bodyMode == "cat" && body != "" {
		preContent = &body
	}
	ending := ""
	switch {
	case preContent != nil && *preContent != "":
		ending = detectLineEnding(*preContent)
	case bodyMode == "sample" && body != "":
		ending = detectLineEnding(body)
	}
	return bom, preContent, ending
}

// One exec per question form of the write target probe
func (s *shellFileOps) probeWriteTargetSequential(path string, preContent *string, wantPre bool) (bool, *string, string) {
	if wantPre && preContent == nil {
		if r := s.cat(path); r.ExitCode == 0 && r.Stdout != "" {
			out := r.Stdout
			preContent = &out
		}
	}
	ending := ""
	if preContent != nil && *preContent != "" {
		ending = detectLineEnding(*preContent)
	} else if head := s.head(path, 4096); head.ExitCode == 0 && head.Stdout != "" {
		ending = detectLineEnding(head.Stdout)
	}
	return s.fileHasBOM(path), preContent, ending
}

// On disk sha256 compared to the intended bytes, nil verdict when no hash could be taken
func (s *shellFileOps) verifyWrittenHash(path string, contentBytes []byte) (verified *bool, failure *WriteResult) {
	defer func() {
		if recover() != nil {
			verified, failure = nil, nil
		}
	}()
	diskSha := ""
	if s.nativeReadEnabled() {
		diskSha, _ = nativeSha256Of(s.nativePath(path))
	}
	if diskSha == "" {
		hashResult := s.exec("sha256sum "+s.escapeShellArg(path, true)+" 2>/dev/null", "", 0)
		if out := pyStrip(hashResult.Stdout); hashResult.ExitCode == 0 && out != "" {
			diskSha = pyFields(out)[0]
		}
	}
	if diskSha == "" {
		return nil, nil
	}
	sum := sha256.Sum256(contentBytes)
	if diskSha != hexString(sum[:]) {
		f := false
		return &f, &WriteResult{Error: "Post-write verification failed for " + path + ": on-disk " +
			"content hash differs from the intended write. The " +
			"write did not persist correctly — re-read the file " +
			"and retry."}
	}
	t := true
	return &t, nil
}

// Atomic write keeping CRLF and BOM, refusing unparseable structured content, then hash, lint and LSP
func (s *shellFileOps) WriteFile(path, content string, preContent *string) *WriteResult {
	started := time.Now()
	path = s.expandPath(path)
	if denied := writeDeniedError(s.env, path, "Write", false); denied != "" {
		return &WriteResult{Error: denied}
	}
	if refused := rejectUnencodable(path, content); refused != nil {
		return refused
	}
	ext := pyLower(pySplitextExt(path))
	if refused := failClosedSyntaxError(path, ext, content); refused != nil {
		return refused
	}
	_, inproc := lintersInproc[ext]
	wantPre := inproc || s.lspHandlesExtension(ext)
	bom, preContent, originalEnding := s.probeWriteTarget(path, preContent, wantPre)
	if ext == ".json" {
		if refused := refuseIntroducedJSONConstant(path, content, preContent); refused != nil {
			return refused
		}
	}
	if originalEnding == "\r\n" {
		content = normalizeLineEndings(content, "\r\n")
	}
	if bom && !hasBOM(content) {
		content = utf8BOM + content
	}
	dirsCreated := pathDirnameOS(path) != ""
	contentBytes, err := pystr.SurrogateEscapeEncode(content)
	if err != nil {
		panic(err)
	}
	if wr := s.atomicWrite(path, content); wr.ExitCode != 0 {
		return &WriteResult{Error: "Failed to write file: " + wr.Stdout}
	}
	verified, verifyErr := s.verifyWrittenHash(path, contentBytes)
	if verifyErr != nil {
		return verifyErr
	}
	lint := s.checkLintDelta(path, preContent, &content)
	lsp := ""
	if lint.Success || lint.Skipped {
		lsp = s.maybeLSPDiagnostics(path, preContent, &content, started)
	}
	sum := sha256.Sum256(contentBytes)
	return &WriteResult{
		BytesWritten: len(contentBytes), DirsCreated: dirsCreated, Verified: verified,
		ContentSha256: shaRef(hexString(sum[:])), Lint: lint.toDict(), LSPDiagnostics: lsp,
	}
}

// Failed match result, a success shaped no op when the edit already landed, else the error with a hint
func (s *shellFileOps) noMatchResult(path, content, oldString, newString string, matchCount int, errText string) (res *PatchResult) {
	if isAlreadyApplied(content, oldString, newString) {
		return &PatchResult{Success: true, NoChange: true,
			Note: "File already contains the target text — the edit " +
				"appears to be already applied to " + path + ". No write " +
				"performed; do not re-send this patch."}
	}
	errMsg := errText
	if errMsg == "" {
		errMsg = "Could not find match for old_string in " + path
	}
	func() {
		defer func() { recover() }()
		errMsg += formatNoMatchHint(errMsg, matchCount, oldString, content)
	}()
	return &PatchResult{Error: errMsg}
}

// Error result when the re read file differs from the intended content after line ending normalization
func (s *shellFileOps) verifyPatchPersisted(path, newContent string) *PatchResult {
	data, failed := s.readExactBytes(path)
	if failed != nil {
		return &PatchResult{Error: "Post-write verification failed: could not re-read " + path}
	}
	bomless, _ := stripBOM(pystr.SurrogateEscapeDecode(data))
	onDisk := strings.ReplaceAll(strings.ReplaceAll(bomless, "\r\n", "\n"), "\r", "\n")
	intended := strings.ReplaceAll(strings.ReplaceAll(newContent, "\r\n", "\n"), "\r", "\n")
	if onDisk != intended {
		return &PatchResult{Error: fmt.Sprintf("Post-write verification failed for %s: on-disk content "+
			"differs from intended write "+
			"(wrote %d chars, read back "+
			"%d chars after normalizing line endings). "+
			"The patch did not persist. Re-read the file and try again.", path, pyLen(intended), pyLen(onDisk))}
	}
	return nil
}

// Fuzzy matched replacement written through write_file with the diff, lint, notes and sha record
func (s *shellFileOps) patchReplace(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult {
	path = s.expandPath(path)
	if denied := writeDeniedError(s.env, path, "Write", false); denied != "" {
		return &PatchResult{Error: denied}
	}
	data, failed := s.readExactBytes(path)
	if failed != nil {
		msg := failed.CwdError
		if msg == "" {
			msg = "Failed to read file: " + path
		}
		return &PatchResult{Error: msg}
	}
	rawContent := pystr.SurrogateEscapeDecode(data)
	content, _ := stripBOM(rawContent)
	var correctionNotes []string
	newContent, matchCount, strategy, errText := fuzzyFindAndReplace(content, oldString, newString, replaceAll, &correctionNotes, probe)
	if errText != "" || matchCount == 0 {
		return s.noMatchResult(path, content, oldString, newString, matchCount, errText)
	}
	if fileEnding := detectLineEnding(content); fileEnding != "" {
		newContent = normalizeLineEndings(newContent, fileEnding)
	}
	writeResult := s.WriteFile(path, newContent, &rawContent)
	if writeResult.Error != "" {
		return &PatchResult{Error: "Failed to write changes: " + writeResult.Error}
	}
	if verifyErr := s.verifyPatchPersisted(path, newContent); verifyErr != nil {
		return verifyErr
	}
	var notes []string
	for _, n := range append(correctionNotes, boundaryNote([]string{strategy}, "old_string's", "")) {
		if n != "" {
			notes = append(notes, n)
		}
	}
	readSum := sha256.Sum256(data)
	w := patchWrite{path: path, readSha: hexString(readSum[:]), readKnown: true}
	if writeResult.ContentSha256 != nil {
		w.writtenSha, w.writtenKnown = *writeResult.ContentSha256, true
	}
	res := &PatchResult{
		Success: true, Diff: unifiedDiffText(content, newContent, path), FilesModified: []string{path},
		Note: strings.Join(notes, "\n"), LSPDiagnostics: writeResult.LSPDiagnostics,
		Writes: []patchWrite{w},
	}
	res.Lint = writeResult.Lint
	return res
}

// File backend handed to the V4A applier carrying the metered edit call's probe
type probedShellFileOps struct {
	*shellFileOps
	probe *editProbe
}

func (p probedShellFileOps) editProbe() *editProbe { return p.probe }

// Parses then applies a V4A patch with the call's probe visible to the applier
func (s *shellFileOps) patchV4A(patchContent string, probe *editProbe) *PatchResult {
	operations, parseErr := parseV4APatch(patchContent)
	if parseErr != "" {
		return &PatchResult{Error: "Failed to parse patch: " + parseErr}
	}
	return applyV4AOperations(operations, probedShellFileOps{shellFileOps: s, probe: probe})
}
