package pyqrcode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

// One python-qrcode 8.2 print_ascii recording
type asciiCase struct {
	Data    string  `json:"data"`
	EC      int     `json:"ec"`
	Border  int     `json:"border"`
	Invert  bool    `json:"invert"`
	Version int     `json:"version"`
	ASCII   string  `json:"ascii"`
	Error   *string `json:"error"`
}

func TestPrintASCIIMatchesPythonQrcode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "qr_ascii.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Overflow string      `json:"overflow"`
		Cases    []asciiCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Cases) != 96 {
		t.Fatalf("%d recorded cases", len(rec.Cases))
	}
	for i, c := range rec.Cases {
		name := fmt.Sprintf("%02d-%.24s-ec%d-b%d-%v", i, strings.ReplaceAll(c.Data, "\n", `\n`), c.EC, c.Border, c.Invert)
		t.Run(name, func(t *testing.T) {
			code, err := Make(c.Data, c.EC, c.Border)
			if c.Error != nil {
				if err == nil || err.Error() != *c.Error {
					t.Fatalf("error %v, want %s", err, *c.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if code.Version() != c.Version {
				t.Fatalf("version %d, want %d", code.Version(), c.Version)
			}
			if got := code.PrintASCII(c.Invert); got != c.ASCII {
				t.Fatalf("ascii\n%s\nwant\n%s", got, c.ASCII)
			}
		})
	}
	if _, err := Make(strings.Repeat("z", 3000), ErrorCorrectL, 1); err == nil || err.Error() != rec.Overflow {
		t.Fatalf("overflow error %v, want %s", err, rec.Overflow)
	}
}
