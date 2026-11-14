package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDotenvSetValuesInterpolatesAndSkipsBareKeys(t *testing.T) {
	t.Setenv("DOTENV_OUTER", "outer")
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("\xef\xbb\xbfA=1\r\nB=${A}-${DOTENV_OUTER}\nBARE\nexport C='x y'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DotenvSetValues(p)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"A": "1", "B": "1-outer", "C": "x y"}) {
		t.Fatalf("values = %v, %v", got, err)
	}
	os.WriteFile(p, []byte("A=\xff\n"), 0o644)
	if _, err := DotenvSetValues(p); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
