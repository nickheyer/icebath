package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"golang.org/x/text/unicode/norm"
)

// UTF-16 rescue skips files over this size and sniffs this many leading bytes
const (
	utf16MaxBytes   = 10 * 1024 * 1024
	utf16SampleSize = 512
)

// UTF-16 encoding a sample implies by its BOM or zero byte parity, empty when it is not UTF-16
func sniffUTF16(sample []byte) string {
	if len(sample) >= 2 && sample[0] == 0xfe && sample[1] == 0xff {
		return "utf-16-be"
	}
	if len(sample) >= 2 && sample[0] == 0xff && sample[1] == 0xfe {
		return "utf-16-le"
	}
	odd, even := 0, 0
	for i, c := range sample {
		if c != 0 {
			continue
		}
		if i%2 == 1 {
			odd++
		} else {
			even++
		}
	}
	switch {
	case even == 0 && odd >= 2:
		return "utf-16-le"
	case odd == 0 && even >= 2:
		return "utf-16-be"
	}
	return ""
}

// Python bytes.decode of UTF-16 with errors replace, one replacement per bad unit or truncated tail
func decodeUTF16Replace(data []byte, bigEndian bool) string {
	unit := func(i int) int {
		if bigEndian {
			return int(data[i])<<8 | int(data[i+1])
		}
		return int(data[i+1])<<8 | int(data[i])
	}
	var b strings.Builder
	i := 0
	for i+1 < len(data) {
		u := unit(i)
		switch {
		case u >= 0xd800 && u < 0xdc00:
			if i+3 < len(data) {
				if u2 := unit(i + 2); u2 >= 0xdc00 && u2 < 0xe000 {
					b.WriteRune(rune(0x10000 + (u-0xd800)<<10 + (u2 - 0xdc00)))
					i += 4
					continue
				}
				b.WriteRune(utf8.RuneError)
				i += 2
				continue
			}
			b.WriteRune(utf8.RuneError)
			return b.String()
		case u >= 0xdc00 && u < 0xe000:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(rune(u))
		}
		i += 2
	}
	if i < len(data) {
		b.WriteRune(utf8.RuneError)
	}
	return b.String()
}

// Page of a UTF-16 file transcoded to UTF-8 with its line total and encoding, false when not UTF-16
func utf16Page(data []byte, offset, limit int) (string, int, string, bool) {
	sample := data
	if len(sample) > utf16SampleSize {
		sample = sample[:utf16SampleSize]
	}
	enc := sniffUTF16(sample)
	if enc == "" {
		return "", 0, "", false
	}
	text := decodeUTF16Replace(data, enc == "utf-16-be")
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	lo := min(offset-1, len(lines))
	hi := min(offset-1+limit, len(lines))
	return strings.Join(lines[lo:hi], "\n"), len(lines), enc, true
}

// UTF-16 file read transcoded to UTF-8, nil when the caller should report a binary file
func (s *shellFileOps) tryReadUTF16(path string, offset, limit int, fileSize int64) *ReadResult {
	if hasBinaryExtension(path) || fileSize > utf16MaxBytes {
		return nil
	}
	data, failed := s.readExactBytes(path)
	if failed != nil || len(data) > utf16MaxBytes {
		return nil
	}
	content, totalLines, encoding, ok := utf16Page(data, offset, limit)
	if !ok {
		return nil
	}
	endLine := offset + limit - 1
	truncated := totalLines > endLine
	hint := "Transcoded from " + strings.ToUpper(encoding) + " to UTF-8 for display. " +
		"Text edits via patch/write_file would re-encode as UTF-8."
	if truncated {
		hint += " Use offset=" + strconv.Itoa(endLine+1) + " to continue reading " +
			"(showing " + strconv.Itoa(offset) + "-" + strconv.Itoa(endLine) + " of " + strconv.Itoa(totalLines) + " lines)"
	}
	return &ReadResult{
		Content: s.addLineNumbers(content, offset), TotalLines: totalLines,
		FileSize: fileSize, Truncated: truncated, Hint: hint,
		TruncatedLines: s.longLineFlag(content),
	}
}

