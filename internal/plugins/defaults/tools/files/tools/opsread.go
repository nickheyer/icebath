package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/internal/tools/document"
	"github.com/nickheyer/icebath/pkg/strs"
)

// UTF-16 files past this size are left as binary
const utf16Max = 10 * 1024 * 1024

// Lead of the error for a path that is not there
const notFound = "File not found: "

// Page of a file numbered by line from offset, at most limit lines: directly on this machine's files, else through one command that
// answers every question at once, else one command per question
func (o *ops) readFile(path string, offset, limit int) *readResult {
	path = o.expand(path)
	if o.native {
		return o.readNative(path, offset, limit)
	}
	if image(path) || binaryExtension(path) {
		return o.readSequential(path, offset, limit)
	}
	end := offset + limit - 1
	mark := sentinel("__ICEBATH_RF_")
	reply := o.exec(o.readProbe(path, offset, end, mark), "", 0)
	if !strings.Contains(reply.output, mark) {
		switch strings.TrimSpace(clean(reply.output)) {
		case missingMark:
			return o.readMissing(path, offset, limit)
		case notRegularMark:
			return notRegular(path)
		}
		return o.readSequential(path, offset, limit)
	}
	segments := strings.Split(reply.output, mark+"\n")
	if reply.code != 0 || len(segments) != 6 {
		return o.readSequential(path, offset, limit)
	}
	status := fields(clean(segments[5]))
	if len(status) < 2 {
		return o.readSequential(path, offset, limit)
	}
	sampleStatus, ok1 := integer(status[0])
	readStatus, ok2 := integer(status[1])
	if !ok1 || !ok2 {
		return o.readSequential(path, offset, limit)
	}
	size, _ := integer(clean(segments[0]))
	var sample []byte
	sampled := false
	if sampleStatus == 0 {
		sample, sampled = unbase64(segments[1])
	}
	isBinary := false
	if sampled {
		isBinary = binaryBytes(sample)
	} else {
		isBinary = binaryText(path, clean(o.exec("head -c 1000 "+o.path(path)+" 2>/dev/null", "", 0).output))
	}
	if isBinary {
		return o.readBinary(path, offset, limit, int64(size), sample)
	}
	if readStatus != 0 {
		return &readResult{Error: "Failed to read file: " + clean(segments[2])}
	}
	total, _ := integer(clean(segments[3]))
	var newline *bool
	if tail := strings.TrimSpace(clean(segments[4])); tail == "0" || tail == "1" {
		ends := tail == "1"
		newline = &ends
	}
	return o.assemble(clean(segments[2]), offset, end, total, int64(size), newline)
}

// One command answering every question of a read in six segments: size, sample, page, line count, whether it ends in a newline, and the
// status of the sample and page commands
func (o *ops) readProbe(path string, offset, end int, mark string) string {
	arg := o.path(path)
	echo := "echo " + mark
	clamp := strconv.Itoa(4*o.lineLimit() + 1)
	return "if [ -f " + arg + " ]; then " +
		"wc -c < " + arg + " 2>/dev/null; " + echo + "; " +
		"head -c 1000 " + arg + " 2>/dev/null | base64 2>/dev/null; __hs=$?; " + echo + "; " +
		"sed -n '" + strconv.Itoa(offset) + "," + strconv.Itoa(end) + "p' " + arg + " 2>/dev/null | cut -b1-" + clamp + " 2>/dev/null; __hr=$?; " + echo + "; " +
		"wc -l < " + arg + " 2>/dev/null; " + echo + "; " +
		"tail -c 1 " + arg + " 2>/dev/null | wc -l; " + echo + "; " +
		`echo "$__hs $__hr"; ` +
		"elif [ -e " + arg + " ] || [ -L " + arg + " ]; then echo " + notRegularMark + "; " +
		"else echo " + missingMark + "; fi"
}

