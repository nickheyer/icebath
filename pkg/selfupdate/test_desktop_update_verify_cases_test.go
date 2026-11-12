package selfupdate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Packaged Desktop fixture with a readable archive, a stamped renderer and the executable lookups replaced
func dvBundle(t *testing.T) (root, archive, dist string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the freshness stamp runs under Node")
	}
	root = t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "home"))
	desktop := filepath.Join(root, "apps", "desktop")
	resources := filepath.Join(desktop, "release", "fixture", "resources")
	dist = filepath.Join(resources, "app.asar.unpacked", "dist")
	sbWrite(t, filepath.Join(dist, "index.html"), `<script type="module" src="./assets/index.js"></script>`)
	sbWrite(t, filepath.Join(dist, "assets", "index.js"), "export {};")
	entry := []byte(`import "electron";`)
	sbWrite(t, filepath.Join(dist, "electron-main.mjs"), string(entry))
	pkgText, err := pyjson.Dumps(pyjson.ObjectOf("main", "dist/electron-main.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	headerText, err := pyjson.Dumps(pyjson.ObjectOf("files", pyjson.ObjectOf(
		"package.json", pyjson.ObjectOf("size", len(pkgText), "offset", "0"),
		"dist", pyjson.ObjectOf("files", pyjson.ObjectOf("electron-main.mjs", pyjson.ObjectOf("size", len(entry), "unpacked", true))))))
	if err != nil {
		t.Fatal(err)
	}
	header := []byte(headerText)
	padded := append(append([]byte{}, header...), make([]byte, (4-len(header)%4)%4)...)
	var buf bytes.Buffer
	for _, v := range []int{4, 8 + len(padded), 4 + len(padded), len(header)} {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(v))
	}
	buf.Write(padded)
	buf.WriteString(pkgText)
	archive = filepath.Join(resources, "app.asar")
	sbWrite(t, archive, buf.String())
	sbWrite(t, filepath.Join(root, ".gitignore"), "apps/desktop/release/\n")
	exe := filepath.Join(filepath.Dir(resources), "Icebath.exe")
	prevVerify, prevIntegrity, prevExe := verifyPackagedExecutable, verifyExeIntegrityError, desktopPackagedExecutable
	t.Cleanup(func() {
		verifyPackagedExecutable, verifyExeIntegrityError, desktopPackagedExecutable = prevVerify, prevIntegrity, prevExe
	})
	verifyPackagedExecutable = func(string) (string, bool) { return exe, true }
	verifyExeIntegrityError = func(string) string { return "" }
	desktopPackagedExecutable = func(string) (string, bool) { return exe, true }
	for _, name := range []string{"scripts/build/freshness.mjs", "scripts/build/frontend-common.mjs"} {
		raw, err := os.ReadFile(filepath.Join(dbrHermesRepository, name))
		if err != nil {
			t.Fatal(err)
		}
		sbWrite(t, filepath.Join(root, filepath.FromSlash(name)), string(raw))
	}
	script, err := pyjson.Dumps("file://" + filepath.ToSlash(filepath.Join(root, "scripts", "build", "freshness.mjs")))
	if err != nil {
		t.Fatal(err)
	}
	stamp := exec.Command(node, "--input-type=module", "-e", "import {recordProduct, buildInputs} from "+script+";"+
		"const [source, product, out] = process.argv.slice(1);"+
		"recordProduct({source, product, out, inputs: buildInputs(source, product)});", root, "desktop", dist)
	if out, err := stamp.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	t.Cleanup(SwapSourceToolsForTests(sourceEnsure, func(context.Context) (string, bool) { return node, true }))
	return root, archive, dist
}

// Root as the optional caller argument the verify takes
func dvRoot(root string) *string { return &root }