// True pointer when any page line exceeds tool_output.max_line_length characters, nil otherwise
func (s *shellFileOps) longLineFlag(content string) *bool {
	maxLen := ToolOutputLimits(s.env).MaxLineLength
	for _, line := range strings.Split(content, "\n") {
		if pyLen(line) > maxLen {
			t := true
			return &t
		}
	}
	return nil
}

// Paginated read with binary detection and line numbers, natively on a local POSIX host
func (s *shellFileOps) readFile(path string, offset, limit int) *ReadResult {
	path = s.expandPath(path)
	offset, limit = normalizeReadPagination(s.env, offset, limit)
	if s.nativeReadEnabled() {
		return s.readFileNative(path, offset, limit)
	}
	if isImagePath(path) || hasBinaryExtension(path) {
		return s.readFileSequential(path, offset, limit)
	}
	lineClampBytes := 4*ToolOutputLimits(s.env).MaxLineLength + 1
	endLine := offset + limit - 1
	sentinel := newFileOpsSentinel(readSentinelPrefix)
	probe := s.exec(s.readProbeCmd(path, offset, endLine, lineClampBytes, sentinel), "", 0)
	output := probe.Stdout
	if !strings.Contains(output, sentinel) {
		switch pyStrip(stripTerminalFenceLeaks(output)) {
		case missingSentinel:
			return s.readFileMissing(path, offset, limit)
		case notRegularSentinel:
			return notRegularError(path)
		}
		return s.readFileSequential(path, offset, limit)
	}
	segments := splitSegments(output, sentinel)
	if probe.ExitCode != 0 || len(segments) != 6 {
		return s.readFileSequential(path, offset, limit)
	}
	sizeSeg, sampleSeg, pageSeg, wcSeg, tailSeg, statusSeg := segments[0], segments[1], segments[2], segments[3], segments[4], segments[5]
	status := pyFields(stripTerminalFenceLeaks(statusSeg))
	if len(status) < 2 {
		return s.readFileSequential(path, offset, limit)
	}
	sampleRC, ok1 := pyIntParse(status[0])
	readRC, ok2 := pyIntParse(status[1])
	if !ok1 || !ok2 {
		return s.readFileSequential(path, offset, limit)
	}
	fileSize := int64(0)
	if n, ok := pyIntParse(pyStrip(stripTerminalFenceLeaks(sizeSeg))); ok {
		fileSize = int64(n)
	}
	var sampleBytes []byte
	haveSample := false
	if sampleRC == 0 {
		sampleBytes, haveSample = decodeBase64Sample(sampleSeg)
	}
	var isBinary bool
	if haveSample {
		isBinary = isLikelyBinaryBytes(sampleBytes)
	} else {
		isBinary = isLikelyBinary(path, stripTerminalFenceLeaks(s.head(path, 1000).Stdout))
	}
	if isBinary {
		return s.readBinaryFile(path, offset, limit, fileSize, sampleBytes)
	}
	if readRC != 0 {
		return &ReadResult{Error: "Failed to read file: " + stripTerminalFenceLeaks(pageSeg)}
	}
	readOutput := stripTerminalFenceLeaks(pageSeg)
	totalLines := 0
	if n, ok := pyIntParse(pyStrip(stripTerminalFenceLeaks(wcSeg))); ok {
		totalLines = n
	}
	tailFlag := pyStrip(stripTerminalFenceLeaks(tailSeg))
	var endsWithNewline *bool
	if tailFlag == "0" || tailFlag == "1" {
		v := tailFlag == "1"
		endsWithNewline = &v
	}
	return s.assembleReadResult(readOutput, offset, endLine, totalLines, fileSize, endsWithNewline)
}

