package storage

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestClampDataUrlReadMaxMbDefaultsAndBoundsTheAttachSizePreference(t *testing.T) {
	if ClampDataURLReadMaxMB(jsToNumber(nil, false)) != DataURLReadDefaultMaxMB || ClampDataURLReadMaxMB(0) != 1 ||
		ClampDataURLReadMaxMB(256) != 256 || ClampDataURLReadMaxMB(99999) != 4096 || DataURLReadMaxBytesFromMB(16) != 16*1024*1024 {
		t.Fatal("clamp")
	}
	if ClampDataURLReadMaxMB(2.5) != 3 || ClampDataURLReadMaxMB(math.Inf(1)) != 16 || jsStringToNumber(" 0x10 ") != 16 || !math.IsNaN(jsStringToNumber("12px")) {
		t.Fatal("number conversion")
	}
}

func TestDataUrlReadMaxPersistsTheClampedCapInUserData(t *testing.T) {
	s, _ := serviceForTest(t)
	if got := jsStringify(s.DataUrlReadMaxGet(), ""); got != `{"maxMb":16,"defaultMaxMb":16,"maxBytes":16777216}` {
		t.Fatalf("get %s", got)
	}
	if got := jsStringify(s.DataUrlReadMaxSet("99999"), ""); got != `{"maxMb":4096,"defaultMaxMb":16,"maxBytes":4294967296}` {
		t.Fatalf("set %s", got)
	}
	raw, _ := os.ReadFile(filepath.Join(s.userData, "data-url-read-max.json"))
	if string(raw) != "{\n  \"maxMb\": 4096\n}" {
		t.Fatalf("file %q", raw)
	}
	if loadDataURLReadMax(s.userData).mb != 4096 {
		t.Fatal("reload")
	}
	if got := jsStringify(s.DataUrlReadMaxSet("NaN"), ""); got != `{"maxMb":16,"defaultMaxMb":16,"maxBytes":16777216}` {
		t.Fatalf("NaN %s", got)
	}
}
