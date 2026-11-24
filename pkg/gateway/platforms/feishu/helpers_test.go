package feishu

import (
	"os"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustObject(t *testing.T, text string) (*jsonutil.Object, any) {
	t.Helper()
	v, err := jsonutil.Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsonutil.Object), v
}