// Shell free read with the sed and cut page semantics, attaching the stat and digest snapshot
func (s *shellFileOps) readFileNative(path string, offset, limit int) *ReadResult {
	full := s.nativePath(path)
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || isNotDirErr(err) {
			if lst, lerr := os.Lstat(full); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
				return notRegularError(path)
			}
			return s.readFileMissing(path, offset, limit)
		}
		return s.readFileSequential(path, offset, limit)
	}
	if !st.Mode().IsRegular() {
		return notRegularError(path)
	}
	fileSize := st.Size()
	if isImagePath(path) {
		return imageRedirectResult(fileSize)
	}
	clamp := 4*ToolOutputLimits(s.env).MaxLineLength + 1
	endLine := offset + limit - 1
	f, err := os.Open(full)
	if err != nil {
		return s.readFileSequential(path, offset, limit)
	}
	defer f.Close()
	sample := make([]byte, 1000)
	n, err := io.ReadFull(f, sample)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return s.readFileSequential(path, offset, limit)
	}
	sample = sample[:n]
	if hasBinaryExtension(path) || isLikelyBinaryBytes(sample) {
		return s.readBinaryFile(path, offset, limit, fileSize, sample)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return s.readFileSequential(path, offset, limit)
	}
	var page bytes.Buffer
	totalLines, lineno := 0, 1
	var kept []byte
	havePartial := false
	var lastByte byte
	sawByte := false
	digest := sha256.New()
	chunk := make([]byte, 1<<20)
	for {
		cn, rerr := f.Read(chunk)
		if cn > 0 {
			data := chunk[:cn]
			digest.Write(data)
			lastByte, sawByte = data[cn-1], true
			if lineno > endLine {
				totalLines += bytes.Count(data, []byte{'\n'})
				havePartial = data[cn-1] != '\n'
			} else {
				pos := 0
				for pos < cn {
					nl := bytes.IndexByte(data[pos:], '\n')
					inPage := offset <= lineno && lineno <= endLine
					if nl < 0 {
						if inPage && len(kept) < clamp {
							kept = append(kept, data[pos:min(cn, pos+(clamp-len(kept)))]...)
						}
						havePartial = true
						break
					}
					nl += pos
					if inPage {
						if len(kept) < clamp {
							kept = append(kept, data[pos:min(nl, pos+(clamp-len(kept)))]...)
						}
						page.Write(kept)
						page.WriteByte('\n')
					}
					kept = kept[:0]
					havePartial = false
					totalLines++
					lineno++
					pos = nl + 1
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return s.readFileSequential(path, offset, limit)
		}
	}
	if havePartial && offset <= lineno && lineno <= endLine {
		page.Write(kept)
		page.WriteByte('\n')
	}
	readOutput := stripTerminalFenceLeaks(decodeUTF8Replace(page.Bytes()))
	var endsWithNewline *bool
	if fileSize != 0 {
		v := sawByte && lastByte == '\n'
		endsWithNewline = &v
	}
	result := s.assembleReadResult(readOutput, offset, endLine, totalLines, fileSize, endsWithNewline)
	var v fileVersion
	v.meta = metaOf(st)
	copy(v.digest[:], digest.Sum(nil))
	attachReadSnapshot(result, v)
	return result
}

// Reports whether an error is ENOTDIR, the NotADirectoryError of a stat
func isNotDirErr(err error) bool { return errors.Is(err, syscall.ENOTDIR) }

// Image read result redirecting the model to the vision tool
func imageRedirectResult(fileSize int64) *ReadResult {
	return &ReadResult{IsImage: true, IsBinary: true, FileSize: fileSize,
		Hint: "Image file detected. Automatically redirected to vision_analyze tool. " +
			"Use vision_analyze with this file path to inspect the image contents."}
}

// One shell command answering every read question in six sentinel closed segments
func (s *shellFileOps) readProbeCmd(path string, offset, endLine, lineClampBytes int, sentinel string) string {
	arg := s.escapeShellArg(path, true)
	mark := "echo " + sentinel
	return "if [ -f " + arg + " ]; then " +
		"wc -c < " + arg + " 2>/dev/null; " + mark + "; " +
		"head -c 1000 " + arg + " 2>/dev/null | base64 2>/dev/null; __hs=$?; " + mark + "; " +
		"sed -n '" + strconv.Itoa(offset) + "," + strconv.Itoa(endLine) + "p' " + arg + " 2>/dev/null" +
		" | cut -b1-" + strconv.Itoa(lineClampBytes) + " 2>/dev/null; __hr=$?; " + mark + "; " +
		"wc -l < " + arg + " 2>/dev/null; " + mark + "; " +
		"tail -c 1 " + arg + " 2>/dev/null | wc -l; " + mark + "; " +
		`echo "$__hs $__hr"; ` +
		"elif [ -e " + arg + " ] || [ -L " + arg + " ]; then echo " + notRegularSentinel + "; " +
		"else echo " + missingSentinel + "; fi"
}

// Not found recovery retrying a unicode equivalent spelling, else suggesting similar files
func (s *shellFileOps) readFileMissing(path string, offset, limit int) *ReadResult {
	if variant, ok := s.unicodeVariantMatch(path); ok {
		result := s.readFile(variant, offset, limit)
		note := "Note: '" + path + "' not found byte-for-byte; resolved to " +
			"the unicode-equivalent file '" + variant + "' (invisible " +
			"encoding difference: NFC/NFD or special space/quote " +
			"characters)."
		if result.Hint != "" {
			result.Hint = note + " " + result.Hint
		} else {
			result.Hint = note
		}
		return result
	}
	return s.suggestSimilarFiles(path)
}

// Binary branch rescuing UTF-16 text, else refusing with the detected type
func (s *shellFileOps) readBinaryFile(path string, offset, limit int, fileSize int64, sampleBytes []byte) *ReadResult {
	if r := s.tryReadUTF16(path, offset, limit, fileSize); r != nil {
		return r
	}
	return &ReadResult{IsBinary: true, FileSize: fileSize, Error: describeBinaryFile(sampleBytes, fileSize)}
}

// One probe per question read used for images, binary extensions and unparseable compound replies
func (s *shellFileOps) readFileSequential(path string, offset, limit int) *ReadResult {
	fileSize, status := s.probeRegularFile(path)
	switch status {
	case "missing":
		return s.readFileMissing(path, offset, limit)
	case "not_regular":
		return notRegularError(path)
	case "ok", "bad_size":
	default:
		return envUnavailableError(path, status)
	}
	if isImagePath(path) {
		return imageRedirectResult(fileSize)
	}
	isBinary, sampleBytes := s.detectBinary(path)
	if isBinary {
		return s.readBinaryFile(path, offset, limit, fileSize, sampleBytes)
	}
	lineClampBytes := 4*ToolOutputLimits(s.env).MaxLineLength + 1
	endLine := offset + limit - 1
	readResult := s.exec("sed -n '"+strconv.Itoa(offset)+","+strconv.Itoa(endLine)+"p' "+s.escapeShellArg(path, true)+
		" | cut -b1-"+strconv.Itoa(lineClampBytes), "", 0)
	if readResult.ExitCode != 0 {
		return &ReadResult{Error: "Failed to read file: " + readResult.Stdout}
	}
	readOutput := stripTerminalFenceLeaks(readResult.Stdout)
	wcResult := s.exec("wc -l < "+s.escapeShellArg(path, true), "", 0)
	totalLines := 0
	if n, ok := pyIntParse(pyStrip(stripTerminalFenceLeaks(wcResult.Stdout))); ok {
		totalLines = n
	}
	var endsWithNewline *bool
	if !(totalLines > endLine) && strings.HasSuffix(readOutput, "\n") {
		tail := s.exec("tail -c 1 "+s.escapeShellArg(path, true)+" | wc -l", "", 0)
		if tail.ExitCode == 0 {
			v := pyStrip(stripTerminalFenceLeaks(tail.Stdout)) != "0"
			endsWithNewline = &v
		}
	}
	return s.assembleReadResult(readOutput, offset, endLine, totalLines, fileSize, endsWithNewline)
}

// Final read result from a raw page with BOM strip, pagination hint, newline fix and dead end hints
func (s *shellFileOps) assembleReadResult(readOutput string, offset, endLine, totalLines int, fileSize int64, endsWithNewline *bool) *ReadResult {
	if fileSize > 0 && endsWithNewline != nil && !*endsWithNewline {
		totalLines++
	}
	if offset == 1 {
		readOutput, _ = stripBOM(readOutput)
	}
	truncated := totalLines > endLine
	hint := ""
	if truncated {
		hint = "Use offset=" + strconv.Itoa(endLine+1) + " to continue reading (showing " + strconv.Itoa(offset) + "-" +
			strconv.Itoa(endLine) + " of " + strconv.Itoa(totalLines) + " lines)"
	}
	if !truncated && strings.HasSuffix(readOutput, "\n") && endsWithNewline != nil && !*endsWithNewline {
		readOutput = readOutput[:len(readOutput)-1]
	}
	if fileSize == 0 {
		return &ReadResult{Content: "", TotalLines: 0, FileSize: 0, Hint: "File is empty (0 bytes)."}
	}
	if totalLines > 0 && offset > totalLines {
		return &ReadResult{Content: "", TotalLines: totalLines, FileSize: fileSize,
			Hint: "Note: offset " + strconv.Itoa(offset) + " is beyond the end of the file " +
				"(" + strconv.Itoa(totalLines) + " lines total). Retry with offset <= " +
				strconv.Itoa(totalLines) + "."}
	}
	return &ReadResult{
		Content: s.addLineNumbers(readOutput, offset), TotalLines: totalLines,
		FileSize: fileSize, Truncated: truncated, Hint: hint,
		TruncatedLines: s.longLineFlag(readOutput),
	}
}

// Confusable characters seen in real filenames, collapsed after NFC
var filenameConfusables = [][2]string{
	{"\u202f", " "},
	{"\u00a0", " "},
	{"\u2019", "'"},
	{"\u2018", "'"},
}

// Filename after NFC with confusable spaces and quotes collapsed
func canonicalFilename(name string) string {
	out := pyNFC(name)
	for _, c := range filenameConfusables {
		out = strings.ReplaceAll(out, c[0], c[1])
	}
	return out
}

// Python unicodedata NFC of a str, invalid bytes passed through untouched like lone surrogates
func pyNFC(s string) string {
	if utf8.ValidString(s) {
		return norm.NFC.String(s)
	}
	var b strings.Builder
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(norm.NFC.String(s[start:i]))
			b.WriteByte(s[i])
			i++
			start = i
			continue
		}
		i += size
	}
	b.WriteString(norm.NFC.String(s[start:]))
	return b.String()
}

