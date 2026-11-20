package pyzip

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLevel(n int) *int { return &n }

// Reads every member of a finished archive, failing on a bad CRC like testzip
func readBack(t *testing.T, path string) map[string]string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = string(b)
	}
	return out
}

func TestWriteDeflatesFilesWithModeDateAndUnicodeFlag(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, bytes.Repeat([]byte("hello "), 5000), 0o640); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 5, 6, 7, 8, 9, 0, time.Local)
	os.Chtimes(src, when, when)
	uni := filepath.Join(dir, "u.txt")
	os.WriteFile(uni, []byte("u"), 0o600)
	out := filepath.Join(dir, "out.zip")
	zf, err := Create(out, Deflated, writeLevel(6))
	if err != nil {
		t.Fatal(err)
	}
	if err := zf.Write(src, "sub/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := zf.Write(uni, "café/中.txt"); err != nil {
		t.Fatal(err)
	}
	if err := zf.Write(dir, "tree"); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	got := readBack(t, out)
	if got["sub/a.txt"] != string(bytes.Repeat([]byte("hello "), 5000)) || got["café/中.txt"] != "u" {
		t.Fatalf("members %v", len(got))
	}
	if _, ok := got["tree/"]; !ok {
		t.Fatalf("directory member missing: %v", got)
	}
	r, _ := zip.OpenReader(out)
	defer r.Close()
	a := r.File[0]
	if a.Method != zip.Deflate || a.Modified.Year() != 2024 || a.Modified.Second() != 8 || a.ExternalAttrs>>16 != 0o100640 {
		t.Fatalf("header %v %v %o", a.Method, a.Modified, a.ExternalAttrs>>16)
	}
	if a.Flags&utfFilenameFlag != 0 || r.File[1].Flags&utfFilenameFlag == 0 {
		t.Fatalf("flags %x %x", a.Flags, r.File[1].Flags)
	}
	if r.File[2].ExternalAttrs&0x10 == 0 || r.File[2].Method != Stored {
		t.Fatalf("dir entry %x", r.File[2].ExternalAttrs)
	}
}

func TestDirectoryNamesTryCP437First(t *testing.T) {
	z := &ZipInfo{Filename: "café/"}
	name, flags, err := z.encodeFilenameFlags()
	if err != nil || !bytes.Equal(name, []byte{'c', 'a', 'f', 0x82, '/'}) || flags != 0 {
		t.Fatalf("%q %x %v", name, flags, err)
	}
	z = &ZipInfo{Filename: "中/"}
	if name, flags, err := z.encodeFilenameFlags(); err != nil || string(name) != "中/" || flags != utfFilenameFlag {
		t.Fatalf("%q %x %v", name, flags, err)
	}
}

func TestUndecodableNamesRaiseTheUTF8EncodeError(t *testing.T) {
	z := &ZipInfo{Filename: "a\xff\xfeb", FlagBits: utfFilenameFlag}
	_, _, err := z.encodeFilenameFlags()
	if err == nil || err.Error() != "'utf-8' codec can't encode characters in position 1-2: surrogates not allowed" || !IsValueError(err) {
		t.Fatalf("%v", err)
	}
	z.Filename = "é\xffb"
	if _, _, err := z.encodeFilenameFlags(); err == nil || err.Error() != "'utf-8' codec can't encode character '\\udcff' in position 1: surrogates not allowed" {
		t.Fatalf("%v", err)
	}
}