// Read straight from this machine's file with the page semantics of the command form, and the version it saw attached
func (o *ops) readNative(path string, offset, limit int) *readResult {
	full := o.full(path)
	st, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			if lst, lerr := os.Lstat(full); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
				return notRegular(path)
			}
			return o.readMissing(path, offset, limit)
		}
		return o.readSequential(path, offset, limit)
	}
	if !st.Mode().IsRegular() {
		return notRegular(path)
	}
	size := st.Size()
	if image(path) {
		return imageResult(size)
	}
	clamp := 4*o.lineLimit() + 1
	end := offset + limit - 1
	f, err := os.Open(full)
	if err != nil {
		return o.readSequential(path, offset, limit)
	}
	defer f.Close()
	sample := make([]byte, 1000)
	n, err := io.ReadFull(f, sample)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return o.readSequential(path, offset, limit)
	}
	sample = sample[:n]
	if binaryExtension(path) || binaryBytes(sample) {
		return o.readBinary(path, offset, limit, size, sample)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return o.readSequential(path, offset, limit)
	}
	var page bytes.Buffer
	total, line := 0, 1
	var kept []byte
	partial, sawByte := false, false
	var last byte
	h := sha256.New()
	chunk := make([]byte, 1<<20)
	for {
		cn, rerr := f.Read(chunk)
		if cn > 0 {
			data := chunk[:cn]
			h.Write(data)
			last, sawByte = data[cn-1], true
			if line > end {
				total += bytes.Count(data, []byte{'\n'})
				partial = data[cn-1] != '\n'
			} else {
				for pos := 0; pos < cn; {
					nl := bytes.IndexByte(data[pos:], '\n')
					inPage := offset <= line && line <= end
					if nl < 0 {
						if inPage && len(kept) < clamp {
							kept = append(kept, data[pos:min(cn, pos+clamp-len(kept))]...)
						}
						partial = true
						break
					}
					nl += pos
					if inPage {
						if len(kept) < clamp {
							kept = append(kept, data[pos:min(nl, pos+clamp-len(kept))]...)
						}
						page.Write(kept)
						page.WriteByte('\n')
					}
					kept = kept[:0]
					partial = false
					total++
					line++
					pos = nl + 1
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return o.readSequential(path, offset, limit)
		}
	}
	if partial && offset <= line && line <= end {
		page.Write(kept)
		page.WriteByte('\n')
	}
	var newline *bool
	if size != 0 {
		ends := sawByte && last == '\n'
		newline = &ends
	}
	result := o.assemble(clean(decode(page.Bytes())), offset, end, total, size, newline)
	v := &fileVersion{meta: metaOf(st)}
	copy(v.digest[:], h.Sum(nil))
	result.version = v
	return result
}

// Read of an image, which goes to the vision tool
func imageResult(size int64) *readResult {
	return &readResult{IsImage: true, IsBinary: true, FileSize: size, Hint: "Image file detected. Automatically redirected to vision_analyze tool. " +
		"Use vision_analyze with this file path to inspect the image contents."}
}

// Read through one command per question, for images, binary extensions and replies that could not be parsed
func (o *ops) readSequential(path string, offset, limit int) *readResult {
	size, status := o.probe(path)
	switch status {
	case "missing":
		return o.readMissing(path, offset, limit)
	case "not_regular":
		return notRegular(path)
	case "ok", "bad_size":
	default:
		return unavailable(path, status)
	}
	if image(path) {
		return imageResult(size)
	}
	if isBinary, sample := o.detectBinary(path); isBinary {
		return o.readBinary(path, offset, limit, size, sample)
	}
	end := offset + limit - 1
	clamp := strconv.Itoa(4*o.lineLimit() + 1)
	page := o.exec("sed -n '"+strconv.Itoa(offset)+","+strconv.Itoa(end)+"p' "+o.path(path)+" | cut -b1-"+clamp, "", 0)
	if page.code != 0 {
		return &readResult{Error: "Failed to read file: " + page.output}
	}
	text := clean(page.output)
	total, _ := integer(clean(o.exec("wc -l < "+o.path(path), "", 0).output))
	var newline *bool
	if total <= end && strings.HasSuffix(text, "\n") {
		if tail := o.exec("tail -c 1 "+o.path(path)+" | wc -l", "", 0); tail.code == 0 {
			ends := strings.TrimSpace(clean(tail.output)) != "0"
			newline = &ends
		}
	}
	return o.assemble(text, offset, end, total, size, newline)
}

// Binary by the bytes of a sample with the sample, else by the text a command printed with none
func (o *ops) detectBinary(path string) (bool, []byte) {
	if sample, ok := o.sample(path, 1000); ok {
		return binaryExtension(path) || binaryBytes(sample), sample
	}
	return binaryText(path, clean(o.exec("head -c 1000 "+o.path(path)+" 2>/dev/null", "", 0).output)), nil
}

// Read of a page: the byte order mark stripped, the last line counted though it has no newline, and the hints of a page that goes on,
// of an empty file and of an offset past the end
func (o *ops) assemble(page string, offset, end, total int, size int64, newline *bool) *readResult {
	if size > 0 && newline != nil && !*newline {
		total++
	}
	if offset == 1 {
		page = strings.TrimPrefix(page, "\ufeff")
	}
	truncated := total > end
	hint := ""
	if truncated {
		hint = "Use offset=" + strconv.Itoa(end+1) + " to continue reading (showing " + strconv.Itoa(offset) + "-" + strconv.Itoa(end) + " of " +
			strconv.Itoa(total) + " lines)"
	}
	if !truncated && strings.HasSuffix(page, "\n") && newline != nil && !*newline {
		page = page[:len(page)-1]
	}
	switch {
	case size == 0:
		return &readResult{Hint: "File is empty (0 bytes)."}
	case total > 0 && offset > total:
		return &readResult{TotalLines: total, FileSize: size, Hint: "Note: offset " + strconv.Itoa(offset) + " is beyond the end of the file (" +
			strconv.Itoa(total) + " lines total). Retry with offset <= " + strconv.Itoa(total) + "."}
	}
	return &readResult{Content: o.number(page, offset), TotalLines: total, FileSize: size, Truncated: truncated, Hint: hint,
		TruncatedLines: o.longLines(page)}
}

// Read of a binary file: UTF-16 text transcoded, anything else refused naming its kind
func (o *ops) readBinary(path string, offset, limit int, size int64, sample []byte) *readResult {
	if r := o.readUTF16(path, offset, limit, size); r != nil {
		return r
	}
	return &readResult{IsBinary: true, FileSize: size, Error: describeBinary(sample, size), document: document.Is(sample, "")}
}

// Read of a UTF-16 text file as UTF-8, nil when the file is not one
func (o *ops) readUTF16(path string, offset, limit int, size int64) *readResult {
	if binaryExtension(path) || size > utf16Max {
		return nil
	}
	var page, encoding string
	var total int
	var ok bool
	if o.native {
		page, total, encoding, ok = o.utf16Native(path, offset, limit)
	} else {
		page, total, encoding, ok = o.utf16Shell(path, offset, limit)
	}
	if !ok {
		return nil
	}
	end := offset + limit - 1
	truncated := total > end
	hint := "Transcoded from " + strings.ToUpper(encoding) + " to UTF-8 for display. Text edits via patch/write_file would re-encode as UTF-8."
	if truncated {
		hint += " Use offset=" + strconv.Itoa(end+1) + " to continue reading (showing " + strconv.Itoa(offset) + "-" + strconv.Itoa(end) + " of " +
			strconv.Itoa(total) + " lines)"
	}
	return &readResult{Content: o.number(page, offset), TotalLines: total, FileSize: size, Truncated: truncated, Hint: hint,
		TruncatedLines: o.longLines(page)}
}

func (o *ops) utf16Native(path string, offset, limit int) (string, int, string, bool) {
	full := o.full(path)
	if st, err := os.Stat(full); err != nil || st.Size() > utf16Max {
		return "", 0, "", false
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", 0, "", false
	}
	return utf16Page(data, offset, limit)
}

// UTF-16 page worked out by Python where commands run
func (o *ops) utf16Shell(path string, offset, limit int) (string, int, string, bool) {
	code := "import sys, json, os\n" +
		"p = " + pyString(path) + "\n" +
		"offset = " + strconv.Itoa(offset) + "\n" +
		"limit = " + strconv.Itoa(limit) + "\n" +
		"MAX = " + strconv.Itoa(utf16Max) + "\n" +
		"try:\n" +
		"    if os.path.getsize(p) > MAX:\n" +
		"        print('ICEBATH_UTF16:NO'); sys.exit(0)\n" +
		"    with open(p, 'rb') as f:\n" +
		"        data = f.read()\n" +
		"    sample = data[:512]\n" +
		"    enc = None\n" +
		"    if sample[:2] == b'\\xfe\\xff':\n" +
		"        enc = 'utf-16-be'\n" +
		"    elif sample[:2] == b'\\xff\\xfe':\n" +
		"        enc = 'utf-16-le'\n" +
		"    else:\n" +
		"        odd = sum(1 for i in range(1, len(sample), 2) if sample[i] == 0)\n" +
		"        even = sum(1 for i in range(0, len(sample), 2) if sample[i] == 0)\n" +
		"        if even == 0 and odd >= 2:\n" +
		"            enc = 'utf-16-le'\n" +
		"        elif odd == 0 and even >= 2:\n" +
		"            enc = 'utf-16-be'\n" +
		"    if enc is None:\n" +
		"        print('ICEBATH_UTF16:NO'); sys.exit(0)\n" +
		"    text = data.decode(enc, 'replace')\n" +
		"    if text[:1] == '\\ufeff':\n" +
		"        text = text[1:]\n" +
		"    lines = text.replace('\\r\\n', '\\n').split('\\n')\n" +
		"    print('ICEBATH_UTF16:OK')\n" +
		"    print(json.dumps({'total_lines': len(lines), 'encoding': enc, 'content': '\\n'.join(lines[offset - 1: offset - 1 + limit])}, ensure_ascii=True))\n" +
		"except Exception:\n" +
		"    print('ICEBATH_UTF16:NO'); sys.exit(0)\n"
	result := o.snippet(code)
	out := clean(result.output)
	at := strings.Index(out, "ICEBATH_UTF16:OK")
	if result.code != 0 || at < 0 {
		return "", 0, "", false
	}
	payload, _, _ := strings.Cut(strings.TrimSpace(out[at+len("ICEBATH_UTF16:OK"):]), "\n")
	var page struct {
		Total    *int    `json:"total_lines"`
		Encoding *string `json:"encoding"`
		Content  *string `json:"content"`
	}
	if json.Unmarshal([]byte(payload), &page) != nil || page.Content == nil || page.Total == nil {
		return "", 0, "", false
	}
	encoding := "utf-16"
	if page.Encoding != nil {
		encoding = *page.Encoding
	}
	return *page.Content, *page.Total, encoding, true
}

// Read of a path that is not there: the one file of its folder spelled the same in Unicode read in its place, else the error naming
// files of similar names
func (o *ops) readMissing(path string, offset, limit int) *readResult {
	if variant, ok := o.unicodeVariant(path); ok {
		result := o.readFile(variant, offset, limit)
		note := "Note: '" + path + "' not found byte-for-byte; resolved to the unicode-equivalent file '" + variant + "' (invisible encoding " +
			"difference: NFC/NFD or special space/quote characters)."
		if result.Hint != "" {
			note += " " + result.Hint
		}
		result.Hint = note
		return result
	}
	return o.similar(path)
}

var confusable = strings.NewReplacer("\u202f", " ", "\u00a0", " ", "\u2019", "'", "\u2018", "'")

// File name in NFC with the spaces and quotes it is often confused with made plain, bytes that are not UTF-8 kept
func canonical(name string) string {
	if utf8.ValidString(name) {
		return confusable.Replace(norm.NFC.String(name))
	}
	var b strings.Builder
	start := 0
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(norm.NFC.String(name[start:i]))
			b.WriteByte(name[i])
			i++
			start = i
			continue
		}
		i += size
	}
	b.WriteString(norm.NFC.String(name[start:]))
	return confusable.Replace(b.String())
}