// On disk spelling unicode equivalent to the path when exactly one entry matches
func (s *shellFileOps) unicodeVariantMatch(path string) (string, bool) {
	dirPath := pathDirnameOS(path)
	if dirPath == "" {
		dirPath = "."
	}
	filename := pathBasenameOS(path)
	if filename == "" {
		return "", false
	}
	target := canonicalFilename(filename)
	ls := s.exec("ls -1 "+s.escapeShellArg(dirPath, true)+" 2>/dev/null", "", 0)
	if ls.ExitCode != 0 || pyStrip(ls.Stdout) == "" {
		return "", false
	}
	var candidates []string
	for _, entry := range pySplitLines(stripTerminalFenceLeaks(ls.Stdout)) {
		if entry != "" && entry != filename && canonicalFilename(entry) == target {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) != 1 {
		return "", false
	}
	if dirPath != "." || strings.Contains(path, "/") {
		return pathJoinOS(dirPath, candidates[0]), true
	}
	return candidates[0], true
}

// File not found result listing up to five similar names from the same directory
func (s *shellFileOps) suggestSimilarFiles(path string) *ReadResult {
	dirPath := pathDirnameOS(path)
	if dirPath == "" {
		dirPath = "."
	}
	filename := pathBasenameOS(path)
	basenameNoExt := pyLower(pySplitextRoot(filename))
	ext := pyLower(pySplitextExt(filename))
	lowerName := pyLower(filename)
	ls := s.exec("ls -1 "+s.escapeShellArg(dirPath, true)+" 2>/dev/null | head -50", "", 0)
	type scoredPath struct {
		score int
		path  string
	}
	var scored []scoredPath
	if trimmed := pyStrip(ls.Stdout); ls.ExitCode == 0 && trimmed != "" {
		for _, f := range strings.Split(trimmed, "\n") {
			if f == "" {
				continue
			}
			lf := pyLower(f)
			score := 0
			switch {
			case lf == lowerName:
				score = 100
			case pyLower(pySplitextRoot(f)) == basenameNoExt:
				score = 90
			case strings.HasPrefix(lf, lowerName) || strings.HasPrefix(lowerName, lf):
				score = 70
			case strings.Contains(lf, lowerName):
				score = 60
			case strings.Contains(lowerName, lf) && pyLen(lf) > 2:
				score = 40
			case ext != "" && pyLower(pySplitextExt(f)) == ext:
				if common := runeSetIntersection(lowerName, lf); float64(common) >= float64(max(pyLen(lowerName), pyLen(lf)))*0.4 {
					score = 30
				}
			}
			if score == 0 && stringRatio(lowerName, lf) >= 0.8 {
				score = 50
			}
			if score > 0 {
				scored = append(scored, scoredPath{score, pathJoinOS(dirPath, f)})
			}
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	similar := []string{}
	for i := 0; i < len(scored) && i < 5; i++ {
		similar = append(similar, scored[i].path)
	}
	return &ReadResult{Error: "File not found: " + path, NotFound: true, SimilarFiles: similar}
}

// Size of the intersection of the character sets of two strings
func runeSetIntersection(a, b string) int {
	set := map[rune]bool{}
	for _, r := range a {
		set[r] = true
	}
	seen := map[rune]bool{}
	n := 0
	for _, r := range b {
		if set[r] && !seen[r] {
			seen[r] = true
			n++
		}
	}
	return n
}

// Whole file as a plain string with its leading BOM stripped and the sha of the exact bytes
func (s *shellFileOps) ReadFileRaw(path string) *ReadResult {
	path = s.expandPath(path)
	fileSize, status := s.probeRegularFile(path)
	switch status {
	case "missing":
		return s.suggestSimilarFiles(path)
	case "not_regular":
		return notRegularError(path)
	case "ok", "bad_size":
	default:
		return envUnavailableError(path, status)
	}
	if isImagePath(path) {
		return &ReadResult{IsImage: true, IsBinary: true, FileSize: fileSize}
	}
	isBinary, sampleBytes := s.detectBinary(path)
	if isBinary {
		return &ReadResult{IsBinary: true, FileSize: fileSize, Error: describeBinaryFile(sampleBytes, fileSize)}
	}
	data, failed := s.readExactBytes(path)
	if failed != nil {
		return &ReadResult{Error: "Failed to read file: " + failed.Stdout}
	}
	raw, _ := stripBOM(pystr.SurrogateEscapeDecode(data))
	sum := sha256.Sum256(data)
	return &ReadResult{Content: raw, FileSize: fileSize, ContentSha256: shaRef(hexString(sum[:]))}
}

// Lower case hex of a digest
func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&0xf]
	}
	return string(out)
}

