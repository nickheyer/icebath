package pyzip

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

type readExpect struct {
	IsZipfile bool                         `json:"is_zipfile"`
	Namelist  []string                     `json:"namelist"`
	Read      map[string]map[string]string `json:"read"`
}

// Python exception class name of an error the reader returned
func pyClass(err error) string {
	if t, ok := err.(interface{ PyTypeName() string }); ok {
		return t.PyTypeName()
	}
	return "?"
}

// Every compression method and damage CPython 3.14.8 recorded reads back with its bytes or exact exception
func TestReaderMatchesCPython(t *testing.T) {
	dir := filepath.Join("testdata", "read")
	raw, err := os.ReadFile(filepath.Join(dir, "expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expect map[string]readExpect
	if err := json.Unmarshal(raw, &expect); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(expect))
	for name := range expect {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := expect[name]
		path := filepath.Join(dir, name)
		if got := IsZipfile(path); got != want.IsZipfile {
			t.Errorf("%s: IsZipfile %v, want %v", name, got, want.IsZipfile)
		}
		zr, err := OpenReader(path)
		if err != nil {
			t.Errorf("%s: open: %v", name, err)
			continue
		}
		if got := zr.Namelist(); !reflect.DeepEqual(got, want.Namelist) {
			t.Errorf("%s: namelist %q, want %q", name, got, want.Namelist)
		}
		for _, member := range want.Namelist {
			got := map[string]string{}
			f, err := zr.Open(member)
			if err == nil {
				var data []byte
				data, err = io.ReadAll(f)
				f.Close()
				if err == nil {
					sum := sha256.Sum256(data)
					got["sha"] = hex.EncodeToString(sum[:])
				}
			}
			if err != nil {
				got["class"], got["msg"] = pyClass(err), err.Error()
			}
			if !reflect.DeepEqual(got, want.Read[member]) {
				t.Errorf("%s %s: %v, want %v", name, member, got, want.Read[member])
			}
		}
		zr.Close()
	}
}

// A missing member is a KeyError naming it and an archive without an end record is not a zip file
func TestReaderKeyErrorAndNotAZip(t *testing.T) {
	zr, err := OpenReader(filepath.Join("testdata", "read", "stored-none.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if _, err := zr.Open("absent"); err == nil || err.Error() != `"There is no item named 'absent' in the archive"` || pyClass(err) != "KeyError" {
		t.Fatalf("missing member: %v", err)
	}
	bogus := filepath.Join(t.TempDir(), "bogus.zip")
	if err := os.WriteFile(bogus, []byte("not a zip archive at all, just text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsZipfile(bogus) {
		t.Fatal("text reported as a zip file")
	}
	if _, err := OpenReader(bogus); err == nil || err.Error() != "File is not a zip file" || pyClass(err) != "BadZipFile" {
		t.Fatalf("open bogus: %v", err)
	}
}
