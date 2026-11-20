package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Under host isolation the audit reads the host boundary table
func TestIsolationHostAuditOfUserPluginDirs(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	for name, body := range map[string]string{"platformprobe": platformPlugin, "fine": probePlugin} {
		dir := filepath.Join(h, "plugins", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: "+name+"\nversion: '1.0'\n")
		writeFile(t, filepath.Join(dir, "__init__.py"), body)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [platformprobe, fine]\n  isolation: host\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	fine, err := auditPluginDir(filepath.Join(h, "plugins", "fine"), nil)
	if err != nil || fine.Verdict != VerdictHost {
		t.Fatalf("fine audit %+v %v", fine, err)
	}
	blocked, err := auditPluginDir(filepath.Join(h, "plugins", "platformprobe"), nil)
	if err != nil || blocked.Verdict != VerdictInProcess || len(blocked.Reasons) == 0 || !strings.Contains(blocked.Reasons[0], "register_platform") {
		t.Fatalf("platformprobe audit %+v %v", blocked, err)
	}
}