// Binary safe base64 read through the backend, maxBytes zero or below meaning no limit
func (s *shellFileOps) readFileBytes(path string, maxBytes int) *ReadResult {
	path = s.expandPath(path)
	fileSize, status := s.probeRegularFile(path)
	switch status {
	case "missing":
		return &ReadResult{Error: "File not found: " + path, NotFound: true}
	case "not_regular":
		return notRegularError(path)
	case "ok":
	case "bad_size":
		return &ReadResult{Error: "Could not determine file size: " + path}
	default:
		return envUnavailableError(path, status)
	}
	if maxBytes > 0 && fileSize > int64(maxBytes) {
		return &ReadResult{FileSize: fileSize,
			Error: "File is too large (" + commaInt(int(fileSize)) + " bytes, limit is " + commaInt(maxBytes) + ")"}
	}
	encoded := s.exec("base64 < "+s.escapeShellArg(path, true), "", 0)
	if encoded.ExitCode != 0 {
		return &ReadResult{Error: "Failed to read binary file: " + encoded.Stdout}
	}
	compact := strings.Join(pyFields(stripTerminalFenceLeaks(encoded.Stdout)), "")
	if !pyB64Valid(compact) {
		return &ReadResult{Error: "Backend returned invalid binary data for: " + path}
	}
	return &ReadResult{Base64Content: compact, FileSize: fileSize, IsBinary: true}
}

// Alphabet prefilter of base64.b64decode with validate true
var regexpBase64Validate = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

// base64.b64decode with validate true accepting the text
func pyB64Valid(s string) bool {
	if s == "" {
		return true
	}
	if !regexpBase64Validate.MatchString(s) {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(s)
	return err == nil
}