func TestPre1980AndPost2107Timestamps(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	os.WriteFile(old, []byte("x"), 0o600)
	os.Chtimes(old, time.Unix(0, 0), time.Unix(0, 0))
	zf, err := Create(filepath.Join(dir, "o.zip"), Deflated, writeLevel(6))
	if err != nil {
		t.Fatal(err)
	}
	err = zf.Write(old, "old")
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Msg != "ZIP does not support timestamps before 1980" {
		t.Fatalf("%v", err)
	}
	late := time.Date(2110, 1, 1, 0, 0, 0, 0, time.Local)
	os.Chtimes(old, late, late)
	err = zf.Write(old, "late")
	var se *StructError
	if !errors.As(err, &se) || se.Msg != "'H' format requires 0 <= number <= 65535" || len(zf.Filelist) != 0 || zf.writing {
		t.Fatalf("%v %d", err, len(zf.Filelist))
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, filepath.Join(dir, "o.zip")); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestArcnameNormalizationStripsRootsAndRefusesEmpty(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	os.WriteFile(f, []byte("x"), 0o600)
	zi, err := ZipInfoFromFile(f, "//a/./b/../c", true)
	if err != nil || zi.Filename != "a/c" || zi.FileSize != 1 {
		t.Fatalf("%v %v", zi, err)
	}
	if _, err := ZipInfoFromFile(f, "/", true); err == nil || err.Error() != "string index out of range" {
		t.Fatalf("%v", err)
	}
	if zi, err := ZipInfoFromFile(f, "a\x00b", true); err != nil || zi.Filename != "a" {
		t.Fatalf("%v %v", zi, err)
	}
}

func TestOpenedMemberIsFinalizedAndCanBeCutAway(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "p.zip")
	zf, err := Create(out, Deflated, writeLevel(6))
	if err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep")
	os.WriteFile(keep, []byte("kept"), 0o600)
	if err := zf.Write(keep, "keep"); err != nil {
		t.Fatal(err)
	}
	n := len(zf.Filelist)
	w, err := zf.Open("partial.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zf.Open("other"); err == nil {
		t.Fatal("a second open handle was allowed")
	}
	w.Write(bytes.Repeat([]byte{7}, 70000))
	if err := w.Close(); err != nil || len(zf.Filelist) != n+1 || zf.Filelist[n].DateTime != defaultDateTime {
		t.Fatalf("%v %d", err, len(zf.Filelist))
	}
	if zf.Filelist[n].ExternalAttr != 0o600<<16 || zf.Filelist[n].FileSize != 70000 {
		t.Fatalf("%o %d", zf.Filelist[n].ExternalAttr, zf.Filelist[n].FileSize)
	}
	offset := zf.Filelist[n].HeaderOffset
	if err := zf.TruncateAt(offset); err != nil {
		t.Fatal(err)
	}
	zf.StartDir = offset
	zf.Filelist = zf.Filelist[:n]
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	if bytes.Contains(raw, []byte("partial.bin")) {
		t.Fatal("cut member bytes remain")
	}
	if got := readBack(t, out); len(got) != 1 || got["keep"] != "kept" {
		t.Fatalf("%v", got)
	}
}

func TestFailedSourceReadStillFinalizesThePartialMember(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "r.zip")
	zf, err := Create(out, Stored, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := zf.Write(dir+"/missing", "missing"); err == nil || len(zf.Filelist) != 0 {
		t.Fatalf("%v", err)
	}
	if err := zf.Write(dir, "d"); err != nil || len(zf.Filelist) != 1 || !zf.Filelist[0].IsDir() {
		t.Fatalf("%v", err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Write(dir, "again"); err == nil || err.Error() != "Attempt to write to ZIP archive that was already closed" {
		t.Fatalf("%v", err)
	}
	raw, _ := os.ReadFile(out)
	end := raw[len(raw)-22:]
	if string(end[:4]) != endArchiveSig || binary.LittleEndian.Uint16(end[10:]) != 1 {
		t.Fatalf("end record %x", end)
	}
}

func TestStripExtraDropsOnlyNamedRecords(t *testing.T) {
	data := []byte{1, 0, 2, 0, 9, 9, 5, 0, 1, 0, 8, 7}
	if got := stripExtra(data, map[int]bool{1: true}); !bytes.Equal(got, []byte{5, 0, 1, 0, 8, 7}) {
		t.Fatalf("%v", got)
	}
}
