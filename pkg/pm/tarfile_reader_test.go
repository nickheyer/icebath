//go:build linux

package pm

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// Raw header fields, numeric ones as the exact text written into the block
type trdHdr struct {
	name     string
	mode     string
	uid      string
	gid      string
	size     string
	mtime    string
	chksum   string
	typ      byte
	linkname string
	magic    string
	uname    string
	gname    string
	devmajor string
	devminor string
	prefix   string
	raw      map[int][]byte
	signed   bool
}

// Octal field text padded to width with a trailing NUL
func trdOct(n int64, width int) string { return fmt.Sprintf("%0*o\x00", width-1, n) }

// Base 256 field holding n, a negative n in two's complement under the 0o377 marker
func trdB256(n int64, width int) string {
	b := make([]byte, width)
	b[0] = 0o200
	if n < 0 {
		b[0] = 0o377
	}
	for i := width - 1; i > 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return string(b)
}

// Header block with ustar magic, default fields and a checksum computed unless one was given
func (h trdHdr) block() []byte {
	b := make([]byte, tarBlockSize)
	put := func(off, width int, v string) { copy(b[off:off+width], v) }
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	put(0, 100, h.name)
	put(100, 8, or(h.mode, trdOct(0o644, 8)))
	put(108, 8, or(h.uid, trdOct(0, 8)))
	put(116, 8, or(h.gid, trdOct(0, 8)))
	put(124, 12, or(h.size, trdOct(0, 12)))
	put(136, 12, or(h.mtime, trdOct(1_000_000_000, 12)))
	b[156] = h.typ
	put(157, 100, h.linkname)
	if h.magic != "v7" {
		put(257, 8, or(h.magic, "ustar\x0000"))
		put(265, 32, h.uname)
		put(297, 32, h.gname)
		put(329, 8, or(h.devmajor, trdOct(0, 8)))
		put(337, 8, or(h.devminor, trdOct(0, 8)))
		put(345, 155, h.prefix)
	}
	for off, v := range h.raw {
		copy(b[off:], v)
	}
	if h.chksum != "" {
		put(148, 8, h.chksum)
		return b
	}
	copy(b[148:156], "        ")
	var sum int64
	for _, c := range b {
		if h.signed {
			sum += int64(int8(c))
		} else {
			sum += int64(c)
		}
	}
	put(148, 8, fmt.Sprintf("%06o\x00 ", sum))
	return b
}

// Data padded with NULs to a whole number of blocks
func trdPad(data []byte) []byte {
	out := append([]byte(nil), data...)
	if r := len(out) % tarBlockSize; r != 0 {
		out = append(out, make([]byte, tarBlockSize-r)...)
	}
	return out
}

// Pax record text for one keyword and value with its self counting length prefix
func trdRecord(key, value string) string {
	body := " " + key + "=" + value + "\n"
	n := len(body) + 1
	for len(fmt.Sprint(n))+len(body) != n {
		n = len(fmt.Sprint(n)) + len(body)
	}
	return fmt.Sprint(n) + body
}

// Extended header of the given type carrying the records in order
func trdPax(typ byte, records ...[2]string) []byte {
	var payload strings.Builder
	for _, r := range records {
		payload.WriteString(trdRecord(r[0], r[1]))
	}
	h := trdHdr{name: "././@PaxHeader", typ: typ, size: trdOct(int64(payload.Len()), 12)}
	return append(h.block(), trdPad([]byte(payload.String()))...)
}

// Regular member header followed by its padded data
func trdFile(name string, data string, mode int64, mtime int64) []byte {
	h := trdHdr{name: name, typ: tarRegType, size: trdOct(int64(len(data)), 12), mode: trdOct(mode, 8), mtime: trdOct(mtime, 12)}
	return append(h.block(), trdPad([]byte(data))...)
}

// Archive of the given blocks closed by two zero blocks
func trdTar(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return append(out, make([]byte, 2*tarBlockSize)...)
}

