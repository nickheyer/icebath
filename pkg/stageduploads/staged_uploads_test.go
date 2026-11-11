package stageduploads

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

func TestSafeUploadFilenameIsBoundedBasenameWithDocumentSuffix(t *testing.T) {
	safe := regexp.MustCompile(`\A[A-Za-z0-9_-][A-Za-z0-9._-]*\z`)
	longSuffix := "x." + strings.Repeat("a", 200)
	cases := []struct {
		id       string
		filename string
		suffix   *string
	}{
		{"cyrillic", "отчёт.docx", ptr(".docx")},
		{"traversal", "../../таблица.xlsx", ptr(".xlsx")},
		{"backslashes", `..\..\таблица.xlsx`, ptr(".xlsx")},
		{"punctuation", "!!!.PDF", ptr(".PDF")},
		{"long-stem", strings.Repeat("long-", 100) + ".docx", ptr(".docx")},
		{"compound", "report.tar.gz", ptr(".gz")},
		{"long-suffix", longSuffix, nil},
		{"dots", "../../..", ptr("")},
		{"missing", "", ptr("")},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			cleaned := SafeFilename(c.filename)
			if n := len([]rune(cleaned)); n < 1 || n > 120 {
				t.Fatalf("length %d of %q", n, cleaned)
			}
			if !safe.MatchString(cleaned) {
				t.Fatalf("unsafe %q", cleaned)
			}
			if pypath.PathName(cleaned) != cleaned {
				t.Fatalf("name of %q is %q", cleaned, pypath.PathName(cleaned))
			}
			if pystr.StripChars(pypath.PathStem(cleaned), ".-") == "" {
				t.Fatalf("empty stem of %q", cleaned)
			}
			if c.suffix != nil && pypath.PathSuffix(cleaned) != *c.suffix {
				t.Fatalf("suffix of %q is %q, want %q", cleaned, pypath.PathSuffix(cleaned), *c.suffix)
			}
		})
	}
}

func ptr(s string) *string { return &s }
