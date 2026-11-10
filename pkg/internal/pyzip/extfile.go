package pyzip

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// ZipExtFile.MAX_N, the read size of read with no limit
const maxN = 1<<31 - 1

// Member stream with ZipExtFile.read semantics alongside io.Reader
type pyFile interface {
	io.ReadCloser
	read(n int) ([]byte, error)
}

// zipfile.ZipExtFile of a member whose decompressor takes chunks with a max_length
type extFile struct {
	fp           *os.File
	pos          int64
	name         string
	compressLeft int64
	left         int64
	dec          pushDecompressor
	eof          bool
	readbuffer   []byte
	offset       int
	expectedCRC  uint32
	runningCRC   uint32
	closed       bool
}

func newExtFile(fp *os.File, dataStart int64, x *ZipInfo, dec pushDecompressor) *extFile {
	return &extFile{fp: fp, pos: dataStart, name: x.Filename, compressLeft: x.CompressSize, left: x.FileSize, dec: dec, expectedCRC: x.CRC}
}

// io.Reader view, one read of len(p) bytes and io.EOF once read returns nothing
func (f *extFile) Read(p []byte) (int, error) {
	data, err := f.read(len(p))
	if err != nil {
		return 0, err
	}
	if len(data) == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return copy(p, data), nil
}

// shutil.copyfileobj of the member into w
func (f *extFile) WriteTo(w io.Writer) (int64, error) { return copyFileObj(f.read, w) }

// Closes the member and releases its decompressor
func (f *extFile) Close() error {
	if !f.closed {
		f.closed = true
		f.dec.release()
	}
	return nil
}

// ZipExtFile.read of up to n bytes, everything to the end when n is negative
func (f *extFile) read(n int) ([]byte, error) {
	if f.closed {
		return nil, &ValueError{"read from closed file."}
	}
	if n < 0 {
		buf := f.readbuffer[f.offset:]
		f.readbuffer, f.offset = nil, 0
		for !f.eof {
			data, err := f.read1(maxN)
			if err != nil {
				return nil, err
			}
			buf = append(buf, data...)
		}
		return buf, nil
	}
	end := n + f.offset
	if end < len(f.readbuffer) {
		buf := f.readbuffer[f.offset:end]
		f.offset = end
		return buf, nil
	}
	n = end - len(f.readbuffer)
	buf := append([]byte(nil), f.readbuffer[f.offset:]...)
	f.readbuffer, f.offset = nil, 0
	for n > 0 && !f.eof {
		data, err := f.read1(n)
		if err != nil {
			return nil, err
		}
		if n < len(data) {
			f.readbuffer, f.offset = data, n
			buf = append(buf, data[:n]...)
			break
		}
		buf = append(buf, data...)
		n -= len(data)
	}
	return buf, nil
}

// ZipExtFile._read1, one bounded decompress call over at most one compressed read
func (f *extFile) read1(n int) ([]byte, error) {
	if f.eof || n <= 0 {
		return nil, nil
	}
	var data []byte
	if f.dec.wantsInput() {
		var err error
		if data, err = f.read2(n); err != nil {
			return nil, err
		}
	}
	data, err := f.dec.decompress(data, max(n, minReadSize))
	if err != nil {
		return nil, err
	}
	f.eof = f.dec.isEOF() || (f.compressLeft <= 0 && f.dec.wantsInput())
	data = data[:min(int64(len(data)), max(f.left, 0))]
	f.left -= int64(len(data))
	if f.left <= 0 {
		f.eof = true
	}
	return data, f.updateCRC(data)
}

// ZipExtFile._read2, at least MIN_READ_SIZE compressed bytes when that much is left
func (f *extFile) read2(n int) ([]byte, error) {
	if f.compressLeft <= 0 {
		return nil, nil
	}
	size := min(int64(max(n, minReadSize)), f.compressLeft)
	data := make([]byte, size)
	got, err := f.fp.ReadAt(data, f.pos)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	f.pos += int64(got)
	f.compressLeft -= int64(got)
	if got == 0 {
		return nil, &EOFError{}
	}
	return data[:got], nil
}

// ZipExtFile._update_crc, the CRC checked once the member ends
func (f *extFile) updateCRC(data []byte) error {
	f.runningCRC = crc32.Update(f.runningCRC, crc32.IEEETable, data)
	if f.eof && f.runningCRC != f.expectedCRC {
		return &BadZipFile{"Bad CRC-32 for file " + pyrepr.StrRepr(f.name)}
	}
	return nil
}

// shutil.COPY_BUFSIZE, 1 MiB on Windows and 256 KiB elsewhere
func copyBufSize() int {
	if runtime.GOOS == "windows" {
		return 1 << 20
	}
	return 256 << 10
}

