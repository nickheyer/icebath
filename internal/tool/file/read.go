package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

const (
	utf16MaxBytes    = 10 * 1024 * 1024
	utf16SampleBytes = 512
	readChunkBytes   = 1 << 20
)

func notRegularError(path string) ReadResult {
	return ReadResult{Error: "Cannot read '" + path + "': not a regular file (directory, dangling symlink, " +
		"FIFO, socket, or device). Reading it could block indefinitely."}
}

func unavailableText(path string) string {
	return "Terminal environment unavailable: could not stat " + path + " " +
		"(the sandbox may still be starting or was removed). Retry shortly."
}

func envUnavailableError(path, status string) ReadResult {
	if status != statusUnavailable {
		return ReadResult{Error: status}
	}
	return ReadResult{Error: unavailableText(path)}
}

func imageRedirect(size int64) ReadResult {
	return ReadResult{IsImage: true, IsBinary: true, FileSize: size,
		Hint: "Image file detected. Automatically redirected to vision_analyze tool. " +
			"Use vision_analyze with this file path to inspect the image contents."}
}

const (
	statusOK          = "ok"
	statusMissing     = "missing"
	statusNotRegular  = "not_regular"
	statusBadSize     = "bad_size"
	statusUnavailable = "env_unavailable"
)

func (o *Operations) probeRegularFile(ctx context.Context, path string) (int64, string, error) {
	arg := shellArg(path)
	res, err := o.exec(ctx, "if [ -f "+arg+" ]; then wc -c < "+arg+" 2>/dev/null; "+
		"elif [ -e "+arg+" ] || [ -L "+arg+" ]; then echo "+notRegularSentinel+"; "+
		"else echo "+missingSentinel+"; fi", nil)
	if err != nil {
		return 0, "", err
	}
	output := trimSpace(stripFenceLeaks(res.stdout))
	switch {
	case output == missingSentinel:
		return 0, statusMissing, nil
	case output == notRegularSentinel:
		return 0, statusNotRegular, nil
	case res.exitCode != 0:
		if res.cwdError != "" {
			return 0, res.cwdError, nil
		}
		return 0, statusUnavailable, nil
	}
	size, ok := parseInt(output)
	if !ok {
		return 0, statusBadSize, nil
	}
	return int64(size), statusOK, nil
}

func (o *Operations) Read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	path, err := o.expandPath(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	limits := o.limits(ctx)
	offset, limit = ReadPage(offset, limit, limits)
	return o.read(ctx, path, offset, limit)
}

