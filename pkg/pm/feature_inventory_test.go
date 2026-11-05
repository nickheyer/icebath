package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSVInventoryUsesTheTargetAndRequiresEveryAnchor(t *testing.T) {
	tmp := isolateHome(t)
	targetRoot := filepath.Join(tmp, "target")
	if out, err := exec.Command(peHostPython(t), "-m", "venv", "--without-pip", targetRoot).CombinedOutput(); err != nil {
		t.Fatalf("venv: %v %s", err, out)
	}
	target := filepath.Join(targetRoot, "bin", "python")
	if runtime.GOOS == "windows" {
		target = filepath.Join(targetRoot, "Scripts", "python.exe")
	}
	dependencies := filepath.Join(tmp, "dependencies")
	site, err := SitePackages(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(tmp, "child.json")
	pkg := sttMkdir(t, filepath.Join(site, "inventory_anchor"))
	writeFile(t, filepath.Join(pkg, "__init__.py"), []byte("import json, pathlib, sys\npathlib.Path("+pyRepr(witness)+").write_text(json.dumps(sys.executable), encoding='utf-8')\n"), 0o666)
	writeFile(t, filepath.Join(pkg, "present.py"), []byte("VALUE = 'target'\n"), 0o666)
	writeFile(t, filepath.Join(site, "one_part.py"), nil, 0o666)
	editable := sttMkdir(t, filepath.Join(tmp, "editable"))
	writeFile(t, filepath.Join(editable, "editable_anchor.py"), nil, 0o666)
	writeFile(t, filepath.Join(site, "editable.pth"), []byte(editable+"\n"), 0o666)
	launches := filepath.Join(tmp, "launches.jsonl")
	writeFile(t, filepath.Join(site, "launches.pth"), []byte("import json, pathlib, sys; p = pathlib.Path("+pyRepr(launches)+"); "+
		"text = p.read_text(encoding='utf-8') if p.exists() else ''; "+
		"p.write_text(text + json.dumps(sys.executable) + '\\n', encoding='utf-8')\n"), 0o666)
	mapping := map[string][]string{"complete": {"inventory_anchor.present"}, "partial": {"one_part", "absent_part"},
		"host-only": {"packaging"}, "editable": {"editable_anchor"}}
	repo := sttMkdir(t, filepath.Join(tmp, "source"))
	var text strings.Builder
	text.WriteString("[project.optional-dependencies]\n")
	for _, name := range []string{"complete", "partial", "host-only", "editable"} {
		text.WriteString("\"" + name + "\"=[]\n")
	}
	writeFile(t, filepath.Join(repo, "pyproject.toml"), []byte(text.String()), 0o666)
	peSwap(t, &Anchors, mapping)
	beforePath, beforeEnv := importPathEntries(), os.Environ()
	ctx := context.Background()
	result, err := InstalledExtras(ctx, repo, dependencies, target)
	if err != nil || !reflect.DeepEqual(result, []string{"complete", "editable"}) {
		t.Fatalf("inventory %v %v", result, err)
	}
	var child string
	if err := json.Unmarshal(readFile(t, witness), &child); err != nil || child != target {
		t.Fatalf("anchor imported by %q %v, want %q", child, err, target)
	}
	var launched []string
	for _, line := range strings.Split(strings.TrimSpace(string(readFile(t, launches))), "\n") {
		var exe string
		if err := json.Unmarshal([]byte(line), &exe); err != nil {
			t.Fatal(err)
		}
		launched = append(launched, exe)
	}
	if !reflect.DeepEqual(launched, []string{target}) {
		t.Fatalf("launches %v", launched)
	}
	if !slices.Equal(importPathEntries(), beforePath) || !slices.Equal(os.Environ(), beforeEnv) {
		t.Fatal("the inventory changed this process's import path or environment")
	}
	writeFile(t, filepath.Join(site, "absent_part.py"), nil, 0o666)
	if result, err := InstalledExtras(ctx, repo, dependencies, target); err != nil || !reflect.DeepEqual(result, []string{"complete", "editable", "partial"}) {
		t.Fatalf("inventory %v %v", result, err)
	}
	_, err = InstalledExtras(ctx, repo, dependencies, filepath.Join(tmp, "missing-python"))
	var probe *FeatureProbeError
	if !errors.As(err, &probe) || !strings.Contains(err.Error(), "feature inventory failed") {
		t.Fatalf("missing interpreter gave %v", err)
	}
}
