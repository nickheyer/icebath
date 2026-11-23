package tuiserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Swaps the code skew probe for the test
func pinCodeSkew(t *testing.T, bootRev, diskRev string, skewed bool) {
	t.Helper()
	saved := detectCodeSkew
	detectCodeSkew = func() (string, string, bool) { return bootRev, diskRev, skewed }
	t.Cleanup(func() { detectCodeSkew = saved })
}

// Error object of a response, failing when the call answered a result
func errorOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	errObj, isDict := resp["error"].(map[string]any)
	if !isDict {
		t.Fatalf("no error in %v", resp)
	}
	return errObj
}

func TestStaleModelSaveKeyRefusesInsteadOfWriting(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	pinCodeSkew(t, "abc1234567", "def4567890", true)
	errObj := errorOf(t, callMethod(t, "model.save_key", map[string]any{"slug": "zai", "api_key": "sk-canary"}))
	if errObj["code"] != 5098 {
		t.Fatalf("error %v", errObj)
	}
	if raw, err := os.ReadFile(filepath.Join(h, ".env")); err == nil && strings.Contains(string(raw), "sk-canary") {
		t.Fatal("the stale process wrote the key")
	}
}