func (o *Operations) read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	if o.nativeEnabled() {
		return o.readNative(ctx, path, offset, limit)
	}
	if o.isImage(path) || HasBinaryExtension(path) {
		return o.readSequential(ctx, path, offset, limit)
	}
	maxLine := o.limits(ctx).MaxLineLength
	clamp := 4*maxLine + 1
	endLine := offset + limit - 1
	sentinel, err := newSentinel(readSentinelPrefix)
	if err != nil {
		return ReadResult{}, err
	}
	probe, err := o.exec(ctx, readProbeCommand(path, offset, endLine, clamp, sentinel), nil)
	if err != nil {
		return ReadResult{}, err
	}
	output := probe.stdout
	if !strings.Contains(output, sentinel) {
		switch trimSpace(stripFenceLeaks(output)) {
		case missingSentinel:
			return o.readMissing(ctx, path, offset, limit)
		case notRegularSentinel:
			return notRegularError(path), nil
		}
		slog.Debug("read_file: compound probe reply has no sentinel; using sequential probes",
			"path", path, "exit", probe.exitCode, "chars", charLen(output))
		return o.readSequential(ctx, path, offset, limit)
	}
	segments := splitSegments(output, sentinel)
	if probe.exitCode != 0 || len(segments) != 6 {
		slog.Debug("read_file: compound probe returned an unexpected shape; using sequential probes",
			"path", path, "exit", probe.exitCode, "segments", len(segments))
		return o.readSequential(ctx, path, offset, limit)
	}
	sizeSeg, sampleSeg, pageSeg, wcSeg, tailSeg, statusSeg := segments[0], segments[1], segments[2], segments[3], segments[4], segments[5]
	status := spaceFields(stripFenceLeaks(statusSeg))
	sampleRC, readRC := 0, 0
	parsed := len(status) >= 2
	if parsed {
		var okSample, okRead bool
		sampleRC, okSample = parseInt(status[0])
		readRC, okRead = parseInt(status[1])
		parsed = okSample && okRead
	}
	if !parsed {
		slog.Debug("read_file: compound probe has unparseable status; using sequential probes", "path", path)
		return o.readSequential(ctx, path, offset, limit)
	}
	size, _ := parseInt(stripFenceLeaks(sizeSeg))
	fileSize := int64(size)

	var sample []byte
	haveSample := false
	if sampleRC == 0 {
		sample, haveSample = decodeBase64Sample(sampleSeg)
	}
	var binary bool
	if haveSample {
		binary = likelyBinaryBytes(sample)
	} else {
		slog.Debug("read_file: no usable base64 sample; using the text heuristic", "path", path, "exit", sampleRC)
		head, err := o.head(ctx, path, 1000)
		if err != nil {
			return ReadResult{}, err
		}
		binary = likelyBinaryText(path, stripFenceLeaks(head.stdout))
	}
	if binary {
		return o.readBinary(ctx, path, offset, limit, fileSize, sample, haveSample)
	}
	if readRC != 0 {
		return ReadResult{Error: "Failed to read file: " + stripFenceLeaks(pageSeg)}, nil
	}
	totalLines, _ := parseInt(stripFenceLeaks(wcSeg))
	var endsWithNewline *bool
	switch trimSpace(stripFenceLeaks(tailSeg)) {
	case "1":
		yes := true
		endsWithNewline = &yes
	case "0":
		no := false
		endsWithNewline = &no
	}
	return assembleRead(stripFenceLeaks(pageSeg), offset, endLine, totalLines, fileSize, endsWithNewline, maxLine), nil
}

func readProbeCommand(path string, offset, endLine, clamp int, sentinel string) string {
	arg := shellArg(path)
	mark := "echo " + sentinel
	return "if [ -f " + arg + " ]; then " +
		"wc -c < " + arg + " 2>/dev/null; " + mark + "; " +
		"head -c 1000 " + arg + " 2>/dev/null | base64 2>/dev/null; __hs=$?; " + mark + "; " +
		fmt.Sprintf("sed -n '%d,%dp' ", offset, endLine) + arg + " 2>/dev/null" +
		fmt.Sprintf(" | cut -b1-%d 2>/dev/null; __hr=$?; ", clamp) + mark + "; " +
		"wc -l < " + arg + " 2>/dev/null; " + mark + "; " +
		"tail -c 1 " + arg + " 2>/dev/null | wc -l; " + mark + "; " +
		`echo "$__hs $__hr"; ` +
		"elif [ -e " + arg + " ] || [ -L " + arg + " ]; then echo " + notRegularSentinel + "; " +
		"else echo " + missingSentinel + "; fi"
}

