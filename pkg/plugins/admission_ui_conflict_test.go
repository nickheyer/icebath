package plugins

import (
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Git origin of one named plugin at a pin importing plugin_proof_dep at that pin, and its HEAD sha
func pubOriginNamed(t *testing.T, root, name, pin string) (string, string) {
	t.Helper()
	repo := filepath.Join(root, name+"-origin")
	c6Git(t, root, "init", "-qb", "main", repo)
	vtWrite(t, filepath.Join(repo, "plugin.yaml"), "name: "+name+"\npython_dependencies:\n- plugin-proof-dep=="+pin+"\nversion: '"+pin+"'\n")
	vtWrite(t, filepath.Join(repo, "helper.py"), "import plugin_proof_dep as plugin_proof_dep\nVERSION = "+pyrepr.StrRepr(pin)+
		"\ndef register(ctx):\n    return (VERSION, plugin_proof_dep.__version__)\n")
	return repo, c6Commit(t, repo, "fixture "+pin)
}