// Entries of a folder, one a line, the first limit of them when it is above zero
func (o *ops) list(dir string, limit int) ([]string, bool) {
	if o.native {
		entries, err := os.ReadDir(o.full(dir))
		if err != nil {
			return nil, false
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				names = append(names, entry.Name())
			}
		}
		slices.Sort(names)
		if limit > 0 {
			names = names[:min(limit, len(names))]
		}
		return names, true
	}
	command := "ls -1 " + o.path(dir) + " 2>/dev/null"
	if limit > 0 {
		command += " | head -" + strconv.Itoa(limit)
	}
	result := o.exec(command, "", 0)
	if result.code != 0 || strings.TrimSpace(result.output) == "" {
		return nil, false
	}
	var names []string
	for _, line := range diff.Split(clean(strings.TrimSpace(result.output))) {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, true
}

// The one entry of the path's folder whose name is the same as the path's in Unicode but differs in bytes
func (o *ops) unicodeVariant(path string) (string, bool) {
	dir := dirname(path)
	if dir == "" {
		dir = "."
	}
	name := basename(path)
	if name == "" {
		return "", false
	}
	want := canonical(name)
	entries, ok := o.list(dir, 0)
	if !ok {
		return "", false
	}
	var found []string
	for _, entry := range entries {
		if entry != name && canonical(entry) == want {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	if dir != "." || strings.Contains(path, "/") {
		return join(dir, found[0]), true
	}
	return found[0], true
}

// Error of a missing file listing up to five names of its folder that look like it
func (o *ops) similar(path string) *readResult {
	dir := dirname(path)
	if dir == "" {
		dir = "."
	}
	name := strings.ToLower(basename(path))
	bare, suffix := stem(name), ext(name)
	type scored struct {
		score int
		path  string
	}
	var all []scored
	entries, _ := o.list(dir, 50)
	for _, entry := range entries {
		lower := strings.ToLower(entry)
		score := 0
		switch {
		case lower == name:
			score = 100
		case stem(lower) == bare:
			score = 90
		case strings.HasPrefix(lower, name) || strings.HasPrefix(name, lower):
			score = 70
		case strings.Contains(lower, name):
			score = 60
		case strings.Contains(name, lower) && utf8.RuneCountInString(lower) > 2:
			score = 40
		case suffix != "" && ext(lower) == suffix:
			if float64(shared(name, lower)) >= float64(max(utf8.RuneCountInString(name), utf8.RuneCountInString(lower)))*0.4 {
				score = 30
			}
		}
		if score == 0 && diff.Ratio(name, lower) >= 0.8 {
			score = 50
		}
		if score > 0 {
			all = append(all, scored{score, join(dir, entry)})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	similar := []string{}
	for i := 0; i < len(all) && i < 5; i++ {
		similar = append(similar, all[i].path)
	}
	return &readResult{Error: notFound + path, NotFound: true, SimilarFiles: similar}
}

// Characters two texts both hold, each counted once
func shared(a, b string) int {
	in := map[rune]bool{}
	for _, r := range a {
		in[r] = true
	}
	n := 0
	for _, r := range b {
		if in[r] {
			n++
			delete(in, r)
		}
	}
	return n
}

// Whole file as text without its byte order mark, with the sha256 of its exact bytes
func (o *ops) readRaw(path string) *readResult {
	path = o.expand(path)
	size, status := o.probe(path)
	switch status {
	case "missing":
		return o.similar(path)
	case "not_regular":
		return notRegular(path)
	case "ok", "bad_size":
	default:
		return unavailable(path, status)
	}
	if image(path) {
		return &readResult{IsImage: true, IsBinary: true, FileSize: size}
	}
	if isBinary, sample := o.detectBinary(path); isBinary {
		return &readResult{IsBinary: true, FileSize: size, Error: describeBinary(sample, size)}
	}
	data, failed := o.exactBytes(path)
	if failed != nil {
		return &readResult{Error: "Failed to read file: " + failed.output}
	}
	return &readResult{Content: strings.TrimPrefix(string(data), utf8BOM), FileSize: size, sha256: digest(data)}
}

const utf8BOM = "\xef\xbb\xbf"

// Bytes of a whole file of at most max bytes, none for no limit
func (o *ops) readBytes(path string, max int) *readResult {
	path = o.expand(path)
	size, status := o.probe(path)
	switch status {
	case "missing":
		return &readResult{Error: notFound + path, NotFound: true}
	case "not_regular":
		return notRegular(path)
	case "ok":
	case "bad_size":
		return &readResult{Error: "Could not determine file size: " + path}
	default:
		return unavailable(path, status)
	}
	if max > 0 && size > int64(max) {
		return &readResult{FileSize: size, Error: "File is too large (" + strs.Thousands(int(size)) + " bytes, limit is " + strs.Thousands(max) + ")"}
	}
	if o.native {
		if data, ok := nativeRead(o.full(path)); ok {
			return &readResult{data: data, FileSize: size, IsBinary: true}
		}
	}
	encoded := o.exec("base64 < "+o.path(path), "", 0)
	if encoded.code != 0 {
		return &readResult{Error: "Failed to read binary file: " + encoded.output}
	}
	data, ok := unbase64(encoded.output)
	if !ok {
		return &readResult{Error: "Backend returned invalid binary data for: " + path}
	}
	return &readResult{data: data, FileSize: size, IsBinary: true}
}

// Bytes of one range of a file, start counted from zero
func (o *ops) readRange(path string, start int64, length int) *readResult {
	path = o.expand(path)
	size, status := o.probe(path)
	switch status {
	case "missing":
		return &readResult{Error: notFound + path, NotFound: true}
	case "not_regular":
		return notRegular(path)
	case "ok":
	case "bad_size":
		return &readResult{Error: "Could not determine file size: " + path}
	default:
		return unavailable(path, status)
	}
	if o.native {
		if f, err := openRegular(o.full(path)); err == nil && f != nil {
			defer f.Close()
			data := make([]byte, length)
			n, err := f.ReadAt(data, start)
			if err == nil || errors.Is(err, io.EOF) {
				return &readResult{data: data[:n], FileSize: size, IsBinary: true}
			}
		}
	}
	encoded := o.exec("tail -c +"+strconv.FormatInt(start+1, 10)+" "+o.path(path)+" | head -c "+strconv.Itoa(length)+" | base64", "", 0)
	if encoded.code != 0 {
		return &readResult{Error: "Failed to read binary file: " + encoded.output}
	}
	data, ok := unbase64(encoded.output)
	if !ok {
		return &readResult{Error: "Backend returned invalid binary data for: " + path}
	}
	return &readResult{data: data, FileSize: size, IsBinary: true}
}
