package pymimetypes

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

func TestGuessTypeTables(t *testing.T) {
	d := New()
	cases := []struct {
		url, typ, enc string
		strict        bool
	}{
		{"a.png", "image/png", "", true},
		{"dir.d/A.PNG", "image/png", "", true},
		{"x.tar.gz", "application/x-tar", "gzip", true},
		{"x.tgz", "application/x-tar", "gzip", true},
		{"x.svgz", "image/svg+xml", "gzip", true},
		{"x.ico", "image/vnd.microsoft.icon", "", true},
		{"x.tif", "image/tiff", "", true},
		{"x.heic", "image/heic", "", true},
		{".png", "", "", true},
		{"noext", "", "", true},
		{"x.unknownext", "", "", false},
		{"http://h/p/a.GIF?q=1#f", "image/gif", "", true},
		{"https://h/p/a.png;params", "image/png", "", true},
		{"data:image/webp;base64,AAAA", "image/webp", "", true},
		{"data:foo,bar", "text/plain", "", true},
		{"data:a=b/c,bar", "text/plain", "", true},
		{"data:nocomma", "", "", true},
		{"c:/dir/x.jpeg", "image/jpeg", "", true},
	}
	for _, c := range cases {
		typ, enc, err := d.GuessType(c.url, c.strict)
		if err != nil || typ != c.typ || enc != c.enc {
			t.Errorf("GuessType(%q) = %q %q %v want %q %q", c.url, typ, enc, err, c.typ, c.enc)
		}
	}
	if typ, _, _ := d.GuessType("x.jpg", true); typ != "image/jpeg" {
		t.Errorf("strict jpg %q", typ)
	}
	if _, _, err := d.GuessType("//[bad/x.png", true); !errors.Is(err, pyurl.ErrInvalidIPv6) {
		t.Errorf("bracket error %v", err)
	}
}

func TestReadMimeTypesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mime.types")
	body := "# comment line\nimage/x-test tst tst2 # trailing\r\nimage/x-other oth\rapplication/x-empty\n#image/x-skip skp\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New()
	if err := d.Read(path, true); err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]string{"a.tst": "image/x-test", "a.tst2": "image/x-test", "a.oth": "image/x-other", "a.skp": "", "a.trailing": ""} {
		if typ, _, _ := d.GuessType(url, true); typ != want {
			t.Errorf("%s = %q want %q", url, typ, want)
		}
	}
	if err := d.Read(filepath.Join(t.TempDir(), "missing"), true); err == nil {
		t.Error("missing file read without error")
	}
	d.AddType("", ".empty", true)
	if typ, _, _ := d.GuessType("a.empty", false); typ != "" {
		t.Errorf("empty type added %q", typ)
	}
}

func TestNonStrictCommonTypes(t *testing.T) {
	d := New()
	for _, e := range commonTypesDefault {
		if _, strictHit := d.types[1][e.k]; strictHit {
			continue
		}
		if typ, _, _ := d.GuessType("f"+e.k, true); typ != "" {
			t.Errorf("strict lookup hit common %s", e.k)
		}
		if typ, _, _ := d.GuessType("f"+e.k, false); typ != e.v {
			t.Errorf("non strict %s = %q want %q", e.k, typ, e.v)
		}
	}
}