// shutil.copyfileobj reading COPY_BUFSIZE chunks until an empty read
func copyFileObj(read func(int) ([]byte, error), w io.Writer) (int64, error) {
	var total int64
	for {
		buf, err := read(copyBufSize())
		if err != nil || len(buf) == 0 {
			return total, err
		}
		n, err := w.Write(buf)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
}

// zipfile.ZipFile.read, the whole member read through ZipExtFile.read
func (r *ZipReader) Read(name string) ([]byte, error) {
	x, err := r.GetInfo(name)
	if err != nil {
		return nil, err
	}
	f, err := r.openInfo(x, pyrepr.StrRepr(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.read(-1)
}

// zipfile.ZipFile.testzip, the first member whose read raises BadZipFile with true, other errors returned
func (r *ZipReader) TestZip() (string, bool, error) {
	for _, x := range r.Filelist {
		err := r.testMember(x.Filename)
		var bad *BadZipFile
		if errors.As(err, &bad) {
			return x.Filename, true, nil
		}
		if err != nil {
			return "", false, err
		}
	}
	return "", false, nil
}

// Opens a member by name and reads it in 1 MiB chunks to its CRC check
func (r *ZipReader) testMember(name string) error {
	x, err := r.GetInfo(name)
	if err != nil {
		return err
	}
	f, err := r.openInfo(x, pyrepr.StrRepr(name))
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		data, err := f.read(1 << 20)
		if err != nil || len(data) == 0 {
			return err
		}
	}
}

// zipfile.ZipFile.extractall into path, the working directory when nil, members nil meaning every name
func (r *ZipReader) ExtractAll(path *string, members []string) error {
	if members == nil {
		members = r.Namelist()
	}
	target := ""
	if path == nil {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		target = cwd
	} else {
		target = *path
	}
	for _, name := range members {
		if _, err := r.extractMember(name, target); err != nil {
			return err
		}
	}
	return nil
}

// zipfile.ZipFile._sanitize_windows_name replacing illegal characters and dropping trailing dots and spaces
func sanitizeWindowsName(arcname, pathsep string) string {
	arcname = strings.Map(func(c rune) rune {
		if strings.ContainsRune(`:<>|"?*`, c) {
			return '_'
		}
		return c
	}, arcname)
	var parts []string
	for _, x := range strings.Split(arcname, pathsep) {
		if x = strings.TrimRight(x, " ."); x != "" {
			parts = append(parts, x)
		}
	}
	return strings.Join(parts, pathsep)
}

// zipfile.ZipFile._extract_member, the written path or an empty one for a skipped invalid name
func (r *ZipReader) extractMember(name, targetpath string) (string, error) {
	member, err := r.GetInfo(name)
	if err != nil {
		return "", err
	}
	sep, arcname := "/", member.Filename
	if runtime.GOOS == "windows" {
		sep = `\`
		arcname = strings.ReplaceAll(arcname, "/", sep)
	}
	drive, root, arcname := pypath.SplitRoot(arcname)
	if r.IgnoreInvalidNames && (drive != "" || root != "") {
		return "", nil
	}
	if r.IgnoreInvalidNames && slices.Contains(strings.Split(arcname, sep), "..") {
		return "", nil
	}
	var parts []string
	for _, x := range strings.Split(arcname, sep) {
		if x != "" && x != "." && x != ".." {
			parts = append(parts, x)
		}
	}
	arcname = strings.Join(parts, sep)
	if runtime.GOOS == "windows" {
		sanitized := sanitizeWindowsName(arcname, sep)
		if r.IgnoreInvalidNames && sanitized != arcname {
			return "", nil
		}
		arcname = sanitized
	}
	if arcname == "" && !member.IsDir() {
		if r.IgnoreInvalidNames {
			return "", nil
		}
		return "", &ValueError{"Empty filename."}
	}
	targetpath = pypath.Normpath(pypath.Join(targetpath, arcname))
	if upperdirs := pypath.Dirname(targetpath); upperdirs != "" {
		if _, err := os.Stat(upperdirs); err != nil {
			if err := pyfs.Makedirs(upperdirs, 0o777, true); err != nil {
				return "", err
			}
		}
	}
	if member.IsDir() {
		if st, err := os.Stat(targetpath); err != nil || !st.IsDir() {
			if err := os.Mkdir(targetpath, 0o777); err != nil {
				if st, serr := os.Stat(targetpath); !errors.Is(err, fs.ErrExist) || serr != nil || !st.IsDir() {
					return "", err
				}
			}
		}
		return targetpath, nil
	}
	source, err := r.openInfo(member, member.Repr())
	if err != nil {
		return "", err
	}
	defer source.Close()
	target, err := os.OpenFile(targetpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return "", err
	}
	_, err = copyFileObj(source.read, target)
	if cerr := target.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return targetpath, nil
}

// zipfile.compressor_names
var compressorNames = map[int]string{0: "store", 1: "shrink", 2: "reduce", 3: "reduce", 4: "reduce", 5: "reduce", 6: "implode",
	7: "tokenize", 8: "deflate", 9: "deflate64", 10: "implode", 12: "bzip2", 14: "lzma", 18: "terse", 19: "lz77", 93: "zstd",
	97: "wavpack", 98: "ppmd"}

// zipfile.ZipInfo.__repr__
func (z *ZipInfo) Repr() string {
	var b strings.Builder
	b.WriteString("<ZipInfo filename=" + pyrepr.StrRepr(z.Filename))
	if z.CompressType != Stored {
		if name, ok := compressorNames[z.CompressType]; ok {
			b.WriteString(" compress_type=" + name)
		} else {
			fmt.Fprintf(&b, " compress_type=%d", z.CompressType)
		}
	}
	hi, lo := z.ExternalAttr>>16, z.ExternalAttr&0xFFFF
	if hi != 0 {
		b.WriteString(" filemode=" + pyrepr.StrRepr(pyfs.Filemode(hi)))
	}
	if lo != 0 {
		fmt.Fprintf(&b, " external_attr=%#x", lo)
	}
	isdir := z.IsDir()
	if !isdir || z.FileSize != 0 {
		fmt.Fprintf(&b, " file_size=%d", z.FileSize)
	}
	if (!isdir || z.CompressSize != 0) && (z.CompressType != Stored || z.FileSize != z.CompressSize) {
		fmt.Fprintf(&b, " compress_size=%d", z.CompressSize)
	}
	b.WriteString(">")
	return b.String()
}
