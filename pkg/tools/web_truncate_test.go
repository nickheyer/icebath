package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

var footerPathRe = regexp.MustCompile(`read_file path="([^"]+)"`)

// test_truncation_footer_sandbox_paths.py test_local_backend_footer_keeps_the_host_path
func TestLocalBackendFooterKeepsTheHostPath(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("TERMINAL_ENV", "local")
	ResetTerminalScopeCache()
	t.Cleanup(ResetTerminalScopeCache)
	env := &Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}}
	rows := make([]string, 5000)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	out, truncated := TruncateWithFooter(env, strings.Join(rows, "\n"), "https://example.com/doc", 3000)
	m := footerPathRe.FindStringSubmatch(out)
	if !truncated || m == nil || !strings.HasPrefix(m[1], h) {
		t.Fatalf("footer path %v truncated %v", m, truncated)
	}
}