// Archive of the given blocks with no end of archive marker
func trdRawTar(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// Gzip compressed copy of an archive
func trdGzip(t testing.TB, data []byte) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Xz compressed copy of an archive
func trdXz(t testing.TB, data []byte) []byte {
	var b bytes.Buffer
	w, err := xz.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Zstd compressed copy of an archive
func trdZstd(t testing.TB, data []byte) []byte {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

// LZMA_alone stream of data, its size in the header or left unknown with an end marker
func trdLZMAAlone(t testing.TB, data []byte, sized bool) []byte {
	var b bytes.Buffer
	cfg := lzma.WriterConfig{DictCap: 1 << 16, EOSMarker: !sized}
	if sized {
		cfg.SizeInHeader, cfg.Size = true, int64(len(data))
	}
	w, err := cfg.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Lzip member of data in the given version, with its trailer CRC replaced when crc is nonzero
func trdLzip(t testing.TB, data []byte, version byte, crc uint32) []byte {
	stream := trdLZMAAlone(t, data, false)[13:]
	out := append([]byte("LZIP"), version, 16)
	out = append(out, stream...)
	if crc == 0 {
		crc = crc32.ChecksumIEEE(data)
	}
	out = binary.LittleEndian.AppendUint32(out, crc)
	out = binary.LittleEndian.AppendUint64(out, uint64(len(data)))
	if version == 1 {
		out = binary.LittleEndian.AppendUint64(out, uint64(6+len(stream)+20))
	}
	return out
}

// Archive of forty half compressible files of 12 KiB each, large enough to span several codec windows
func trdBig() []byte {
	var parts [][]byte
	state := uint32(2463534242)
	for i := range 40 {
		data := make([]byte, 12*1024)
		for j := range data {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			data[j] = "abcdefgh"[state%8]
		}
		parts = append(parts, trdFile(fmt.Sprintf("big/%02d", i), string(data), 0o644, 1_000_000_000+int64(i)))
	}
	return trdTar(parts...)
}

// One crafted archive by name
type trdCase struct {
	name    string
	archive func(t testing.TB) []byte
}

// Mode, kind, time, link count and content of every path under root, then the error, as one comparable text
func trdSummary(t testing.TB, root string, extractErr error) string {
	var lines []string
	now := time.Now().UnixNano()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mtime := fmt.Sprint(st.ModTime().UnixNano())
		if diff := st.ModTime().UnixNano() - now; diff > -int64(24*time.Hour) && diff < int64(24*time.Hour) {
			mtime = "recent"
		}
		nlink := uint64(1)
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			nlink = uint64(sys.Nlink)
		}
		switch {
		case st.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			lines = append(lines, fmt.Sprintf("%s link -> %s", tarRepr(rel), tarRepr(target)))
		case st.IsDir():
			mode := fmt.Sprintf("%o", st.Mode().Perm())
			if st.Mode().Perm() == trdDirDefault() {
				mode = "default"
			}
			lines = append(lines, fmt.Sprintf("%s dir mode=%s mtime=%s", tarRepr(rel), mode, mtime))
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content := tarBytesRepr(data)
			if len(data) > 64 {
				sum := sha256.Sum256(data)
				content = fmt.Sprintf("sha=%s", hex.EncodeToString(sum[:])[:16])
			}
			lines = append(lines, fmt.Sprintf("%s file mode=%o mtime=%s nlink=%d size=%d %s", tarRepr(rel), st.Mode().Perm(), mtime, nlink, len(data), content))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	text := strings.ReplaceAll(trdErrorText(extractErr), realpath(root), "<dest>")
	lines = append(lines, "error "+strings.ReplaceAll(text, realpath(filepath.Dir(root)), "<parent>"))
	return strings.Join(lines, "\n")
}

// Mode a directory made without an explicit mode gets under the process umask
var trdDirDefault = sync.OnceValue(func() fs.FileMode {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return fs.FileMode(0o777 &^ old)
})

// Python class and message of an extraction error, an OS error by its errno alone
func trdErrorText(err error) string {
	var read *TarReadError
	var filter *TarFilterError
	var errno syscall.Errno
	switch {
	case err == nil:
		return "none"
	case errors.As(err, &read):
		return read.Class + ": " + read.Message
	case errors.As(err, &filter):
		return filter.Kind + ": " + filter.Error()
	case errors.As(err, &errno):
		return fmt.Sprintf("OSError[%d]", int(errno))
	}
	return "unclassified: " + err.Error()
}

// Writes an archive to a fresh directory and extracts it into another, returning the tree summary
func trdExtract(t testing.TB, archive []byte) string {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.tar")
	if err := os.WriteFile(path, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	err := ExtractTarFile(path, dest)
	return trdSummary(t, dest, err)
}

// Header only member of the given type and name
func trdEntry(typ byte, name, linkname string) []byte {
	return trdHdr{name: name, typ: typ, linkname: linkname, mode: trdOct(0o755, 8)}.block()
}

// GNU longname or longlink block carrying text
func trdLong(typ byte, text string) []byte {
	h := trdHdr{name: "././@LongLink", typ: typ, size: trdOct(int64(len(text)+1), 12), magic: "ustar  \x00"}
	return append(h.block(), trdPad([]byte(text+"\x00"))...)
}

// Old GNU sparse member with up to four runs in its header and the rest in extension blocks
func trdOldSparse(name string, runs [][2]int64, realsize int64, data string, headerSize string) []byte {
	raw := map[int][]byte{}
	head := runs
	if len(head) > 4 {
		head = runs[:4]
	}
	for i, r := range head {
		raw[386+24*i] = []byte(trdOct(r[0], 12))
		raw[386+24*i+12] = []byte(trdOct(r[1], 12))
	}
	rest := runs[len(head):]
	if len(rest) > 0 {
		raw[482] = []byte{1}
	}
	raw[483] = []byte(trdOct(realsize, 12))
	if headerSize == "" {
		headerSize = trdOct(int64(len(data)), 12)
	}
	h := trdHdr{name: name, typ: tarGNUSparse, size: headerSize, magic: "ustar  \x00", raw: raw}
	out := h.block()
	for len(rest) > 0 {
		blk := make([]byte, tarBlockSize)
		n := min(21, len(rest))
		for i, r := range rest[:n] {
			copy(blk[24*i:], trdOct(r[0], 12))
			copy(blk[24*i+12:], trdOct(r[1], 12))
		}
		rest = rest[n:]
		if len(rest) > 0 {
			blk[504] = 1
		}
		out = append(out, blk...)
	}
	return append(out, trdPad([]byte(data))...)
}

// Every crafted case, each archive built in Go from raw header blocks
func trdCases() []trdCase {
	file := func(name, data string) []byte { return trdFile(name, data, 0o644, 1_000_000_000) }
	zeros := func(n int) []byte { return make([]byte, n*tarBlockSize) }
	garbage := bytes.Repeat([]byte("garbage!"), 64)
	cases := []trdCase{
		{name: "plain", archive: func(testing.TB) []byte {
			return trdTar(trdFile("a", "alpha", 0o755, 1_100_000_000), trdEntry(tarDirType, "d/", ""), file("d/b", "bravo"), trdEntry(tarSymType, "l", "a"), trdEntry(tarLnkType, "h", "a"))
		}},
		{name: "solaris_x", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarSolarisXHDType, [2]string{"path", "solaris/long/name.txt"}, [2]string{"mtime", "1234567890.5"}), file("short", "solaris"))
		}},
		{name: "solaris_x_size", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarSolarisXHDType, [2]string{"size", "700"}), file("big", "0123456789"), file("next", "n"))
		}},
		{name: "global_size_bigger", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"size", "700"}), file("a", "0123456789"), file("b", "hello"))
		}},
		{name: "global_size_smaller", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"size", "3"}), file("a", "0123456789"))
		}},
		{name: "global_size_beyond_end", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"size", "5000"}), file("a", "0123456789"))
		}},
		{name: "global_size_negative", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"size", "-3"}), file("a", "hello"), zeros(40))
		}},
		{name: "global_path", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"path", "renamed///"}), file("a", "first"), file("b", "second"))
		}},
		{name: "global_mtime_float", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"mtime", "1500000000.987654321"}), file("a", "x"), trdEntry(tarDirType, "d", ""))
		}},
		{name: "global_then_local", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXGLType, [2]string{"mtime", "1600000000"}, [2]string{"path", "g"}), trdPax(tarXHDType, [2]string{"path", "local"}), file("a", "x"), file("b", "y"))
		}},
		{name: "global_sparse_size_invalid", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXGLType, [2]string{"GNU.sparse.size", "abc"}), file("a", "x"))
		}},
		{name: "pax_mtime_values", archive: func(testing.TB) []byte {
			var parts [][]byte
			for i, v := range []string{"1.5", "-1.25", "1700000000.123456789", "bogus", "١٢٣٤٥٦٧٨٩٠", "1_000_000_000.25", " 42 ", "1e9", "\x1c5"} {
				parts = append(parts, trdPax(tarXHDType, [2]string{"mtime", v}), file(fmt.Sprintf("m%d", i), "t"))
			}
			return trdTar(parts...)
		}},
		{name: "pax_mtime_nan", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXHDType, [2]string{"mtime", "nan"}), file("a", "x"))
		}},
		{name: "pax_mtime_inf", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"mtime", "-inf"}), file("a", "x"))
		}},
		{name: "pax_mtime_dir_huge", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXHDType, [2]string{"mtime", "1e300"}), trdEntry(tarDirType, "d", ""))
		}},
		{name: "pax_size_invalid", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"size", "abc"}), file("a", "hello"), file("b", "bee"))
		}},
		{name: "pax_size_negative_first", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"size", "-5"}), file("a", "hello"), file("b", "bee"))
		}},
		{name: "pax_size_negative_later", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXHDType, [2]string{"size", "-5"}), file("a", "hello"), file("b", "bee"))
		}},
		{name: "pax_size_digits_limit", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"size", strings.Repeat("9", 5000)}), file("a", "hello"))
		}},
		{name: "pax_path_trailing", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"path", "dir///"}), trdEntry(tarDirType, "x", ""), file("dir/f", "in"))
		}},
		{name: "pax_path_absolute", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"path", "/abs/file"}), file("a", "abs"))
		}},
		{name: "pax_linkpath", archive: func(testing.TB) []byte {
			return trdTar(file("target", "t"), trdPax(tarXHDType, [2]string{"linkpath", "target"}), trdEntry(tarSymType, "sym", "ignored"))
		}},
		{name: "pax_invalid_record_later", archive: func(testing.TB) []byte {
			h := trdHdr{name: "pax", typ: tarXHDType, size: trdOct(8, 12)}
			return trdTar(file("first", "1"), h.block(), trdPad([]byte("garbage\n")), file("a", "x"))
		}},
		{name: "pax_invalid_record_first", archive: func(testing.TB) []byte {
			h := trdHdr{name: "pax", typ: tarXHDType, size: trdOct(8, 12)}
			return trdTar(h.block(), trdPad([]byte("garbage\n")), file("a", "x"))
		}},
		{name: "pax_record_edge_lengths", archive: func(testing.TB) []byte {
			payload := trdRecord("path", "edge") + strings.Repeat("0", 25) + "5 x=\n"
			h := trdHdr{name: "pax", typ: tarXHDType, size: trdOct(int64(len(payload)), 12)}
			return trdTar(file("first", "1"), h.block(), trdPad([]byte(payload)), file("a", "x"))
		}},
		{name: "pax_then_garbage", archive: func(testing.TB) []byte {
			return trdRawTar(file("first", "1"), trdPax(tarXHDType, [2]string{"path", "p"}), garbage, zeros(2))
		}},
		{name: "invalid_after_first", archive: func(testing.TB) []byte {
			return trdRawTar(file("a", "alpha"), garbage, file("b", "never"), zeros(2))
		}},
		{name: "truncated_after_first", archive: func(testing.TB) []byte {
			return trdRawTar(file("a", "alpha"), garbage[:100])
		}},
		{name: "zero_block_middle", archive: func(testing.TB) []byte {
			return trdTar(file("a", "alpha"), zeros(1), file("b", "bravo"))
		}},
		{name: "no_end_marker", archive: func(testing.TB) []byte { return trdRawTar(file("a", "alpha")) }},
		{name: "missing_padding", archive: func(testing.TB) []byte {
			return trdRawTar(trdHdr{name: "a", typ: tarRegType, size: trdOct(10, 12)}.block(), []byte("0123456789"))
		}},
		{name: "truncated_data", archive: func(testing.TB) []byte {
			return trdRawTar(trdHdr{name: "a", typ: tarRegType, size: trdOct(1000, 12)}.block(), bytes.Repeat([]byte("z"), 600))
		}},
		{name: "first_bad_checksum", archive: func(testing.TB) []byte { return trdTar(garbage) }},
		{name: "first_invalid_number", archive: func(testing.TB) []byte {
			return trdTar(trdHdr{name: "a", typ: tarRegType, mode: "0x644\x00\x00\x00"}.block())
		}},
		{name: "empty_file", archive: func(testing.TB) []byte { return nil }},
		{name: "tiny_file", archive: func(testing.TB) []byte { return garbage[:100] }},
		{name: "all_zero", archive: func(testing.TB) []byte { return zeros(2) }},
		{name: "gnu_longname_longlink", archive: func(testing.TB) []byte {
			long := strings.Repeat("n", 150) + "/" + strings.Repeat("m", 60)
			return trdTar(trdLong(tarGNULongName, long), file("short", "long"), trdLong(tarGNULongLink, long), trdEntry(tarSymType, "sym", "x"), trdLong(tarGNULongName, "dd//"), trdEntry(tarDirType, "ignored", ""))
		}},
		{name: "gnu_longname_regular_slash", archive: func(testing.TB) []byte {
			h := trdHdr{name: "ignored", typ: tarARegType, size: trdOct(3, 12)}
			return trdTar(trdLong(tarGNULongName, "x/"), h.block(), trdPad([]byte("abc")))
		}},
		{name: "gnu_longname_then_eof", archive: func(testing.TB) []byte {
			return trdRawTar(file("first", "1"), trdLong(tarGNULongName, "lost"))
		}},
		{name: "gnu_longname_first_then_garbage", archive: func(testing.TB) []byte {
			return trdTar(trdLong(tarGNULongName, "lost"), garbage)
		}},
		{name: "sparse_old", archive: func(testing.TB) []byte {
			runs := [][2]int64{{0, 4}, {1000, 4}, {2000, 4}, {3000, 4}, {4000, 4}, {5000, 4}}
			return trdTar(trdOldSparse("sparse", runs, 6000, "aaaabbbbccccddddeeeeffff", ""), file("after", "after"))
		}},
		{name: "sparse_old_zero_runs", archive: func(testing.TB) []byte {
			return trdTar(trdOldSparse("sparse", [][2]int64{{10, 3}}, 20, "xyz", ""), file("after", "after"))
		}},
		{name: "sparse_old_huge_header_size", archive: func(testing.TB) []byte {
			return trdTar(trdOldSparse("sparse", [][2]int64{{0, 5}}, 5, "hello", trdB256(1<<62, 12)), file("after", "after"))
		}},
		{name: "sparse_old_negative_realsize", archive: func(testing.TB) []byte {
			h := trdOldSparse("sparse", [][2]int64{{0, 5}}, 0, "hello", "")
			copy(h[483:495], trdB256(-5, 12))
			return trdTar(trdRechecksum(h), file("after", "after"))
		}},
		{name: "sparse_pax00", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.size", "3000"}, [2]string{"GNU.sparse.numblocks", "2"}, [2]string{"GNU.sparse.offset", "100"}, [2]string{"GNU.sparse.numbytes", "3"}, [2]string{"GNU.sparse.offset", "2000"}, [2]string{"GNU.sparse.numbytes", "4"}, [2]string{"GNU.sparse.name", "sparse00"})
			return trdTar(pax, file("GNUSparseFile.0/sparse00", "abcdefg"), file("after", "after"))
		}},
		{name: "sparse_pax00_bad_offset", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.size", "3000"}, [2]string{"GNU.sparse.offset", "1x"}, [2]string{"GNU.sparse.numbytes", "3"})
			return trdTar(file("first", "1"), pax, file("s", "abc"), file("after", "after"))
		}},
		{name: "sparse_pax01", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.size", "5000"}, [2]string{"GNU.sparse.numblocks", "2"}, [2]string{"GNU.sparse.map", "10,2,4000,3"}, [2]string{"GNU.sparse.name", "sparse01"})
			return trdTar(pax, file("GNUSparseFile.0/sparse01", "abcde"), file("after", "after"))
		}},
		{name: "sparse_pax01_bad_map", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.map", "1,x"})
			return trdTar(file("first", "1"), pax, file("s", "abc"))
		}},
		{name: "sparse_pax10", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.major", "1"}, [2]string{"GNU.sparse.minor", "0"}, [2]string{"GNU.sparse.name", "sparse10"}, [2]string{"GNU.sparse.realsize", "9000"})
			data := trdPad([]byte("2\n5\n3\n8000\n4\n"))
			data = append(data, []byte("abcdefg")...)
			h := trdHdr{name: "GNUSparseFile.0/sparse10", typ: tarRegType, size: trdOct(int64(len(data)), 12)}
			return trdTar(pax, h.block(), trdPad(data), file("after", "after"))
		}},
		{name: "sparse_pax10_bad_count", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.major", "1"}, [2]string{"GNU.sparse.minor", "0"})
			h := trdHdr{name: "s", typ: tarRegType, size: trdOct(512, 12)}
			return trdTar(file("first", "1"), pax, h.block(), trdPad([]byte("\nzzz")))
		}},
		{name: "sparse_pax10_no_newline", archive: func(testing.TB) []byte {
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.major", "1"}, [2]string{"GNU.sparse.minor", "0"})
			h := trdHdr{name: "s", typ: tarRegType, size: trdOct(512, 12)}
			return trdRawTar(file("first", "1"), pax, h.block(), bytes.Repeat([]byte("7"), 512))
		}},
		{name: "base256_fields", archive: func(testing.TB) []byte {
			a := trdHdr{name: "a", typ: tarRegType, size: trdB256(5, 12), mtime: trdB256(1<<33, 12)}
			b := trdHdr{name: "b", typ: tarRegType, size: trdOct(1, 12), mtime: trdB256(-1000, 12), mode: trdB256(0o4777, 8)}
			return trdTar(a.block(), trdPad([]byte("12345")), b.block(), trdPad([]byte("b")))
		}},
		{name: "base256_huge_mtime", archive: func(testing.TB) []byte {
			mtime := "\x80" + strings.Repeat("\xff", 11)
			return trdTar(trdHdr{name: "a", typ: tarRegType, mtime: mtime}.block())
		}},
		{name: "base256_huge_size", archive: func(testing.TB) []byte {
			size := "\x80" + strings.Repeat("\x7f", 11)
			return trdTar(trdHdr{name: "a", typ: tarRegType, size: size}.block(), trdPad([]byte("partial")))
		}},
		{name: "nonutf8_names", archive: func(testing.TB) []byte {
			return trdTar(file("caf\xe9", "latin"), trdPax(tarXHDType, [2]string{"path", "n\xffx"}), file("p1", "raw"), trdPax(tarXHDType, [2]string{"hdrcharset", "BINARY"}, [2]string{"path", "b\xfe\xc3\xa9"}), file("p2", "binary"), trdPax(tarXHDType, [2]string{"path", "u\xc3\xa9"}), file("p3", "utf8"))
		}},
		{name: "ustar_prefix", archive: func(testing.TB) []byte {
			a := trdHdr{name: "file.txt", prefix: "pre/fix", typ: tarRegType, size: trdOct(2, 12)}
			d := trdHdr{name: "sub//", prefix: "pre", typ: tarDirType}
			return trdTar(a.block(), trdPad([]byte("pf")), d.block())
		}},
		{name: "gnu_header_prefix_area", archive: func(testing.TB) []byte {
			h := trdHdr{name: "gnufile", typ: tarRegType, size: trdOct(1, 12), magic: "ustar  \x00", raw: map[int][]byte{345: []byte(trdOct(1_000_000_000, 12))}}
			return trdTar(h.block(), trdPad([]byte("g")))
		}},
		{name: "v7_archive", archive: func(testing.TB) []byte {
			d := trdHdr{name: "dir/", typ: tarARegType, magic: "v7"}
			f := trdHdr{name: "dir/file", typ: tarARegType, size: trdOct(2, 12), magic: "v7"}
			return trdTar(d.block(), f.block(), trdPad([]byte("v7")))
		}},
		{name: "signed_checksum", archive: func(testing.TB) []byte {
			h := trdHdr{name: "hi\xc3\xa9\xff", typ: tarRegType, size: trdOct(1, 12), signed: true}
			return trdTar(h.block(), trdPad([]byte("s")))
		}},
		{name: "octal_variants", archive: func(testing.TB) []byte {
			a := trdHdr{name: "a", typ: tarRegType, mode: " 755 \x00\x00\x00", size: " 2\x00", mtime: "0o7777777777"}
			b := trdHdr{name: "b", typ: tarRegType, mode: "6_4_4\x00", size: "0_1", mtime: "\x1c 1234567 \x00"}
			return trdTar(a.block(), trdPad([]byte("aa")), b.block(), trdPad([]byte("b")))
		}},
		{name: "unknown_type", archive: func(testing.TB) []byte {
			return trdTar(trdHdr{name: "vol", typ: 'V', size: trdOct(3, 12)}.block(), trdPad([]byte("vol")))
		}},
		{name: "hardlink_dir_chain", archive: func(testing.TB) []byte {
			g := trdHdr{name: "g", typ: tarLnkType, linkname: "d", mode: trdOct(0o640, 8)}
			h := trdHdr{name: "h", typ: tarLnkType, linkname: "g", mode: trdOct(0o750, 8)}
			return trdTar(trdEntry(tarDirType, "d", ""), g.block(), h.block())
		}},
		{name: "hardlink_missing", archive: func(testing.TB) []byte {
			return trdTar(file("a", "x"), trdEntry(tarLnkType, "h", "nowhere"))
		}},
		{name: "symlink_fallback_order", archive: func(testing.TB) []byte {
			return trdTar(file("a", "first"), trdEntry(tarLnkType, "h", "a"), file("a", "second"), trdEntry(tarLnkType, "h2", "a"))
		}},
		{name: "contiguous_type", archive: func(testing.TB) []byte {
			return trdTar(trdHdr{name: "c", typ: tarContType, size: trdOct(4, 12)}.block(), trdPad([]byte("cont")))
		}},
		{name: "dir_sorting", archive: func(testing.TB) []byte {
			mk := func(name string, mt int64) []byte {
				return trdHdr{name: name, typ: tarDirType, mtime: trdOct(mt, 12)}.block()
			}
			return trdTar(mk("z\xc3\xa9", 1_000_000_001), mk("z\xff", 1_000_000_002), mk("z\xc3\xa9/in", 1_000_000_003), file("z\xff/f", "f"))
		}},
		{name: "sparse_old_beyond_int64", archive: func(testing.TB) []byte {
			return trdTar(trdOldSparse("sparse", [][2]int64{{0, 5}}, 5, "hello", "\x80"+strings.Repeat("\x7f", 11)), file("after", "after"))
		}},
		{name: "sparse_old_truncated_extension", archive: func(testing.TB) []byte {
			runs := [][2]int64{{0, 1}, {10, 1}, {20, 1}, {30, 1}, {40, 1}}
			h := trdOldSparse("sparse", runs, 50, "abcde", "")
			return trdRawTar(file("first", "1"), h[:tarBlockSize+300])
		}},
		{name: "sparse_old_bad_extension_number", archive: func(testing.TB) []byte {
			runs := [][2]int64{{0, 1}, {10, 1}, {20, 1}, {30, 1}, {40, 1}}
			h := trdOldSparse("sparse", runs, 50, "abcde", "")
			copy(h[tarBlockSize:], "zz")
			return trdTar(file("first", "1"), h, file("after", "after"))
		}},
		{name: "sparse_old_many_runs", archive: func(testing.TB) []byte {
			var runs [][2]int64
			var data strings.Builder
			for i := range 30 {
				runs = append(runs, [2]int64{int64(i) * 100, 2})
				data.WriteString(fmt.Sprintf("%02d", i))
			}
			return trdTar(trdOldSparse("many", runs, 3100, data.String(), ""), file("after", "after"))
		}},
		{name: "sparse_after_pax_size", archive: func(testing.TB) []byte {
			runs := [][2]int64{{0, 1}, {10, 1}, {20, 1}, {30, 1}, {40, 1}}
			return trdTar(trdPax(tarXHDType, [2]string{"size", "5"}, [2]string{"path", "renamed"}), trdOldSparse("sparse", runs, 50, "abcde", ""), file("after", "after"))
		}},
		{name: "sparse_pax10_long_map", archive: func(testing.TB) []byte {
			var m strings.Builder
			m.WriteString("120\n")
			var data strings.Builder
			for i := range 120 {
				m.WriteString(fmt.Sprintf("%d\n1\n", i*1000))
				data.WriteByte(byte('a' + i%26))
			}
			pax := trdPax(tarXHDType, [2]string{"GNU.sparse.major", "1"}, [2]string{"GNU.sparse.minor", "0"}, [2]string{"GNU.sparse.name", "longmap"}, [2]string{"GNU.sparse.realsize", "120000"})
			body := append(trdPad([]byte(m.String())), []byte(data.String())...)
			h := trdHdr{name: "x", typ: tarRegType, size: trdOct(int64(len(body)), 12)}
			return trdTar(pax, h.block(), trdPad(body), file("after", "after"))
		}},
		{name: "pax_size_before_longname", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"size", "3"}), trdLong(tarGNULongName, "longnamed"), file("short", "abcdefgh"), file("after", "after"))
		}},
		{name: "pax_path_and_sparse_name_order", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"GNU.sparse.name", "sparsename/"}, [2]string{"path", "pathname/"}), file("a", "1"), trdPax(tarXHDType, [2]string{"path", "first/"}, [2]string{"GNU.sparse.name", "second/"}), file("b", "2"))
		}},
		{name: "pax_duplicate_keys", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"path", "one"}, [2]string{"mtime", "5"}, [2]string{"path", "two=x"}, [2]string{"hdrcharset", "BINARY"}, [2]string{"hdrcharset", "ISO-IR 10646 2000 UTF-8"}), file("a", "dup"))
		}},
		{name: "pax_truncated_payload", archive: func(testing.TB) []byte {
			h := trdHdr{name: "pax", typ: tarXHDType, size: trdOct(2000, 12)}
			return trdRawTar(file("first", "1"), h.block(), []byte(trdRecord("path", "p")))
		}},
		{name: "global_then_eof", archive: func(testing.TB) []byte {
			return trdRawTar(file("first", "1"), trdPax(tarXGLType, [2]string{"path", "p"}))
		}},
		{name: "global_linkpath_symlink", archive: func(testing.TB) []byte {
			return trdTar(file("t", "t"), trdPax(tarXGLType, [2]string{"linkpath", "t"}), trdEntry(tarSymType, "s", "other"), file("f", "f"))
		}},
		{name: "solaris_x_middle", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarSolarisXHDType, [2]string{"path", "sol"}, [2]string{"size", "2"}), file("x", "abcdef"), file("after", "after"))
		}},
		{name: "dir_with_size", archive: func(testing.TB) []byte {
			d := trdHdr{name: "d", typ: tarDirType, size: trdOct(512, 12)}
			return trdTar(d.block(), file("hidden", "in dir data"), file("after", "after"))
		}},
		{name: "symlink_with_size", archive: func(testing.TB) []byte {
			s := trdHdr{name: "s", typ: tarSymType, linkname: "after", size: trdOct(512, 12)}
			return trdTar(s.block(), file("hidden", "in link data"), file("after", "after"))
		}},
		{name: "regtype_trailing_slash", archive: func(testing.TB) []byte {
			return trdTar(file("x/", "slash"), trdHdr{name: "v/", typ: tarARegType, magic: "ustar  \x00"}.block())
		}},
		{name: "negative_size_first", archive: func(testing.TB) []byte {
			return trdTar(trdHdr{name: "a", typ: tarRegType, size: trdB256(-5, 12)}.block())
		}},
		{name: "negative_size_later", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdHdr{name: "a", typ: tarRegType, size: trdB256(-5, 12)}.block(), file("after", "after"))
		}},
		{name: "chksum_base256", archive: func(testing.TB) []byte {
			b := trdHdr{name: "a", typ: tarRegType, size: trdOct(1, 12)}.block()
			copy(b[148:156], "        ")
			var sum int64
			for _, c := range b {
				sum += int64(c)
			}
			copy(b[148:156], trdB256(sum, 8))
			return trdTar(b, trdPad([]byte("c")))
		}},
		{name: "non_ascii_number_later", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdHdr{name: "a", typ: tarRegType, mode: "\xb5\x00"}.block(), file("after", "after"))
		}},
		{name: "bad_devmajor_first", archive: func(testing.TB) []byte {
			return trdTar(trdHdr{name: "a", typ: tarRegType, devmajor: "9\x00"}.block())
		}},
		{name: "longname_huge_size", archive: func(testing.TB) []byte {
			h := trdHdr{name: "././@LongLink", typ: tarGNULongName, size: "\x80" + strings.Repeat("\x01", 11), magic: "ustar  \x00"}
			return trdTar(file("first", "1"), h.block(), file("after", "after"))
		}},
		{name: "longname_and_pax_path", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"path", "paxname"}), trdLong(tarGNULongName, "gnuname"), file("short", "both"), trdLong(tarGNULongName, "gnu2"), trdPax(tarXHDType, [2]string{"path", "pax2"}), file("short2", "both2"))
		}},
		{name: "pax_path_nul", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXHDType, [2]string{"path", "a\x00b"}), file("a", "x"))
		}},
		{name: "pax_linkpath_nul_symlink", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXHDType, [2]string{"linkpath", "fi\x00rst"}), trdEntry(tarSymType, "s", "x"))
		}},
		{name: "pax_linkpath_nul_hardlink", archive: func(testing.TB) []byte {
			return trdTar(file("first", "1"), trdPax(tarXHDType, [2]string{"linkpath", "fi\x00rst"}), trdEntry(tarLnkType, "h", "x"))
		}},
		{name: "pax_path_empty", archive: func(testing.TB) []byte {
			return trdTar(trdPax(tarXHDType, [2]string{"path", ""}), file("a", "x"))
		}},
		{name: "dot_names", archive: func(testing.TB) []byte {
			return trdTar(trdEntry(tarDirType, "./", ""), trdEntry(tarDirType, ".", ""), file("./f", "dot"))
		}},
		{name: "filter_outside_nonutf8", archive: func(testing.TB) []byte {
			return trdTar(file("ok", "1"), file("../caf\xe9", "out"))
		}},
		{name: "filter_absolute_symlink", archive: func(testing.TB) []byte {
			return trdTar(trdEntry(tarSymType, "s\xff", "/etc/passwd"))
		}},
		{name: "filter_symlink_escape", archive: func(testing.TB) []byte {
			return trdTar(trdEntry(tarDirType, "d", ""), trdEntry(tarSymType, "d/up", "../../x\xfe"))
		}},
		{name: "filter_hardlink_outside", archive: func(testing.TB) []byte {
			return trdTar(trdEntry(tarLnkType, "h", "../outside"))
		}},
		{name: "filter_special", archive: func(testing.TB) []byte {
			return trdTar(file("ok", "1"), trdEntry(tarFifoType, "fifo\x01", ""))
		}},
		{name: "link_fallback_rejected", archive: func(testing.TB) []byte {
			return trdTar(trdEntry(tarDirType, "d", ""), trdEntry(tarLnkType, "d/../h", "d"))
		}},
	}
	byName := map[string]func(testing.TB) []byte{}
	for _, c := range cases {
		byName[c.name] = c.archive
	}
	for _, base := range []string{"plain", "global_size_bigger", "sparse_old_huge_header_size", "first_bad_checksum", "missing_padding", "hardlink_dir_chain", "symlink_fallback_order", "sparse_pax10", "empty_file", "sparse_old_beyond_int64"} {
		raw := byName[base]
		cases = append(cases,
			trdCase{name: base + "_gz", archive: func(t testing.TB) []byte { return trdGzip(t, raw(t)) }},
			trdCase{name: base + "_xz", archive: func(t testing.TB) []byte { return trdXz(t, raw(t)) }},
			trdCase{name: base + "_zst", archive: func(t testing.TB) []byte { return trdZstd(t, raw(t)) }})
	}
	plain := byName["plain"]
	for _, name := range []string{"real_gnu_S", "real_posix_00", "real_posix_01", "real_posix_10", "real_gnu_longname", "real_bsdtar_pax"} {
		cases = append(cases, trdCase{name: name, archive: func(t testing.TB) []byte { return trdGunzip(t, trdFixture(t, name)) }})
	}
	for _, name := range []string{"plain_bz2", "global_size_bigger_bz2", "first_bad_checksum_bz2", "bz2_truncated"} {
		cases = append(cases, trdCase{name: name, archive: func(t testing.TB) []byte { return trdFixture(t, name) }})
	}
	cases = append(cases,
		trdCase{name: "gz_truncated", archive: func(t testing.TB) []byte { g := trdGzip(t, plain(t)); return g[:len(g)/2] }},
		trdCase{name: "gz_header_only", archive: func(t testing.TB) []byte { return trdGzip(t, plain(t))[:5] }},
		trdCase{name: "gz_zero_padded", archive: func(t testing.TB) []byte { return append(trdGzip(t, plain(t)), make([]byte, 300)...) }},
		trdCase{name: "gz_trailing_garbage", archive: func(t testing.TB) []byte { return append(trdGzip(t, plain(t)), "garbage"...) }},
		trdCase{name: "gz_two_members", archive: func(t testing.TB) []byte {
			data := plain(t)
			return append(trdGzip(t, data[:1024]), trdGzip(t, data[1024:])...)
		}},
		trdCase{name: "gz_all_flags", archive: func(t testing.TB) []byte { return trdGzipMember(t, plain(t), true, 0) }},
		trdCase{name: "gz_bad_crc", archive: func(t testing.TB) []byte { return trdGzipMember(t, plain(t), true, 1) }},
		trdCase{name: "gz_short_content_bad_crc", archive: func(t testing.TB) []byte {
			return trdGzipMember(t, byName["no_end_marker"](t)[:tarBlockSize], false, 7)
		}},
		trdCase{name: "gz_empty_content", archive: func(t testing.TB) []byte { return trdGzip(t, nil) }},
		trdCase{name: "gz_bad_method", archive: func(t testing.TB) []byte {
			g := trdGzip(t, plain(t))
			g[2] = 7
			return g
		}},
		trdCase{name: "xz_truncated", archive: func(t testing.TB) []byte { return trdXz(t, plain(t))[:40] }},
		trdCase{name: "zst_truncated", archive: func(t testing.TB) []byte { return trdZstd(t, plain(t))[:20] }},
		trdCase{name: "bz2_garbage", archive: func(testing.TB) []byte { return append([]byte("BZh9"), make([]byte, 40)...) }},
		trdCase{name: "lzma_alone_big_truncated", archive: func(t testing.TB) []byte { a := trdLZMAAlone(t, trdBig(), false); return a[:len(a)*2/3] }},
		trdCase{name: "lzip_big_truncated", archive: func(t testing.TB) []byte { l := trdLzip(t, trdBig(), 1, 0); return l[:len(l)*2/3] }},
		trdCase{name: "gz_big_truncated", archive: func(t testing.TB) []byte { g := trdGzip(t, trdBig()); return g[:len(g)*2/3] }},
		trdCase{name: "zst_big_truncated", archive: func(t testing.TB) []byte { z := trdZstd(t, trdBig()); return z[:len(z)*2/3] }},
		trdCase{name: "big_plain", archive: func(testing.TB) []byte { return trdBig() }},
		trdCase{name: "big_gz", archive: func(t testing.TB) []byte { return trdGzip(t, trdBig()) }},
		trdCase{name: "big_xz", archive: func(t testing.TB) []byte { return trdXz(t, trdBig()) }},
		trdCase{name: "big_zst", archive: func(t testing.TB) []byte { return trdZstd(t, trdBig()) }},
		trdCase{name: "big_lzip", archive: func(t testing.TB) []byte { return trdLzip(t, trdBig(), 1, 0) }},
		trdCase{name: "zst_skippable_first", archive: func(t testing.TB) []byte {
			skip := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 0x184D2A53), 5)
			return append(append(skip, "hello"...), trdZstd(t, plain(t))...)
		}},
		trdCase{name: "zst_only_skippable", archive: func(testing.TB) []byte {
			skip := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 0x184D2A5F), 3)
			return append(skip, "abc"...)
		}},
		trdCase{name: "lzma_alone", archive: func(t testing.TB) []byte { return trdLZMAAlone(t, plain(t), false) }},
		trdCase{name: "lzma_alone_sized", archive: func(t testing.TB) []byte { return trdLZMAAlone(t, plain(t), true) }},
		trdCase{name: "lzma_alone_truncated", archive: func(t testing.TB) []byte { a := trdLZMAAlone(t, plain(t), false); return a[:len(a)*2/3] }},
		trdCase{name: "lzma_alone_trailing_garbage", archive: func(t testing.TB) []byte { return append(trdLZMAAlone(t, plain(t), false), "trailing garbage"...) }},
		trdCase{name: "lzma_alone_no_end_marker_then_more", archive: func(t testing.TB) []byte {
			data := byName["no_end_marker"](t)
			return append(trdLZMAAlone(t, data[:1024], false), trdLZMAAlone(t, data[1024:], false)...)
		}},
		trdCase{name: "lzma_alone_short_tail", archive: func(t testing.TB) []byte {
			return append(trdLZMAAlone(t, byName["no_end_marker"](t), false), 0, 0, 0, 0)
		}},
		trdCase{name: "lzip_v1", archive: func(t testing.TB) []byte { return trdLzip(t, plain(t), 1, 0) }},
		trdCase{name: "lzip_v0", archive: func(t testing.TB) []byte { return trdLzip(t, plain(t), 0, 0) }},
		trdCase{name: "lzip_two_members", archive: func(t testing.TB) []byte {
			data := plain(t)
			return append(trdLzip(t, data[:1024], 1, 0), trdLzip(t, data[1024:], 1, 0)...)
		}},
		trdCase{name: "lzip_truncated", archive: func(t testing.TB) []byte { l := trdLzip(t, plain(t), 1, 0); return l[:len(l)-10] }},
		trdCase{name: "lzip_version_two", archive: func(t testing.TB) []byte { return trdLzip(t, plain(t), 2, 0) }},
		trdCase{name: "lzip_bad_magic", archive: func(t testing.TB) []byte { l := trdLzip(t, plain(t), 1, 0); l[3] = 'Q'; return l }},
	)
	return cases
}

