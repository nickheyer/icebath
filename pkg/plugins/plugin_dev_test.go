package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func doctorPluginDir(t *testing.T, dir, manifest, init string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), manifest)
	if init != "" {
		writeFile(t, filepath.Join(dir, "__init__.py"), init)
	}
	return dir
}

func resolvedPath(p string) string {
	r, _ := pypath.Resolve(p)
	return r
}

func runDoctor(t *testing.T, ctx context.Context, target string) *PluginDoctorReport {
	t.Helper()
	report, err := doctorPlugin(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func findingText(r *PluginDoctorReport) string {
	var parts []string
	for _, f := range r.Findings {
		parts = append(parts, f.Message)
	}
	return strings.Join(parts, "\n")
}

// Records every staging copy source, failing the test once a copy runs away
func trackDoctorCopies(t *testing.T, fail error) *[]string {
	t.Helper()
	var copied []string
	previous := doctorCopyTree
	doctorCopyTree = func(src, dst string, ignore pyfs.IgnoreErrFunc) error {
		copied = append(copied, src)
		if fail != nil {
			return fail
		}
		if len(copied) > 20 {
			return errors.New("runaway staging copy")
		}
		return previous(src, dst, ignore)
	}
	t.Cleanup(func() { doctorCopyTree = previous })
	return &copied
}

const doctorNoopInit = "def register(ctx):\n    pass\n"

func hostedProfileKeys() map[string]bool {
	hostedProfilesMu.Lock()
	defer hostedProfilesMu.Unlock()
	out := map[string]bool{}
	for k := range hostedProfiles {
		out[k] = true
	}
	return out
}

const acmeProviderInit = "from providers import register_provider\nfrom providers.base import ProviderProfile\n" +
	"register_provider(ProviderProfile(name='%s', auth_type='external_process',\n                                  process_command='acme'%s))\n"

func desktopUniPlugin(t *testing.T, root string) string {
	t.Helper()
	plugin := doctorPluginDir(t, filepath.Join(root, "plugins", "uni"), "name: uni\nversion: 0.1.0\n", doctorNoopInit)
	if err := os.MkdirAll(filepath.Join(plugin, "desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(plugin, "desktop", "plugin.js"), "export default { id: 'uni' } // v1\n")
	return plugin
}

func staleWarnings(r *PluginDoctorReport) []string {
	var out []string
	for _, f := range r.Findings {
		if strings.Contains(f.Message, "stale copy") {
			out = append(out, f.Message)
		}
	}
	return out
}