func TestReadablePackagedEntryPasses(t *testing.T) {
	root, _, _ := dvBundle(t)
	if err := verifyWindowsDesktopUpdate(context.Background(), dvRoot(root)); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentStampDoesNotHideDamagedOutput(t *testing.T) {
	for _, damage := range []string{"archive", "truncated", "entry", "empty-index", "unreadable-index", "no-module"} {
		t.Run(damage, func(t *testing.T) {
			root, archive, dist := dvBundle(t)
			switch damage {
			case "archive":
				sbWrite(t, archive, "not an asar")
			case "truncated":
				raw, _ := os.ReadFile(archive)
				sbWrite(t, archive, string(raw[:len(raw)-4]))
			case "entry":
				sbWrite(t, filepath.Join(dist, "electron-main.mjs"), "")
			default:
				body := map[string]string{"empty-index": "", "unreadable-index": "\xff", "no-module": "<html></html>"}[damage]
				sbWrite(t, filepath.Join(dist, "index.html"), body)
			}
			if err := verifyWindowsDesktopUpdate(context.Background(), dvRoot(root)); err == nil {
				t.Fatal("damaged output verified")
			}
		})
	}
}

func TestDefaultRootIsTheImportedCheckoutNotCwd(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	prev := verifyPackagedExecutable
	t.Cleanup(func() { verifyPackagedExecutable = prev })
	var seen []string
	verifyPackagedExecutable = func(desktop string) (string, bool) {
		seen = append(seen, desktop)
		return "", false
	}
	err := verifyWindowsDesktopUpdate(context.Background(), nil)
	var py *pm.PyError
	if !errors.As(err, &py) || py.Class != "RuntimeError" || !strings.Contains(py.Message, "executable is missing") {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 1 || seen[0] != filepath.Join(checkoutRoot(), "apps", "desktop") {
		t.Fatalf("looked under %v", seen)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if info, err := os.Stat(filepath.Join(checkoutRoot(), filepath.Base(exe))); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("checkout root %s does not hold the running binary", checkoutRoot())
	}
	if checkoutRoot() == tmp {
		t.Fatal("checkout root is the cwd")
	}
}

// Packaged app lookup that finds an app under root and nowhere else
func dvAppOnlyUnder(root string) func(string) (string, bool) {
	desktop, _ := filepath.Abs(filepath.Join(root, "apps", "desktop"))
	return func(candidate string) (string, bool) {
		if abs, _ := filepath.Abs(candidate); abs == desktop {
			return filepath.Join(desktop, "release", "fixture", "Icebath.exe"), true
		}
		return "", false
	}
}

func TestCallerRootWithoutAPackagedAppFallsBackToTheCheckout(t *testing.T) {
	checkout, _, _ := dvBundle(t)
	prevCheckout := verifyCheckoutRoot
	t.Cleanup(func() { verifyCheckoutRoot = prevCheckout })
	verifyPackagedExecutable = dvAppOnlyUnder(checkout)
	verifyCheckoutRoot = func() string { return checkout }
	if err := verifyWindowsDesktopUpdate(context.Background(), dvRoot(filepath.Join(t.TempDir(), "icebath_home"))); err != nil {
		t.Fatal(err)
	}
}

func TestCallerRootThatHasAPackagedAppIsVerifiedAsGiven(t *testing.T) {
	checkout, _, _ := dvBundle(t)
	supplied := filepath.Join(t.TempDir(), "supplied")
	resources := filepath.Join(supplied, "apps", "desktop", "release", "fixture", "resources")
	sbWrite(t, filepath.Join(resources, "app.asar"), "not an asar")
	prevCheckout := verifyCheckoutRoot
	t.Cleanup(func() { verifyCheckoutRoot = prevCheckout })
	verifyPackagedExecutable = dvAppOnlyUnder(supplied)
	verifyCheckoutRoot = func() string { return checkout }
	err := verifyWindowsDesktopUpdate(context.Background(), dvRoot(supplied))
	var py *pm.PyError
	if !errors.As(err, &py) || py.Class != "RuntimeError" || !strings.Contains(py.Message, "archive or main entry is invalid") {
		t.Fatalf("err = %v", err)
	}
}
