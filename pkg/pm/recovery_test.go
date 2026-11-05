package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Process environment as a map
func sttEnviron() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// Installs a ruamel.yaml distribution into site, its parser module and metadata each optional
func sttInstallRuamel(t *testing.T, site string, withMain, withMetadata bool) {
	t.Helper()
	pkg := filepath.Join(site, "ruamel", "yaml")
	writeFile(t, filepath.Join(pkg, "__init__.py"), []byte("from ruamel.yaml.main import *  # NOQA\n\n__version__ = '0.18.6'\n"), 0o666)
	if withMain {
		writeFile(t, filepath.Join(pkg, "main.py"), []byte("__all__ = ['YAML']\n\n\nclass YAML:\n    def __init__(self, typ=None, pure=False):\n        self.typ = typ or ['rt']\n        self.pure = pure\n"), 0o666)
	}
	if !withMetadata {
		return
	}
	dist := filepath.Join(site, "ruamel.yaml-0.18.6.dist-info")
	writeFile(t, filepath.Join(dist, "METADATA"), []byte("Metadata-Version: 2.1\nName: ruamel.yaml\nVersion: 0.18.6\nRequires-Python: >=3.7\n"), 0o666)
	writeFile(t, filepath.Join(dist, "WHEEL"), []byte("Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n"), 0o666)
	writeFile(t, filepath.Join(dist, "top_level.txt"), []byte("ruamel\n"), 0o666)
	writeFile(t, filepath.Join(dist, "RECORD"), []byte("ruamel/yaml/__init__.py,,\nruamel/yaml/main.py,,\nruamel.yaml-0.18.6.dist-info/METADATA,,\nruamel.yaml-0.18.6.dist-info/WHEEL,,\nruamel.yaml-0.18.6.dist-info/top_level.txt,,\nruamel.yaml-0.18.6.dist-info/RECORD,,\n"), 0o666)
}

func TestStartupValidationChecksRealRuamelDependency(t *testing.T) {
	for _, failure := range []string{"None", "missing_distribution", "broken_module"} {
		t.Run(failure, func(t *testing.T) {
			isolateHome(t)
			tmp := t.TempDir()
			candidate := filepath.Join(tmp, "candidate")
			sttOutput(t, tmp, sttHostPython(t), "-m", "venv", "--without-pip", candidate)
			python := VenvPython(candidate)
			target := strings.TrimSpace(sttOutput(t, tmp, python, "-I", "-c", "import sysconfig; print(sysconfig.get_paths()['purelib'])"))
			sttInstallRuamel(t, target, failure != "broken_module", failure != "missing_distribution")
			writeFile(t, filepath.Join(tmp, "pyproject.toml"), []byte("[project]\nname=\"yaml-recovery\"\ndependencies=[\"ruamel.yaml\"]\n"), 0o666)

			err := ValidateEnvironment(context.Background(), python, nil, sttEnviron(), tmp)
			if failure == "None" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var installErr *InstallError
			if !errors.As(err, &installErr) || !strings.Contains(err.Error(), "ruamel") {
				t.Fatalf("got %v, want an install error naming ruamel", err)
			}
		})
	}
}