// Gzip member with every optional header field, a bad unchecked header CRC, and the given or real trailer CRC
func trdGzipMember(t testing.TB, data []byte, allFlags bool, crc uint32) []byte {
	head := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff}
	if allFlags {
		head[3] = 2 | 4 | 8 | 16
		head = append(head, 3, 0)
		head = append(head, "xyz"...)
		head = append(head, bytes.Repeat([]byte("n"), 2000)...)
		head = append(head, 0)
		head = append(head, "comment\x00"...)
		head = append(head, 0xde, 0xad)
	}
	var body bytes.Buffer
	w, err := flate.NewWriter(&body, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if crc == 0 {
		crc = crc32.ChecksumIEEE(data)
	}
	out := append(head, body.Bytes()...)
	out = binary.LittleEndian.AppendUint32(out, crc)
	return binary.LittleEndian.AppendUint32(out, uint32(len(data)))
}

// Archives made by GNU tar 1.35 and bsdtar stored gzipped, and CPython bz2 streams, all base64
var trdFixtures = map[string]string{
	"real_gnu_S":             "H4sIAAAAAAAC/+3Y0QqDIBTGcR/FR/CQ6YPsCQQdCC2itvdfkxVrF+2qYvH/3ShHRPhEkDN0oR/SNTdJbcaMnLVlHH2NIt5WSoy3UhtX6uIra5S+qB08hnvotVaHKSFUi0TmuYh8JlaSetfn+ZTm6gHTBgAA/sItx7jl3wQAAAAAziK1cXW9a0Jut23//uj/vtTL/q/xTpzSZo+ADu//HqvcP88EAAAAAIBTewK3eXvkACgAAA==",
	"real_posix_00":          "H4sIAAAAAAAC/+3ZwWqDQBAGYM8+hU9gZnbX1T14Tk+llz6AjStI1QTXQNqnr02akkhoEaKH+H+XBceRhfUHccLVS3Z4slluW7dyu6x1tigr690T9bRSx7U3XEkxe0yx4oj08ToLqYQXHLwZ7F2Xtf1WvGUSOlg/v4ankw9d+WlT+X0m0r+uNPv6rdpu3l3aV+LLyrYonO1Sk0hSvkiGTR+ddakiowe1nzZhDFEyeORvnzHR7bbzHqObbeQLCrKurG3K4vyW+ZKCzelabGScGKVlqBImQczSX+j5TxT5EflnjpUc5L+/X3oBIf8AAACLVJd5PuW3CQAAAAAAwKOwTf5nPbyc/+yqrGzu/y/4v/kPaTGc/3CE+c8sxgxKFLNhH5l6JBNFflz+KbrOP8U60pj/zHb+iAEAAMCifAEh4fRqACgAAA==",
	"real_posix_01":          "H4sIAAAAAAAC/+3Z0WqDMBQG4Fz7FD6AxHOSGM2Ft9uuxmDsAbKagVRdqS2UPf1sy1jrRUfBedH+300gB4xwcgT5Zfrid0/BV2Hdp/3Kr/vwUTdBTIkG1pjDOhivpGwmmHLDGdnDPiudsYh3YgbbfuPXw6uI+6Rs/Pj8Jo+dl339FUq974mOzivdtn1vPhfLvtSRprOKb0P5e3Mi406rrV+VrtBkEkPOJso5oiJxLkuOxyQUKYr9ph4ewurnRuyPWBz3cqfzwhmrpSmYFDHrSMBUZDo06/XQq4ehe1Jz4ZQtJv0UXJ5/5tzo0fxrkysRE+YfAADgLrV1VU38OwIAAAAAAHCTQlddrMvT/GfV+LqbPP75M/8hq8b5D2cK+c8crglfDLNjhC835Z9G/rr5p1H+S7nNLPKf2fqPMQAAALgr39lE2x8AKAAA",
	"real_posix_10":          "H4sIAAAAAAAC/+3Z0WqDMBQG4FznKXyCeE4So7no7barMRh7gDAzcLS2aAdlT7/UbqztoKNgvWj/70Y4QRCOvyC/yp/C5iGGOnZ93q9C18e3Zh7FmChx1g7X5PhK2mjBVFouyA1z1sYakW3EBD76dejSo4jbpHV2//iidptXi/C+7GYsj6ZNm6YkDe1P27CIs9835ui0i2HeN59xZrYbNlJTFtZNuoP1z9q3d7zuZqU3ZeWtM8pWTJqYjRQwAZWnpT0PO7tLW1SGK6+dH/VTcDr/rMsU9sP8G1takRHyf3FG+sqQlZa8k9p7okp6X8jv2BJiCAAAAFNbNHU98u8oAAAAAADAVYptffJc7fd/q3lo2tHrv3/7P3J/+j8uNPq/KZzTy1lmzygErsqFIn9e/qk4zD+VrnDo/ybbP2IAAHBzvgANdvM+ACgAAA==",
	"real_gnu_longname":      "H4sIAAAAAAAC/+3X3wqCMBiH4V3KrsB9a5s77QK8CSGLKAz6c/9Ng6Ckzqai73OiDM/k94qFKcy2urSH6tieVB6SlN731+T7KjZu3vev8xijU7pSI3jc7vVVa7VSu4kYhbW8/27UMYSf++/urURvg5T9ubMuPa4D+8+umEH/XemG/Q/0f9H9bybCh4f+s/+Z9T9+9t92PwX0n/7n6P+5qfd0f+z+/9u/iB/2P+1f2H92rAEAAAAAAAAAlukJLJqINAAoAAA=",
	"real_bsdtar_pax":        "H4sIAAAAAAAC/+2YTW7CMBCFveYUPkGZGf/FC/ZdcgWrGBUpKShNJY5fe0ErIghQyYlS5ttY0cRSPOP3xvE6HF9j2MR2uSmDEAIAnDEyj4n+CKAUSTTkyIIDchKUBtRCHsUIfH12oU2f0u733dB7t+L9xc0EVCgPoXtfbSZiuSAv37pdE1fovHKV11a96ErnPVPlYBgKNleDgrmD8vW9rf/80Ne/dUIa1n9x1j/+H8twh/8bUuf1t8qAmbn/T+WnDy6fDE3s/3EiuPFkyqf5D/6f9O9g5v4/E/3/+n8dw7ZMilM+rNYD/p/OoGf114rAsv+P4//2Wf0/b/iFgos9gNBr74ebQJraXJ86E/8vp/r79Q+ge/pHUsn/gfVfXP9j1J+Zw//foQ67j+2ujuPr35JEcBoN2BwA1NYS3/+NweX+h8mC0eOC4NT/6FQrvlf7V5RT/SP93/T0Ty6f/4H1P079WQYMwzBPxzcD8YDaACAAAA==",
	"plain_bz2":              "QlpoOTFBWSZTWRtpXL8AANzbkMmAQAD/hAAD9ETfAARAAEgwALhDKmmgaAAZA0fqhjJiaYTTEwE0wCKTUyanqTTJpptPUQNFk6a9Ou1CpxuH8GmaE4AX24xHLwlU03BQTCcmSiXBYQYTuUTmQoEprQQKhArtnMFkykAnrAWTbtONN+daWkfS0RNZC4VtIZNQpzuFhmMVTCbokIYV8339vP4nXxKkEWdLxhEUQiEeRNkQfxdyRThQkBtpXL8=",
	"global_size_bigger_bz2": "QlpoOTFBWSZTWTlca4EAAERfkMsQQAH/5kBAUAD25J5QBAAoCCAAgDakEDUJpk/SR6aaTT0JjahhoZDTJoBiGmmjQ0ZYZ8cX82qKK6/q8VyQLdO+GstJjIkTvg8uQJkkmINOagTTIFAYQpWDkJZZNEwGSPWpVvKz7i52CTtkiwg5bi03pYVFUR1khxW/FhRtxrP76884I0j5M6Z07ePGXHSWia0jQiaEKfIj2LuSKcKEgcrjXAg=",
	"first_bad_checksum_bz2": "QlpoOTFBWSZTWXQG1zwAAMpRgMAAIAAygBAAAAggAFCGBAVIjTTaqXckwDAPAaBQGSSTNAUfF3JFOFCQdAbXPA==",
	"bz2_truncated":          "QlpoOTFBWSZTWRtpXL8AANzbkMmAQAD/hAAD9ETfAARAAEgwALhDKmmgaAAZA0fqhjJiaYTTEwE0wCKT",
}