func (o *Operations) readNative(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	full := o.nativePath(path)
	fi, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			if li, lerr := os.Lstat(full); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
				return notRegularError(path), nil
			}
			return o.readMissing(ctx, path, offset, limit)
		}
		return o.readSequential(ctx, path, offset, limit)
	}
	if !fi.Mode().IsRegular() {
		return notRegularError(path), nil
	}
	fileSize := fi.Size()
	if o.isImage(path) {
		return imageRedirect(fileSize), nil
	}
	maxLine := o.limits(ctx).MaxLineLength
	clamp := 4*maxLine + 1
	endLine := offset + limit - 1

	f, err := os.Open(full)
	if err != nil {
		return o.readSequential(ctx, path, offset, limit)
	}
	defer f.Close()
	sample := make([]byte, 1000)
	n, err := io.ReadFull(f, sample)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return o.readSequential(ctx, path, offset, limit)
	}
	sample = sample[:n]
	if HasBinaryExtension(path) || likelyBinaryBytes(sample) {
		return o.readBinary(ctx, path, offset, limit, fileSize, sample, true)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return o.readSequential(ctx, path, offset, limit)
	}

	var page bytes.Buffer
	totalLines := 0
	lineNo := 1
	kept := make([]byte, 0, 256)
	havePartial := false
	var lastByte byte
	sawBytes := false
	digest := sha256.New()
	chunk := make([]byte, readChunkBytes)
	for {
		got, rerr := f.Read(chunk)
		if got > 0 {
			data := chunk[:got]
			digest.Write(data)
			lastByte, sawBytes = data[got-1], true
			if lineNo > endLine {
				totalLines += bytes.Count(data, []byte{'\n'})
				havePartial = data[got-1] != '\n'
			} else {
				pos := 0
				for pos < got {
					if lineNo > endLine {
						totalLines += bytes.Count(data[pos:], []byte{'\n'})
						havePartial = data[got-1] != '\n'
						break
					}
					nl := bytes.IndexByte(data[pos:], '\n')
					inPage := offset <= lineNo && lineNo <= endLine
					if nl < 0 {
						if inPage && len(kept) < clamp {
							kept = append(kept, data[pos:min(got, pos+(clamp-len(kept)))]...)
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
					lineNo++
					pos = nl + 1
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return o.readSequential(ctx, path, offset, limit)
		}
	}
	if havePartial && offset <= lineNo && lineNo <= endLine {
		page.Write(kept)
		page.WriteByte('\n')
	}
	var endsWithNewline *bool
	if fileSize != 0 {
		ends := sawBytes && lastByte == '\n'
		endsWithNewline = &ends
	}
	result := assembleRead(stripFenceLeaks(decodeLossy(page.Bytes())), offset, endLine, totalLines, fileSize, endsWithNewline, maxLine)
	dev, ino, changeNs, err := fileStamp(full, fi)
	if err != nil {
		return ReadResult{}, err
	}
	version := &Version{Device: dev, Inode: ino, Size: fi.Size(), ModTimeNs: fi.ModTime().UnixNano(), ChangeTimeNs: changeNs}
	copy(version.Digest[:], digest.Sum(nil))
	result.Version = version
	return result, nil
}

func (o *Operations) readMissing(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	variant, ok, err := o.unicodeVariant(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	if !ok {
		return o.suggestSimilar(ctx, path)
	}
	result, err := o.Read(ctx, variant, offset, limit)
	if err != nil {
		return ReadResult{}, err
	}
	note := "Note: '" + path + "' not found byte-for-byte; resolved to " +
		"the unicode-equivalent file '" + variant + "' (invisible " +
		"encoding difference: NFC/NFD or special space/quote " +
		"characters)."
	if result.Hint != "" {
		result.Hint = note + " " + result.Hint
	} else {
		result.Hint = note
	}
	return result, nil
}

func (o *Operations) readBinary(ctx context.Context, path string, offset, limit int, fileSize int64, sample []byte, haveSample bool) (ReadResult, error) {
	rescued, ok, err := o.readUTF16(ctx, path, offset, limit, fileSize)
	if err != nil {
		return ReadResult{}, err
	}
	if ok {
		return rescued, nil
	}
	if !haveSample {
		sample = nil
	}
	return ReadResult{IsBinary: true, FileSize: fileSize, Error: DescribeBinary(sample, fileSize)}, nil
}

func utf16Encoding(sample []byte) string {
	if len(sample) >= 2 {
		switch {
		case sample[0] == 0xfe && sample[1] == 0xff:
			return "utf-16-be"
		case sample[0] == 0xff && sample[1] == 0xfe:
			return "utf-16-le"
		}
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

func decodeUTF16(data []byte, bigEndian bool) string {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		if bigEndian {
			units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
		} else {
			units = append(units, uint16(data[i+1])<<8|uint16(data[i]))
		}
	}
	text := string(utf16.Decode(units))
	if len(data)%2 == 1 {
		text += replacementChar
	}
	return text
}

func (o *Operations) readUTF16(ctx context.Context, path string, offset, limit int, fileSize int64) (ReadResult, bool, error) {
	if HasBinaryExtension(path) || fileSize > utf16MaxBytes {
		return ReadResult{}, false, nil
	}
	var data []byte
	if o.env.HostFilesystem() {
		raw, err := os.ReadFile(o.hostPath(path))
		if err != nil {
			return ReadResult{}, false, nil
		}
		data = raw
	} else {
		raw, failed, err := o.readExactBytes(ctx, path)
		if err != nil {
			return ReadResult{}, false, err
		}
		if failed != nil {
			return ReadResult{}, false, nil
		}
		data = raw
	}
	if len(data) > utf16MaxBytes {
		return ReadResult{}, false, nil
	}
	encoding := utf16Encoding(data[:min(len(data), utf16SampleBytes)])
	if encoding == "" {
		return ReadResult{}, false, nil
	}
	text := decodeUTF16(data, encoding == "utf-16-be")
	text = strings.TrimPrefix(text, bom)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	totalLines := len(lines)
	var selected []string
	if offset-1 < len(lines) {
		selected = lines[offset-1 : min(len(lines), offset-1+limit)]
	}
	content := strings.Join(selected, "\n")
	endLine := offset + limit - 1
	truncated := totalLines > endLine
	hint := "Transcoded from " + strings.ToUpper(encoding) + " to UTF-8 for display. " +
		"Text edits via patch/write_file would re-encode as UTF-8."
	if truncated {
		hint += fmt.Sprintf(" Use offset=%d to continue reading (showing %d-%d of %d lines)", endLine+1, offset, endLine, totalLines)
	}
	maxLine := o.limits(ctx).MaxLineLength
	return ReadResult{
		Content:        addLineNumbers(content, offset, maxLine),
		TotalLines:     totalLines,
		FileSize:       fileSize,
		Truncated:      truncated,
		Hint:           hint,
		TruncatedLines: anyLineLonger(content, maxLine),
	}, true, nil
}

func (o *Operations) readSequential(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	fileSize, status, err := o.probeRegularFile(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	switch status {
	case statusMissing:
		return o.readMissing(ctx, path, offset, limit)
	case statusNotRegular:
		return notRegularError(path), nil
	case statusOK, statusBadSize:
	default:
		return envUnavailableError(path, status), nil
	}
	if o.isImage(path) {
		return imageRedirect(fileSize), nil
	}
	binary, sample, haveSample, err := o.detectBinary(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	if binary {
		return o.readBinary(ctx, path, offset, limit, fileSize, sample, haveSample)
	}
	maxLine := o.limits(ctx).MaxLineLength
	clamp := 4*maxLine + 1
	endLine := offset + limit - 1
	page, err := o.exec(ctx, fmt.Sprintf("sed -n '%d,%dp' %s | cut -b1-%d", offset, endLine, shellArg(path), clamp), nil)
	if err != nil {
		return ReadResult{}, err
	}
	if page.exitCode != 0 {
		return ReadResult{Error: "Failed to read file: " + page.stdout}, nil
	}
	output := stripFenceLeaks(page.stdout)
	wc, err := o.exec(ctx, "wc -l < "+shellArg(path), nil)
	if err != nil {
		return ReadResult{}, err
	}
	totalLines, _ := parseInt(stripFenceLeaks(wc.stdout))
	var endsWithNewline *bool
	if !(totalLines > endLine) && strings.HasSuffix(output, "\n") {
		tail, err := o.exec(ctx, "tail -c 1 "+shellArg(path)+" | wc -l", nil)
		if err != nil {
			return ReadResult{}, err
		}
		if tail.exitCode == 0 {
			ends := trimSpace(stripFenceLeaks(tail.stdout)) != "0"
			endsWithNewline = &ends
		}
	}
	return assembleRead(output, offset, endLine, totalLines, fileSize, endsWithNewline, maxLine), nil
}

func assembleRead(output string, offset, endLine, totalLines int, fileSize int64, endsWithNewline *bool, maxLine int) ReadResult {
	noFinalNewline := endsWithNewline != nil && !*endsWithNewline
	if fileSize > 0 && noFinalNewline {
		totalLines++
	}
	if offset == 1 {
		output, _ = stripBOM(output)
	}
	truncated := totalLines > endLine
	hint := ""
	if truncated {
		hint = fmt.Sprintf("Use offset=%d to continue reading (showing %d-%d of %d lines)", endLine+1, offset, endLine, totalLines)
	}
	if !truncated && strings.HasSuffix(output, "\n") && noFinalNewline {
		output = output[:len(output)-1]
	}
	if fileSize == 0 {
		return ReadResult{Hint: "File is empty (0 bytes)."}
	}
	if offset > totalLines && totalLines > 0 {
		return ReadResult{TotalLines: totalLines, FileSize: fileSize,
			Hint: fmt.Sprintf("Note: offset %d is beyond the end of the file "+
				"(%d lines total). Retry with offset <= %d.", offset, totalLines, totalLines)}
	}
	return ReadResult{
		Content:        addLineNumbers(output, offset, maxLine),
		TotalLines:     totalLines,
		FileSize:       fileSize,
		Truncated:      truncated,
		Hint:           hint,
		TruncatedLines: anyLineLonger(output, maxLine),
	}
}

func canonicalName(name string) string {
	out := norm.NFC.String(name)
	out = strings.ReplaceAll(out, string(rune(0x202f)), " ")
	out = strings.ReplaceAll(out, string(rune(0x00a0)), " ")
	out = strings.ReplaceAll(out, string(rune(0x2019)), "'")
	out = strings.ReplaceAll(out, string(rune(0x2018)), "'")
	return out
}

func (o *Operations) unicodeVariant(ctx context.Context, path string) (string, bool, error) {
	style := o.paths()
	dirPath := style.dir(path)
	if dirPath == "" {
		dirPath = "."
	}
	filename := style.base(path)
	if filename == "" {
		return "", false, nil
	}
	target := canonicalName(filename)
	listing, err := o.exec(ctx, "ls -1 "+shellArg(dirPath)+" 2>/dev/null", nil)
	if err != nil {
		return "", false, err
	}
	if listing.exitCode != 0 || isBlank(listing.stdout) {
		return "", false, nil
	}
	var candidates []string
	for _, entry := range splitLines(stripFenceLeaks(listing.stdout), false) {
		if entry != "" && entry != filename && canonicalName(entry) == target {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) != 1 {
		return "", false, nil
	}
	if dirPath != "." || strings.Contains(path, "/") {
		return style.join(dirPath, candidates[0]), true, nil
	}
	return candidates[0], true, nil
}

func sharedChars(a, b string) int {
	inA := make(map[rune]struct{})
	for _, r := range decodeRunes(a) {
		inA[r] = struct{}{}
	}
	shared := make(map[rune]struct{})
	for _, r := range decodeRunes(b) {
		if _, ok := inA[r]; ok {
			shared[r] = struct{}{}
		}
	}
	return len(shared)
}

func (o *Operations) suggestSimilar(ctx context.Context, path string) (ReadResult, error) {
	style := o.paths()
	dirPath := style.dir(path)
	if dirPath == "" {
		dirPath = "."
	}
	filename := style.base(path)
	stem := strings.ToLower(style.stem(filename))
	ext := strings.ToLower(style.ext(filename))
	lowerName := strings.ToLower(filename)
	listing, err := o.exec(ctx, "ls -1 "+shellArg(dirPath)+" 2>/dev/null | head -50", nil)
	if err != nil {
		return ReadResult{}, err
	}
	type scoredPath struct {
		score int
		path  string
	}
	var scored []scoredPath
	if listing.exitCode == 0 && !isBlank(listing.stdout) {
		for _, f := range strings.Split(trimSpace(listing.stdout), "\n") {
			if f == "" {
				continue
			}
			lf := strings.ToLower(f)
			score := 0
			switch {
			case lf == lowerName:
				score = 100
			case strings.ToLower(style.stem(f)) == stem:
				score = 90
			case strings.HasPrefix(lf, lowerName) || strings.HasPrefix(lowerName, lf):
				score = 70
			case strings.Contains(lf, lowerName):
				score = 60
			case strings.Contains(lowerName, lf) && charLen(lf) > 2:
				score = 40
			case ext != "" && strings.ToLower(style.ext(f)) == ext:
				longest := max(charLen(lowerName), charLen(lf))
				if float64(sharedChars(lowerName, lf)) >= float64(longest)*0.4 {
					score = 30
				}
			}
			if score == 0 && similarity(lowerName, lf) >= 0.8 {
				score = 50
			}
			if score > 0 {
				scored = append(scored, scoredPath{score, style.join(dirPath, f)})
			}
		}
	}
	sort.SliceStable(scored, func(p, q int) bool { return scored[p].score > scored[q].score })
	result := ReadResult{Error: "File not found: " + path, NotFound: true}
	for i, s := range scored {
		if i == 5 {
			break
		}
		result.SimilarFiles = append(result.SimilarFiles, s.path)
	}
	return result, nil
}

func (o *Operations) ReadRaw(ctx context.Context, path string) (ReadResult, error) {
	path, err := o.expandPath(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	fileSize, status, err := o.probeRegularFile(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	switch status {
	case statusMissing:
		return o.suggestSimilar(ctx, path)
	case statusNotRegular:
		return notRegularError(path), nil
	case statusOK, statusBadSize:
	default:
		return envUnavailableError(path, status), nil
	}
	if o.isImage(path) {
		return ReadResult{IsImage: true, IsBinary: true, FileSize: fileSize}, nil
	}
	binary, sample, haveSample, err := o.detectBinary(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	if binary {
		if !haveSample {
			sample = nil
		}
		return ReadResult{IsBinary: true, FileSize: fileSize, Error: DescribeBinary(sample, fileSize)}, nil
	}
	data, failed, err := o.readExactBytes(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	if failed != nil {
		return ReadResult{Error: "Failed to read file: " + failed.stdout}, nil
	}
	content, _ := stripBOM(string(data))
	digest := sha256.Sum256(data)
	return ReadResult{Content: content, FileSize: fileSize, ContentSHA256: hex.EncodeToString(digest[:])}, nil
}

func (o *Operations) ReadBytes(ctx context.Context, path string, maxBytes int64) (ReadResult, error) {
	path, err := o.expandPath(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	fileSize, status, err := o.probeRegularFile(ctx, path)
	if err != nil {
		return ReadResult{}, err
	}
	switch status {
	case statusMissing:
		return ReadResult{Error: "File not found: " + path, NotFound: true}, nil
	case statusNotRegular:
		return notRegularError(path), nil
	case statusBadSize:
		return ReadResult{Error: "Could not determine file size: " + path}, nil
	case statusOK:
	default:
		return envUnavailableError(path, status), nil
	}
	if fileSize > maxBytes {
		return ReadResult{FileSize: fileSize,
			Error: "File is too large (" + commas(fileSize) + " bytes, limit is " + commas(maxBytes) + ")"}, nil
	}
	encoded, err := o.exec(ctx, "base64 < "+shellArg(path), nil)
	if err != nil {
		return ReadResult{}, err
	}
	if encoded.exitCode != 0 {
		return ReadResult{Error: "Failed to read binary file: " + encoded.stdout}, nil
	}
	compact := strings.Join(spaceFields(stripFenceLeaks(encoded.stdout)), "")
	if _, err := base64.StdEncoding.DecodeString(compact); err != nil {
		return ReadResult{Error: "Backend returned invalid binary data for: " + path}, nil
	}
	return ReadResult{Base64Content: &compact, FileSize: fileSize, IsBinary: true}, nil
}
