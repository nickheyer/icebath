package pyqrcode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type versionCase struct {
	Data    string   `json:"data"`
	EC      int      `json:"ec"`
	Version int      `json:"version"`
	Mask    int      `json:"mask"`
	Rows    []string `json:"rows"`
}

// Hex text of one module row, the most significant bit the leftmost module
func rowHex(row []bool) string {
	digits := (len(row) + 3) / 4
	pad := digits*4 - len(row)
	out := make([]byte, 0, digits)
	acc, n := 0, pad
	for _, cell := range row {
		acc <<= 1
		if cell {
			acc |= 1
		}
		n++
		if n == 4 {
			out = append(out, "0123456789abcdef"[acc])
			acc, n = 0, 0
		}
	}
	return string(out)
}

// Mask the best mask search picks for data at a level, the pattern qrcode make applies
func chosenMask(t *testing.T, data string, ec int) int {
	t.Helper()
	c := &Code{errorCorrection: ec, segments: optimalDataChunks([]byte(data), 20)}
	version, err := c.bestFit(1)
	if err != nil {
		t.Fatal(err)
	}
	c.version = version
	mask, err := c.bestMaskPattern()
	if err != nil {
		t.Fatal(err)
	}
	return mask
}

func TestEveryVersionLevelAndMaskMatchesPythonQrcode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "qr_versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Cases []versionCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	versions, masks, levels := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for i, tc := range rec.Cases {
		versions[tc.Version], masks[tc.Mask], levels[tc.EC] = true, true, true
		code, err := Make(tc.Data, tc.EC, 0)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if code.Version() != tc.Version {
			t.Fatalf("case %d ec %d len %d: version %d, want %d", i, tc.EC, len(tc.Data), code.Version(), tc.Version)
		}
		if mask := chosenMask(t, tc.Data, tc.EC); mask != tc.Mask {
			t.Fatalf("case %d version %d: mask %d, want %d", i, tc.Version, mask, tc.Mask)
		}
		matrix := code.Matrix()
		if len(matrix) != len(tc.Rows) {
			t.Fatalf("case %d: %d rows, want %d", i, len(matrix), len(tc.Rows))
		}
		for r, row := range matrix {
			if got := rowHex(row); got != tc.Rows[r] {
				t.Fatalf("case %d version %d mask %d: row %d differs", i, tc.Version, tc.Mask, r)
			}
		}
	}
	if len(versions) != 40 || len(masks) != 8 || len(levels) != 4 {
		t.Fatal(fmt.Sprintf("coverage versions %d masks %d levels %d", len(versions), len(masks), len(levels)))
	}
}