// Decoded bytes of an embedded fixture
func trdFixture(t testing.TB, name string) []byte {
	data, err := base64.StdEncoding.DecodeString(trdFixtures[name])
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Decompressed bytes of a gzip stream
func trdGunzip(t testing.TB, data []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Header block with its checksum recomputed after raw edits
func trdRechecksum(block []byte) []byte {
	out := append([]byte(nil), block...)
	copy(out[148:156], "        ")
	var sum int64
	for _, c := range out[:tarBlockSize] {
		sum += int64(c)
	}
	copy(out[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return out
}

// Every crafted, compressed and real world archive extracts from a file to the tree and error CPython produced
func TestTarReaderMatchesCPython(t *testing.T) {
	for _, c := range trdCases() {
		t.Run(c.name, func(t *testing.T) {
			want, ok := trdWant[c.name]
			if !ok {
				t.Fatalf("no CPython summary recorded for %s", c.name)
			}
			if got := trdExtract(t, c.archive(t)); got != want {
				t.Errorf("extracting %s\ngot:\n%s\nwant:\n%s", c.name, got, want)
			}
		})
	}
}

// Every archive extracts from memory, the deb payload path, to what CPython produced reading a BytesIO
func TestTarReaderInMemoryMatchesCPython(t *testing.T) {
	for _, c := range trdCases() {
		t.Run(c.name, func(t *testing.T) {
			want, ok := trdWantMem[c.name]
			if !ok {
				want = trdWant[c.name]
			}
			dest := filepath.Join(t.TempDir(), "dest")
			err := ExtractTar(bytes.NewReader(c.archive(t)), dest)
			if got := trdSummary(t, dest, err); got != want {
				t.Errorf("extracting %s from memory\ngot:\n%s\nwant:\n%s", c.name, got, want)
			}
		})
	}
}

// Every recorded CPython summary belongs to a case so none silently stops being checked
func TestTarReaderExpectationsHaveCases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range trdCases() {
		names[c.name] = true
	}
	for name := range trdWant {
		if !names[name] {
			t.Errorf("summary %s has no case", name)
		}
	}
	for name := range trdWantMem {
		if !names[name] {
			t.Errorf("in memory summary %s has no case", name)
		}
	}
}

// Int parsing gives the values and ValueError texts CPython int gives
func TestTarReaderPythonInt(t *testing.T) {
	for _, c := range trdPyInts {
		n, err := tarPyInt(c.text, c.isBytes)
		got := ""
		if err != nil {
			got = trdErrorText(err)
		} else {
			got = n.String()
		}
		if got != c.want {
			t.Errorf("int(%q bytes=%v) = %q, want %q", c.text, c.isBytes, got, c.want)
		}
	}
}

// Float parsing accepts exactly what CPython float accepts and yields the same bits
func TestTarReaderPythonFloat(t *testing.T) {
	for _, c := range trdPyFloats {
		f, ok := tarPyFloat(c.text)
		if ok != c.valid {
			t.Errorf("float(%q) valid=%v, want %v", c.text, ok, c.valid)
			continue
		}
		if !ok {
			continue
		}
		bits := math.Float64bits(f)
		if math.IsNaN(f) {
			bits = 0x7ff8000000000000
		}
		if bits != c.bits {
			t.Errorf("float(%q) bits %#x, want %#x", c.text, bits, c.bits)
		}
	}
}

// Numeric header fields read as tarfile nti reads them
func TestTarReaderNti(t *testing.T) {
	for _, c := range trdNtis {
		n, err := tarNti([]byte(c.field))
		got := ""
		var header *tarHeaderError
		var read *TarReadError
		switch {
		case errors.As(err, &header):
			got = header.class
		case errors.As(err, &read):
			got = read.Class
		case err != nil:
			got = err.Error()
		default:
			got = n.String()
		}
		if got != c.want {
			t.Errorf("nti(%q) = %s, want %s", c.field, got, c.want)
		}
	}
}

// Names and bytes render exactly as Python repr renders them
func TestTarReaderRepr(t *testing.T) {
	for _, c := range trdReprs {
		if got := tarRepr(c.raw); got != c.want {
			t.Errorf("repr(%q) = %s, want %s", c.raw, got, c.want)
		}
	}
	for _, c := range trdBytesReprs {
		if got := tarBytesRepr([]byte(c.raw)); got != c.want {
			t.Errorf("repr(%q) = %s, want %s", c.raw, got, c.want)
		}
	}
}

// Tree and error CPython 3.14 tarfile produced for each case under the hermes filter, read from a file
var trdWant = map[string]string{
	"all_zero": `error none`,
	"bad_devmajor_first": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('invalid header')`,
	"base256_fields": `'a' file mode=644 mtime=8589934592000000000 nlink=1 size=5 b'12345'
'b' file mode=755 mtime=-1000000000000 nlink=1 size=1 b'b'
error none`,
	"base256_huge_mtime": `'a' file mode=644 mtime=recent nlink=1 size=0 b''
error OverflowError: timestamp out of range for platform time_t`,
	"base256_huge_size": `'a' file mode=644 mtime=recent nlink=1 size=0 b''
error ReadError: unexpected end of data`,
	"big_gz": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=1000000026000000000 nlink=1 size=12288 sha=ed5c6d316f38d2fb
'big/27' file mode=644 mtime=1000000027000000000 nlink=1 size=12288 sha=391357284770b942
'big/28' file mode=644 mtime=1000000028000000000 nlink=1 size=12288 sha=77d9847292bad349
'big/29' file mode=644 mtime=1000000029000000000 nlink=1 size=12288 sha=7601c083a443bb73
'big/30' file mode=644 mtime=1000000030000000000 nlink=1 size=12288 sha=6d1dc8517aa02882
'big/31' file mode=644 mtime=1000000031000000000 nlink=1 size=12288 sha=5746da96ae4aad0f
'big/32' file mode=644 mtime=1000000032000000000 nlink=1 size=12288 sha=360d13053a0e42f8
'big/33' file mode=644 mtime=1000000033000000000 nlink=1 size=12288 sha=4b6611c97b848f60
'big/34' file mode=644 mtime=1000000034000000000 nlink=1 size=12288 sha=1faf09852aa3c838
'big/35' file mode=644 mtime=1000000035000000000 nlink=1 size=12288 sha=2168080c091754e1
'big/36' file mode=644 mtime=1000000036000000000 nlink=1 size=12288 sha=498bf80bdc360dae
'big/37' file mode=644 mtime=1000000037000000000 nlink=1 size=12288 sha=a8019aa905484660
'big/38' file mode=644 mtime=1000000038000000000 nlink=1 size=12288 sha=aeadd68199bd4d4e
'big/39' file mode=644 mtime=1000000039000000000 nlink=1 size=12288 sha=88d3e3d77c3d1e1e
error none`,
	"big_lzip": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=1000000026000000000 nlink=1 size=12288 sha=ed5c6d316f38d2fb
'big/27' file mode=644 mtime=1000000027000000000 nlink=1 size=12288 sha=391357284770b942
'big/28' file mode=644 mtime=1000000028000000000 nlink=1 size=12288 sha=77d9847292bad349
'big/29' file mode=644 mtime=1000000029000000000 nlink=1 size=12288 sha=7601c083a443bb73
'big/30' file mode=644 mtime=1000000030000000000 nlink=1 size=12288 sha=6d1dc8517aa02882
'big/31' file mode=644 mtime=1000000031000000000 nlink=1 size=12288 sha=5746da96ae4aad0f
'big/32' file mode=644 mtime=1000000032000000000 nlink=1 size=12288 sha=360d13053a0e42f8
'big/33' file mode=644 mtime=1000000033000000000 nlink=1 size=12288 sha=4b6611c97b848f60
'big/34' file mode=644 mtime=1000000034000000000 nlink=1 size=12288 sha=1faf09852aa3c838
'big/35' file mode=644 mtime=1000000035000000000 nlink=1 size=12288 sha=2168080c091754e1
'big/36' file mode=644 mtime=1000000036000000000 nlink=1 size=12288 sha=498bf80bdc360dae
'big/37' file mode=644 mtime=1000000037000000000 nlink=1 size=12288 sha=a8019aa905484660
'big/38' file mode=644 mtime=1000000038000000000 nlink=1 size=12288 sha=aeadd68199bd4d4e
'big/39' file mode=644 mtime=1000000039000000000 nlink=1 size=12288 sha=88d3e3d77c3d1e1e
error none`,
	"big_plain": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=1000000026000000000 nlink=1 size=12288 sha=ed5c6d316f38d2fb
'big/27' file mode=644 mtime=1000000027000000000 nlink=1 size=12288 sha=391357284770b942
'big/28' file mode=644 mtime=1000000028000000000 nlink=1 size=12288 sha=77d9847292bad349
'big/29' file mode=644 mtime=1000000029000000000 nlink=1 size=12288 sha=7601c083a443bb73
'big/30' file mode=644 mtime=1000000030000000000 nlink=1 size=12288 sha=6d1dc8517aa02882
'big/31' file mode=644 mtime=1000000031000000000 nlink=1 size=12288 sha=5746da96ae4aad0f
'big/32' file mode=644 mtime=1000000032000000000 nlink=1 size=12288 sha=360d13053a0e42f8
'big/33' file mode=644 mtime=1000000033000000000 nlink=1 size=12288 sha=4b6611c97b848f60
'big/34' file mode=644 mtime=1000000034000000000 nlink=1 size=12288 sha=1faf09852aa3c838
'big/35' file mode=644 mtime=1000000035000000000 nlink=1 size=12288 sha=2168080c091754e1
'big/36' file mode=644 mtime=1000000036000000000 nlink=1 size=12288 sha=498bf80bdc360dae
'big/37' file mode=644 mtime=1000000037000000000 nlink=1 size=12288 sha=a8019aa905484660
'big/38' file mode=644 mtime=1000000038000000000 nlink=1 size=12288 sha=aeadd68199bd4d4e
'big/39' file mode=644 mtime=1000000039000000000 nlink=1 size=12288 sha=88d3e3d77c3d1e1e
error none`,
	"big_xz": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=1000000026000000000 nlink=1 size=12288 sha=ed5c6d316f38d2fb
'big/27' file mode=644 mtime=1000000027000000000 nlink=1 size=12288 sha=391357284770b942
'big/28' file mode=644 mtime=1000000028000000000 nlink=1 size=12288 sha=77d9847292bad349
'big/29' file mode=644 mtime=1000000029000000000 nlink=1 size=12288 sha=7601c083a443bb73
'big/30' file mode=644 mtime=1000000030000000000 nlink=1 size=12288 sha=6d1dc8517aa02882
'big/31' file mode=644 mtime=1000000031000000000 nlink=1 size=12288 sha=5746da96ae4aad0f
'big/32' file mode=644 mtime=1000000032000000000 nlink=1 size=12288 sha=360d13053a0e42f8
'big/33' file mode=644 mtime=1000000033000000000 nlink=1 size=12288 sha=4b6611c97b848f60
'big/34' file mode=644 mtime=1000000034000000000 nlink=1 size=12288 sha=1faf09852aa3c838
'big/35' file mode=644 mtime=1000000035000000000 nlink=1 size=12288 sha=2168080c091754e1
'big/36' file mode=644 mtime=1000000036000000000 nlink=1 size=12288 sha=498bf80bdc360dae
'big/37' file mode=644 mtime=1000000037000000000 nlink=1 size=12288 sha=a8019aa905484660
'big/38' file mode=644 mtime=1000000038000000000 nlink=1 size=12288 sha=aeadd68199bd4d4e
'big/39' file mode=644 mtime=1000000039000000000 nlink=1 size=12288 sha=88d3e3d77c3d1e1e
error none`,
	"big_zst": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=1000000026000000000 nlink=1 size=12288 sha=ed5c6d316f38d2fb
'big/27' file mode=644 mtime=1000000027000000000 nlink=1 size=12288 sha=391357284770b942
'big/28' file mode=644 mtime=1000000028000000000 nlink=1 size=12288 sha=77d9847292bad349
'big/29' file mode=644 mtime=1000000029000000000 nlink=1 size=12288 sha=7601c083a443bb73
'big/30' file mode=644 mtime=1000000030000000000 nlink=1 size=12288 sha=6d1dc8517aa02882
'big/31' file mode=644 mtime=1000000031000000000 nlink=1 size=12288 sha=5746da96ae4aad0f
'big/32' file mode=644 mtime=1000000032000000000 nlink=1 size=12288 sha=360d13053a0e42f8
'big/33' file mode=644 mtime=1000000033000000000 nlink=1 size=12288 sha=4b6611c97b848f60
'big/34' file mode=644 mtime=1000000034000000000 nlink=1 size=12288 sha=1faf09852aa3c838
'big/35' file mode=644 mtime=1000000035000000000 nlink=1 size=12288 sha=2168080c091754e1
'big/36' file mode=644 mtime=1000000036000000000 nlink=1 size=12288 sha=498bf80bdc360dae
'big/37' file mode=644 mtime=1000000037000000000 nlink=1 size=12288 sha=a8019aa905484660
'big/38' file mode=644 mtime=1000000038000000000 nlink=1 size=12288 sha=aeadd68199bd4d4e
'big/39' file mode=644 mtime=1000000039000000000 nlink=1 size=12288 sha=88d3e3d77c3d1e1e
error none`,
	"bz2_garbage": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"bz2_truncated": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"chksum_base256": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'c'
error none`,
	"contiguous_type": `'c' file mode=644 mtime=1000000000000000000 nlink=1 size=4 b'cont'
error none`,
	"dir_sorting": "'z\\udcff' dir mode=default mtime=1000000002000000000\n'z\\udcff/f' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'f'\n'z\u00e9' dir mode=default mtime=1000000001000000000\n'z\u00e9/in' dir mode=default mtime=1000000003000000000\nerror none",
	"dir_with_size": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'd' dir mode=default mtime=1000000000000000000
'hidden' file mode=644 mtime=1000000000000000000 nlink=1 size=11 b'in dir data'
error none`,
	"dot_names": `'f' file mode=644 mtime=1000000000000000000 nlink=1 size=3 b'dot'
error none`,
	"empty_file": `error ReadError: file could not be opened successfully:
- method gz: ReadError('empty file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('empty file')`,
	"empty_file_gz": `error ReadError: file could not be opened successfully:
- method gz: ReadError('empty file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"empty_file_xz": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('empty file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"empty_file_zst": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('empty file')
- method tar: ReadError('truncated header')`,
	"filter_absolute_symlink": `error AbsoluteLinkError: 's\udcff' is a link to an absolute path`,
	"filter_hardlink_outside": `error LinkOutsideDestinationError: 'h' would link to '<parent>/outside', which is outside the destination`,
	"filter_outside_nonutf8": `'ok' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error OutsideDestinationError: '../caf\udce9' would be extracted to '<parent>/caf\udce9', which is outside the destination`,
	"filter_special": `'ok' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error SpecialFileError: 'fifo\x01' is a special file`,
	"filter_symlink_escape": `'d' dir mode=default mtime=recent
error LinkOutsideDestinationError: 'd/up' would link to '<parent>/x\udcfe', which is outside the destination`,
	"first_bad_checksum": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('invalid header')`,
	"first_bad_checksum_bz2": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('invalid header')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"first_bad_checksum_gz": `error ReadError: file could not be opened successfully:
- method gz: ReadError('invalid header')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"first_bad_checksum_xz": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('invalid header')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"first_bad_checksum_zst": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('invalid header')
- method tar: ReadError('truncated header')`,
	"first_invalid_number": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('invalid header')`,
	"global_linkpath_symlink": `'f' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'f'
's' link -> 't'
't' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b't'
error none`,
	"global_mtime_float": `'a' file mode=644 mtime=1500000000987654209 nlink=1 size=1 b'x'
'd' dir mode=default mtime=1500000000987654209
error none`,
	"global_path": `'renamed' file mode=644 mtime=1000000000000000000 nlink=1 size=6 b'second'
error none`,
	"global_size_beyond_end": `'a' file mode=644 mtime=recent nlink=1 size=0 b''
error ReadError: unexpected end of data`,
	"global_size_bigger": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=1701b188c9330ac8
'b' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=d8171ac3e23043ef
error none`,
	"global_size_bigger_bz2": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=1701b188c9330ac8
'b' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=d8171ac3e23043ef
error none`,
	"global_size_bigger_gz": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=1701b188c9330ac8
'b' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=d8171ac3e23043ef
error none`,
	"global_size_bigger_xz": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=1701b188c9330ac8
'b' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=d8171ac3e23043ef
error none`,
	"global_size_bigger_zst": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=1701b188c9330ac8
'b' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=d8171ac3e23043ef
error none`,
	"global_size_negative": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=16381 sha=d2c3bc21e8188b20
error none`,
	"global_size_smaller": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=3 b'012'
error none`,
	"global_sparse_size_invalid": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: invalid literal for int() with base 10: 'abc'`,
	"global_then_eof": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ReadError: empty header`,
	"global_then_local": `'g' file mode=644 mtime=1600000000000000000 nlink=1 size=1 b'y'
'local' file mode=644 mtime=1600000000000000000 nlink=1 size=1 b'x'
error none`,
	"gnu_header_prefix_area": `'07346545000' dir mode=default mtime=recent
'07346545000/gnufile' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'g'
error none`,
	"gnu_longname_first_then_garbage": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('invalid header')`,
	"gnu_longname_longlink": `'dd' dir mode=default mtime=1000000000000000000
'nnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnn' dir mode=default mtime=recent
'nnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnn/mmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmm' file mode=644 mtime=1000000000000000000 nlink=1 size=4 b'long'
'sym' link -> 'nnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnn/mmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmm'
error none`,
	"gnu_longname_regular_slash": `'x' file mode=644 mtime=1000000000000000000 nlink=1 size=3 b'abc'
error none`,
	"gnu_longname_then_eof": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ReadError: empty header`,
	"gz_all_flags": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"gz_bad_crc": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"gz_bad_method": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"gz_big_truncated": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=recent nlink=1 size=0 b''
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"gz_empty_content": `error ReadError: file could not be opened successfully:
- method gz: ReadError('empty file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"gz_header_only": `error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"gz_short_content_bad_crc": `'a' file mode=644 mtime=recent nlink=1 size=0 b''
error BadGzipFile: CRC check failed 0x7 != 0xcbabcb0`,
	"gz_trailing_garbage": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"gz_truncated": `'a' file mode=755 mtime=1100000000000000000 nlink=1 size=5 b'alpha'
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"gz_two_members": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"gz_zero_padded": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"hardlink_dir_chain": `'d' dir mode=default mtime=1000000000000000000
'g' dir mode=640 mtime=1000000000000000000
'h' dir mode=750 mtime=1000000000000000000
error none`,
	"hardlink_dir_chain_gz": `'d' dir mode=default mtime=1000000000000000000
'g' dir mode=640 mtime=1000000000000000000
'h' dir mode=750 mtime=1000000000000000000
error none`,
	"hardlink_dir_chain_xz": `'d' dir mode=default mtime=1000000000000000000
'g' dir mode=640 mtime=1000000000000000000
'h' dir mode=750 mtime=1000000000000000000
error none`,
	"hardlink_dir_chain_zst": `'d' dir mode=default mtime=1000000000000000000
'g' dir mode=640 mtime=1000000000000000000
'h' dir mode=750 mtime=1000000000000000000
error none`,
	"hardlink_missing": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'x'
error KeyError: "linkname 'nowhere' not found"`,
	"invalid_after_first": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'alpha'
error none`,
	"link_fallback_rejected": `'d' dir mode=default mtime=1000000000000000000
'h' dir mode=default mtime=1000000000000000000
error none`,
	"longname_and_pax_path": `'gnu2' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'both2'
'paxname' file mode=644 mtime=1000000000000000000 nlink=1 size=4 b'both'
error none`,
	"longname_huge_size": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ReadError: empty header`,
	"lzip_bad_magic": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"lzip_big_truncated": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=recent nlink=1 size=0 b''
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"lzip_truncated": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzip_two_members": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzip_v0": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzip_v1": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzip_version_two": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"lzma_alone": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzma_alone_big_truncated": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=1000000020000000000 nlink=1 size=12288 sha=d89c7d771e79fbe3
'big/21' file mode=644 mtime=1000000021000000000 nlink=1 size=12288 sha=f6a6f59688fe0c7e
'big/22' file mode=644 mtime=1000000022000000000 nlink=1 size=12288 sha=f99098fa0e91c230
'big/23' file mode=644 mtime=1000000023000000000 nlink=1 size=12288 sha=3173da9e84569c52
'big/24' file mode=644 mtime=1000000024000000000 nlink=1 size=12288 sha=d96d89dd6a192d07
'big/25' file mode=644 mtime=1000000025000000000 nlink=1 size=12288 sha=e9f4a66b1e55d1da
'big/26' file mode=644 mtime=recent nlink=1 size=0 b''
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"lzma_alone_no_end_marker_then_more": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'alpha'
error none`,
	"lzma_alone_short_tail": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'alpha'
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"lzma_alone_sized": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzma_alone_trailing_garbage": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"lzma_alone_truncated": `'a' file mode=755 mtime=1100000000000000000 nlink=1 size=5 b'alpha'
'd' dir mode=default mtime=recent
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"missing_padding": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=10 b'0123456789'
error ReadError: unexpected end of data`,
	"missing_padding_gz": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=10 b'0123456789'
error ReadError: unexpected end of data`,
	"missing_padding_xz": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=10 b'0123456789'
error ReadError: unexpected end of data`,
	"missing_padding_zst": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=10 b'0123456789'
error ReadError: unexpected end of data`,
	"negative_size_first": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('invalid offset')`,
	"negative_size_later": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"no_end_marker": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'alpha'
error none`,
	"non_ascii_number_later": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"nonutf8_names": "'b\\udcfe\u00e9' file mode=644 mtime=1000000000000000000 nlink=1 size=6 b'binary'\n'caf\\udce9' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'latin'\n'n\\udcffx' file mode=644 mtime=1000000000000000000 nlink=1 size=3 b'raw'\n'u\u00e9' file mode=644 mtime=1000000000000000000 nlink=1 size=4 b'utf8'\nerror none",
	"octal_variants": `'a' file mode=755 mtime=1073741823000000000 nlink=1 size=2 b'aa'
'b' file mode=644 mtime=342391000000000 nlink=1 size=1 b'b'
error none`,
	"pax_duplicate_keys": `'two=x' file mode=644 mtime=5000000000 nlink=1 size=3 b'dup'
error none`,
	"pax_invalid_record_first": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('invalid header')`,
	"pax_invalid_record_later": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"pax_linkpath": `'sym' link -> 'target'
'target' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b't'
error none`,
	"pax_linkpath_nul_hardlink": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: lstat: embedded null character in path`,
	"pax_linkpath_nul_symlink": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: lstat: embedded null character in path`,
	"pax_mtime_dir_huge": `'d' dir mode=default mtime=recent
'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error OverflowError: timestamp out of range for platform time_t`,
	"pax_mtime_inf": `'a' file mode=644 mtime=recent nlink=1 size=1 b'x'
error OverflowError: timestamp out of range for platform time_t`,
	"pax_mtime_nan": `'a' file mode=644 mtime=recent nlink=1 size=1 b'x'
'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: Invalid value NaN (not a number)`,
	"pax_mtime_values": `'m0' file mode=644 mtime=1500000000 nlink=1 size=1 b't'
'm1' file mode=644 mtime=-1250000000 nlink=1 size=1 b't'
'm2' file mode=644 mtime=1700000000123456716 nlink=1 size=1 b't'
'm3' file mode=644 mtime=0 nlink=1 size=1 b't'
'm4' file mode=644 mtime=1234567890000000000 nlink=1 size=1 b't'
'm5' file mode=644 mtime=1000000000250000000 nlink=1 size=1 b't'
'm6' file mode=644 mtime=42000000000 nlink=1 size=1 b't'
'm7' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b't'
'm8' file mode=644 mtime=0 nlink=1 size=1 b't'
error none`,
	"pax_path_absolute": `'abs' dir mode=default mtime=recent
'abs/file' file mode=644 mtime=1000000000000000000 nlink=1 size=3 b'abs'
error none`,
	"pax_path_and_sparse_name_order": `'pathname' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
'second' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'2'
error none`,
	"pax_path_empty": `error OSError[21]`,
	"pax_path_nul": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: lstat: embedded null character in path`,
	"pax_path_trailing": `'dir' dir mode=default mtime=1000000000000000000
'dir/f' file mode=644 mtime=1000000000000000000 nlink=1 size=2 b'in'
error none`,
	"pax_record_edge_lengths": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"pax_size_before_longname": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'longnamed' file mode=644 mtime=1000000000000000000 nlink=1 size=3 b'abc'
'short' file mode=644 mtime=1000000000000000000 nlink=1 size=8 b'abcdefgh'
error none`,
	"pax_size_digits_limit": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=0 b''
error none`,
	"pax_size_invalid": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=0 b''
error none`,
	"pax_size_negative_first": `error none`,
	"pax_size_negative_later": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"pax_then_garbage": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ReadError: invalid header`,
	"pax_truncated_payload": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ReadError: empty header`,
	"plain": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"plain_bz2": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"plain_gz": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"plain_xz": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"plain_zst": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"real_bsdtar_pax": `'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd' dir mode=default mtime=recent
'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee' dir mode=default mtime=recent
'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee/leaf' file mode=644 mtime=recent nlink=1 size=4 b'leaf'
'plainfile' file mode=644 mtime=1200000000000000000 nlink=1 size=5 b'plain'
error none`,
	"real_gnu_S": `'plainfile' file mode=644 mtime=1200000000000000000 nlink=1 size=5 b'plain'
'sparsefile' file mode=644 mtime=1200000000000000000 nlink=1 size=300003 sha=9120ffb0598e47b2
error none`,
	"real_gnu_longname": `'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd' dir mode=default mtime=1200000000000000000
'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee' dir mode=default mtime=1200000000000000000
'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee/leaf' file mode=644 mtime=1200000000000000000 nlink=1 size=4 b'leaf'
error none`,
	"real_posix_00": `'plainfile' file mode=644 mtime=1200000000000000000 nlink=1 size=5 b'plain'
'sparsefile' file mode=644 mtime=1200000000000000000 nlink=1 size=300003 sha=9120ffb0598e47b2
error none`,
	"real_posix_01": `'plainfile' file mode=644 mtime=1200000000000000000 nlink=1 size=5 b'plain'
'sparsefile' file mode=644 mtime=1200000000000000000 nlink=1 size=300003 sha=9120ffb0598e47b2
error none`,
	"real_posix_10": `'plainfile' file mode=644 mtime=1200000000000000000 nlink=1 size=5 b'plain'
'sparsefile' file mode=644 mtime=1200000000000000000 nlink=1 size=300003 sha=9120ffb0598e47b2
error none`,
	"regtype_trailing_slash": `'v' dir mode=default mtime=1000000000000000000
'x' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'slash'
error none`,
	"signed_checksum": "'hi\u00e9\\udcff' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b's'\nerror none",
	"solaris_x": `'solaris' dir mode=default mtime=recent
'solaris/long' dir mode=default mtime=recent
'solaris/long/name.txt' file mode=644 mtime=1234567890500000000 nlink=1 size=7 b'solaris'
error none`,
	"solaris_x_middle": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
'sol' file mode=644 mtime=1000000000000000000 nlink=1 size=2 b'ab'
error none`,
	"solaris_x_size": `'big' file mode=644 mtime=1000000000000000000 nlink=1 size=700 sha=c14c6b285c51dc04
error none`,
	"sparse_after_pax_size": `'renamed' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'a\x00\x00\x00\x00'
error none`,
	"sparse_old": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=6000 sha=37a9c22d5b384961
error none`,
	"sparse_old_bad_extension_number": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"sparse_old_beyond_int64": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ValueError: cannot fit 'int' into an offset-sized integer`,
	"sparse_old_beyond_int64_gz": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ValueError: cannot fit 'int' into an offset-sized integer`,
	"sparse_old_beyond_int64_xz": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ValueError: cannot fit 'int' into an offset-sized integer`,
	"sparse_old_beyond_int64_zst": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ValueError: cannot fit 'int' into an offset-sized integer`,
	"sparse_old_huge_header_size": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ReadError: unexpected end of data`,
	"sparse_old_huge_header_size_gz": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ReadError: unexpected end of data`,
	"sparse_old_huge_header_size_xz": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ReadError: unexpected end of data`,
	"sparse_old_huge_header_size_zst": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error ReadError: unexpected end of data`,
	"sparse_old_many_runs": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'many' file mode=644 mtime=1000000000000000000 nlink=1 size=3100 sha=22a93b35e97be927
error none`,
	"sparse_old_negative_realsize": `'sparse' file mode=644 mtime=recent nlink=1 size=5 b'hello'
error OSError[22]`,
	"sparse_old_truncated_extension": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error IndexError: index out of range`,
	"sparse_old_zero_runs": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=20 b'\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00xyz\x00\x00\x00\x00\x00\x00\x00'
error none`,
	"sparse_pax00": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse00' file mode=644 mtime=1000000000000000000 nlink=1 size=3000 sha=8468b015cdd1c007
error none`,
	"sparse_pax00_bad_offset": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error none`,
	"sparse_pax01": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse01' file mode=644 mtime=1000000000000000000 nlink=1 size=5000 sha=200ef05537dc94e7
error none`,
	"sparse_pax01_bad_map": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: invalid literal for int() with base 10: 'x'`,
	"sparse_pax10": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse10' file mode=644 mtime=1000000000000000000 nlink=1 size=9000 sha=22c2dffa279f3fa4
error none`,
	"sparse_pax10_bad_count": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: invalid literal for int() with base 10: b''`,
	"sparse_pax10_gz": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse10' file mode=644 mtime=1000000000000000000 nlink=1 size=9000 sha=22c2dffa279f3fa4
error none`,
	"sparse_pax10_long_map": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'longmap' file mode=644 mtime=1000000000000000000 nlink=1 size=120000 sha=8b5ebb85d1f99ee6
error none`,
	"sparse_pax10_no_newline": `'first' file mode=644 mtime=1000000000000000000 nlink=1 size=1 b'1'
error ValueError: not enough values to unpack (expected 2, got 1)`,
	"sparse_pax10_xz": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse10' file mode=644 mtime=1000000000000000000 nlink=1 size=9000 sha=22c2dffa279f3fa4
error none`,
	"sparse_pax10_zst": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'sparse10' file mode=644 mtime=1000000000000000000 nlink=1 size=9000 sha=22c2dffa279f3fa4
error none`,
	"symlink_fallback_order": `'a' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h2' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
error none`,
	"symlink_fallback_order_gz": `'a' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h2' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
error none`,
	"symlink_fallback_order_xz": `'a' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h2' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
error none`,
	"symlink_fallback_order_zst": `'a' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
'h2' file mode=755 mtime=1000000000000000000 nlink=3 size=6 b'second'
error none`,
	"symlink_with_size": `'after' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'after'
'hidden' file mode=644 mtime=1000000000000000000 nlink=1 size=12 b'in link data'
's' link -> 'after'
error none`,
	"tiny_file": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"truncated_after_first": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'alpha'
error none`,
	"truncated_data": `'a' file mode=644 mtime=recent nlink=1 size=0 b''
error ReadError: unexpected end of data`,
	"unknown_type": `error SpecialFileError: 'vol' is a special file`,
	"ustar_prefix": `'pre' dir mode=default mtime=recent
'pre/fix' dir mode=default mtime=recent
'pre/fix/file.txt' file mode=644 mtime=1000000000000000000 nlink=1 size=2 b'pf'
'pre/sub' dir mode=default mtime=1000000000000000000
error none`,
	"v7_archive": `'dir' dir mode=default mtime=1000000000000000000
'dir/file' file mode=644 mtime=1000000000000000000 nlink=1 size=2 b'v7'
error none`,
	"xz_truncated": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
	"zero_block_middle": `'a' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'alpha'
error none`,
	"zst_big_truncated": `'big' dir mode=default mtime=recent
'big/00' file mode=644 mtime=1000000000000000000 nlink=1 size=12288 sha=509e351ab17e2fdf
'big/01' file mode=644 mtime=1000000001000000000 nlink=1 size=12288 sha=eecdd31bdc75302d
'big/02' file mode=644 mtime=1000000002000000000 nlink=1 size=12288 sha=d1cb6ed4068236fe
'big/03' file mode=644 mtime=1000000003000000000 nlink=1 size=12288 sha=3649d176532442c7
'big/04' file mode=644 mtime=1000000004000000000 nlink=1 size=12288 sha=f021503ce8e93c58
'big/05' file mode=644 mtime=1000000005000000000 nlink=1 size=12288 sha=3f25564d80241875
'big/06' file mode=644 mtime=1000000006000000000 nlink=1 size=12288 sha=23cf7b057376c6a9
'big/07' file mode=644 mtime=1000000007000000000 nlink=1 size=12288 sha=0aef08fbe45be2fb
'big/08' file mode=644 mtime=1000000008000000000 nlink=1 size=12288 sha=fdf7fd5fa597a3b0
'big/09' file mode=644 mtime=1000000009000000000 nlink=1 size=12288 sha=ac28e98445b44197
'big/10' file mode=644 mtime=1000000010000000000 nlink=1 size=12288 sha=054cb564790ba400
'big/11' file mode=644 mtime=1000000011000000000 nlink=1 size=12288 sha=1e8ead7e08e055bc
'big/12' file mode=644 mtime=1000000012000000000 nlink=1 size=12288 sha=496409d1e5fea100
'big/13' file mode=644 mtime=1000000013000000000 nlink=1 size=12288 sha=84e3b7e7f07ebdda
'big/14' file mode=644 mtime=1000000014000000000 nlink=1 size=12288 sha=65743708dfbca27e
'big/15' file mode=644 mtime=1000000015000000000 nlink=1 size=12288 sha=13768a0a698651ff
'big/16' file mode=644 mtime=1000000016000000000 nlink=1 size=12288 sha=3a1005e93bbb82dd
'big/17' file mode=644 mtime=1000000017000000000 nlink=1 size=12288 sha=17a95020a2d17768
'big/18' file mode=644 mtime=1000000018000000000 nlink=1 size=12288 sha=4db50a33caa4c3c5
'big/19' file mode=644 mtime=1000000019000000000 nlink=1 size=12288 sha=403746ed19a8eda0
'big/20' file mode=644 mtime=recent nlink=1 size=0 b''
error EOFError: Compressed file ended before the end-of-stream marker was reached`,
	"zst_only_skippable": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('empty file')
- method tar: ReadError('truncated header')`,
	"zst_skippable_first": `'a' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'd' dir mode=default mtime=1000000000000000000
'd/b' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'bravo'
'h' file mode=755 mtime=1000000000000000000 nlink=2 size=5 b'alpha'
'l' link -> 'a'
error none`,
	"zst_truncated": `error ReadError: file could not be opened successfully:
- method gz: ReadError('not a gzip file')
- method bz2: ReadError('not a bzip2 file')
- method xz: ReadError('not an lzma file')
- method zst: ReadError('not a zstd file')
- method tar: ReadError('truncated header')`,
}

// Cases whose in memory extraction CPython reported differently, read from a BytesIO
var trdWantMem = map[string]string{
	"sparse_old_beyond_int64": `'sparse' file mode=644 mtime=1000000000000000000 nlink=1 size=5 b'hello'
error OverflowError: Python int too large to convert to C ssize_t`,
}

// Values and ValueError messages CPython int gives str and bytes text in base 10
var trdPyInts = []struct {
	text    string
	isBytes bool
	want    string
}{
	{"\x1c5", false, "ValueError: invalid literal for int() with base 10: '\\x1c5'"},
	{"5\x1c", false, "ValueError: invalid literal for int() with base 10: '5\\x1c'"},
	{" 5 ", false, "5"},
	{"\xc2\xa05", false, "5"},
	{"\xd9\xa3", false, "3"},
	{"1_0", false, "10"},
	{"_1", false, "ValueError: invalid literal for int() with base 10: '_1'"},
	{"1_", false, "ValueError: invalid literal for int() with base 10: '1_'"},
	{"1__0", false, "ValueError: invalid literal for int() with base 10: '1__0'"},
	{"+5", false, "5"},
	{"- 5", false, "ValueError: invalid literal for int() with base 10: '- 5'"},
	{"-5", false, "-5"},
	{"0x10", false, "ValueError: invalid literal for int() with base 10: '0x10'"},
	{"010", false, "10"},
	{"\x7f", false, "ValueError: invalid literal for int() with base 10: '\\x7f'"},
	{"5\x00", false, "ValueError: invalid literal for int() with base 10: '5\\x00'"},
	{"", false, "ValueError: invalid literal for int() with base 10: ''"},
	{"\xe2\x80\x805", false, "5"},
	{"\xef\xbc\x95", false, "5"},
	{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", false, "ValueError: invalid literal for int() with base 10: 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	{"11111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111", false, "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 5000 digits; use sys.set_int_max_str_digits() to increase the limit"},
	{"11111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111x", false, "ValueError: invalid literal for int() with base 10: '1111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111"},
	{"\xff", false, "ValueError: invalid literal for int() with base 10: '\\udcff'"},
	{"123456789012345678901234567890", false, "123456789012345678901234567890"},
	{"\xd9\xa1\xd9\xa2\xd9\xa3", false, "123"},
	{"\x1c5", true, "ValueError: invalid literal for int() with base 10: b'\\x1c5'"},
	{" 5\x0a", true, "5"},
	{"1_0", true, "10"},
	{"\xa05", true, "ValueError: invalid literal for int() with base 10: b'\\xa05'"},
	{"", true, "ValueError: invalid literal for int() with base 10: b''"},
	{"5\x00", true, "ValueError: invalid literal for int() with base 10: b'5\\x00'"},
	{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", true, "ValueError: invalid literal for int() with base 10: b'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	{"-7", true, "-7"},
	{"0o7", true, "ValueError: invalid literal for int() with base 10: b'0o7'"},
}

// Bits of the float CPython parses from each text, invalid when it raises ValueError
var trdPyFloats = []struct {
	text  string
	valid bool
	bits  uint64
}{
	{"1.5", true, 0x3ff8000000000000},
	{" 1_0.5 ", true, 0x4025000000000000},
	{"1_.5", false, 0},
	{"inf", true, 0x7ff0000000000000},
	{"-Infinity", true, 0xfff0000000000000},
	{"nan", true, 0x7ff8000000000000},
	{"1e5", true, 0x40f86a0000000000},
	{"1e", false, 0},
	{".5", true, 0x3fe0000000000000},
	{"5.", true, 0x4014000000000000},
	{".", false, 0},
	{"0x10", false, 0},
	{"\xd9\xa3.5", true, 0x400c000000000000},
	{"1e500", true, 0x7ff0000000000000},
	{"\x1c1", false, 0},
	{"1\x1c", false, 0},
	{"1_0e1_0", true, 0x42374876e8000000},
	{"infinit", false, 0},
	{"+nan", true, 0x7ff8000000000000},
	{"1.5\x00", false, 0},
	{"1700000000.123456789", true, 0x41d954fc4007e6b7},
	{"-0.0", true, 0x8000000000000000},
	{"++1", false, 0},
	{"1e+5", true, 0x40f86a0000000000},
	{"1e+-5", false, 0},
	{"\xe3\x80\x80 2.5\xe2\x80\x83", true, 0x4004000000000000},
	{"1__0", false, 0},
	{"_1", false, 0},
	{"1_", false, 0},
}

// Values tarfile nti reads from raw fields, or the exception class it raises
var trdNtis = []struct {
	field string
	want  string
}{
	{"0000644\x00", "420"},
	{" 755 \x00\x00\x00", "493"},
	{"0o17\x00", "15"},
	{"6_4_4\x00", "420"},
	{"\x00\x00\x00\x00", "0"},
	{"", "IndexError"},
	{"\x1c 12 \x00", "10"},
	{"0x10\x00", "InvalidHeaderError"},
	{"\xb5\x00", "InvalidHeaderError"},
	{"8\x00", "InvalidHeaderError"},
	{"0o\x00", "InvalidHeaderError"},
	{"-7\x00", "-7"},
	{"7\x7f", "InvalidHeaderError"},
	{"\x80\x00\x00\x00\x00\x00\x00\x05", "5"},
	{"\xff\xff\xff\xff\xff\xff\xff\xfb", "-5"},
	{"\x80\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f", "154135671558081661678616447"},
	{"\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00", "-309485009821345068724781056"},
	{"12 34\x00", "InvalidHeaderError"},
	{"111111111111", "9817068105"},
}

// Python repr of str values whose raw bytes surrogateescape decoded
var trdReprs = []struct {
	raw  string
	want string
}{
	{"plain", "'plain'"},
	{"it's", "\"it's\""},
	{"it's \"q\"", "'it\\'s \"q\"'"},
	{"a\xffb\xc2\x85\xc2\xa0\xe2\x80\xa8\xf0\x9f\x98\x80'", "\"a\\udcffb\\x85\\xa0\\u2028\U0001f600'\""},
	{"tab\x09nl\x0acr\x0d\\", "'tab\\tnl\\ncr\\r\\\\'"},
	{"\x00\x1f\x7f", "'\\x00\\x1f\\x7f'"},
	{"caf\xc3\xa9", "'caf\u00e9'"},
	{"\xe2\x80\x8b\xef\xbb\xbf\xf3\xa0\x80\x81", "'\\u200b\\ufeff\\U000e0001'"},
	{"\xee\x80\x80", "'\\ue000'"},
}

// Python repr of bytes values
var trdBytesReprs = []struct {
	raw  string
	want string
}{
	{"plain", "b'plain'"},
	{"it's", "b\"it's\""},
	{"it's \"q\"", "b'it\\'s \"q\"'"},
	{"\x00\xff\x7f\x09\x0a\x0d\\", "b'\\x00\\xff\\x7f\\t\\n\\r\\\\'"},
	{"\xc3\xa9", "b'\\xc3\\xa9'"},
}
