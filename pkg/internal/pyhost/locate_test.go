package pyhost

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Writes each file under root with its parent directories
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Multiarch triple the sysconfig data of this machine's architecture carries
func testMultiarch() string {
	return map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH] + "-linux-gnu"
}

// Build variables of a POSIX install as the sysconfig data module spells them
func sysconfigVars(abiflags, ldlibrary, soname, libdir string, shared bool) string {
	flag := "0"
	if shared {
		flag = "1"
	}
	gil := "0"
	if abiflags == "t" {
		gil = "1"
	}
	return "build_time_vars = {'ABIFLAGS': '" + abiflags + "',\n 'INSTSONAME': '" + soname + "',\n 'LDLIBRARY': '" + ldlibrary +
		"',\n 'LIBDIR': '" + libdir + "',\n 'MULTIARCH': '" + testMultiarch() + "',\n 'Py_ENABLE_SHARED': " + flag +
		",\n 'Py_GIL_DISABLED': " + gil + ",\n 'Py_DEBUG': 0}\n"
}

// Standalone POSIX install with a shared library beside the stdlib
func standaloneTree(t *testing.T, abiflags string, shared, withLib bool) string {
	root := t.TempDir()
	tag := "python3.13" + abiflags
	files := map[string]string{
		"bin/" + tag:            "",
		"lib/" + tag + "/os.py": "",
		"lib/" + tag + "/_sysconfigdata_" + abiflags + "_linux_" + testMultiarch() + ".py": sysconfigVars(abiflags, "lib"+tag+".so", "lib"+tag+".so.1.0", "/install/lib", shared),
	}
	if withLib {
		files["lib/lib"+tag+".so.1.0"] = ""
	}
	writeTree(t, root, files)
	return root
}

// A standalone install resolves its library, stdlib, headers and site directory from the executable alone
func TestDiscoverStandaloneLinux(t *testing.T) {
	root := standaloneTree(t, "", true, true)
	in, err := DiscoverFor(filepath.Join(root, "bin", "python3.13"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if in.Major != 3 || in.Minor != 13 || in.Venv || in.BasePrefix != root || in.Prefix != root {
		t.Fatalf("%+v", in)
	}
	if in.Library != filepath.Join(root, "lib", "libpython3.13.so.1.0") || in.StdlibDir != filepath.Join(root, "lib", "python3.13") {
		t.Fatalf("library %s stdlib %s", in.Library, in.StdlibDir)
	}
	if in.SitePackages != filepath.Join(root, "lib", "python3.13", "site-packages") || in.IncludeDir != filepath.Join(root, "include", "python3.13") {
		t.Fatalf("site %s include %s", in.SitePackages, in.IncludeDir)
	}
	if in.Macros["Py_GIL_DISABLED"] || in.Macros["__APPLE__"] {
		t.Fatalf("macros %v", in.Macros)
	}
}

// A venv resolves its base install through the home key and keeps its own site directory
func TestDiscoverVenv(t *testing.T) {
	base := standaloneTree(t, "", true, true)
	venv := t.TempDir()
	writeTree(t, venv, map[string]string{
		"pyvenv.cfg": "home = " + filepath.Join(base, "bin") + "\ninclude-system-site-packages = false\nversion_info = 3.13.5\n",
		"bin/python": "",
	})
	in, err := DiscoverFor(filepath.Join(venv, "bin", "python"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !in.Venv || in.Prefix != venv || in.BasePrefix != base || in.Minor != 13 {
		t.Fatalf("%+v", in)
	}
	if in.SitePackages != filepath.Join(venv, "lib", "python3.13", "site-packages") || in.Library != filepath.Join(base, "lib", "libpython3.13.so.1.0") {
		t.Fatalf("site %s library %s", in.SitePackages, in.Library)
	}
	writeTree(t, venv, map[string]string{"pyvenv.cfg": "version_info = 3.13.5\n"})
	if _, err := DiscoverFor(filepath.Join(venv, "bin", "python"), "linux"); err == nil || !strings.HasSuffix(err.Error(), "has no home key") {
		t.Fatalf("missing home %v", err)
	}
}

// A static build refuses with the uv command that installs a shared one
func TestDiscoverNoSharedLibrary(t *testing.T) {
	root := standaloneTree(t, "", false, false)
	_, err := DiscoverFor(filepath.Join(root, "bin", "python3.13"), "linux")
	var ns *NoSharedLibraryError
	if !errors.As(err, &ns) || len(ns.Tried) != 0 {
		t.Fatalf("%T %v", err, err)
	}
	want := "is built without a shared libpython (Py_ENABLE_SHARED=0), LDLIBRARY=libpython3.13.so, install a shared build with `uv python install 3.13` and host plugins with `uv python find --managed-python 3.13`"
	if !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("message %q", err.Error())
	}
}

// A shared build whose library file is absent names every path it tried
func TestDiscoverDeclaredLibraryMissing(t *testing.T) {
	root := standaloneTree(t, "", true, false)
	_, err := DiscoverFor(filepath.Join(root, "bin", "python3.13"), "linux")
	var ns *NoSharedLibraryError
	if !errors.As(err, &ns) || len(ns.Tried) < 2 || !strings.Contains(err.Error(), "declares a shared libpython that is not installed") {
		t.Fatalf("%T %v", err, err)
	}
	if ns.Tried[0] != filepath.Join(root, "lib", "libpython3.13.so.1.0") {
		t.Fatalf("tried %v", ns.Tried)
	}
}

// A free threaded build carries its abiflags and defines Py_GIL_DISABLED for the header layout
func TestDiscoverFreeThreaded(t *testing.T) {
	root := standaloneTree(t, "t", true, true)
	in, err := DiscoverFor(filepath.Join(root, "bin", "python3.13t"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if in.ABIFlags != "t" || !in.Macros["Py_GIL_DISABLED"] || in.Library != filepath.Join(root, "lib", "libpython3.13t.so.1.0") {
		t.Fatalf("%+v", in)
	}
}

// Windows layouts resolve python3X.dll beside the executable and the flat Lib and include directories
func TestDiscoverWindows(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"python.exe": "", "python313.dll": "", "python3.dll": "", "Lib/os.py": ""})
	in, err := DiscoverFor(filepath.Join(root, "python.exe"), "windows")
	if err != nil {
		t.Fatal(err)
	}
	if in.Library != filepath.Join(root, "python313.dll") || in.Minor != 13 || !in.Macros["MS_WINDOWS"] || in.StdlibDir != filepath.Join(root, "Lib") {
		t.Fatalf("%+v", in)
	}
	venv := t.TempDir()
	writeTree(t, venv, map[string]string{"pyvenv.cfg": "home = " + root + "\nversion = 3.13.5\n", "Scripts/python.exe": ""})
	in, err = DiscoverFor(filepath.Join(venv, "Scripts", "python.exe"), "windows")
	if err != nil {
		t.Fatal(err)
	}
	if in.BasePrefix != root || in.SitePackages != filepath.Join(venv, "Lib", "site-packages") {
		t.Fatalf("%+v", in)
	}
	ft := t.TempDir()
	writeTree(t, ft, map[string]string{"python3.13t.exe": "", "python313t.dll": "", "python313t_d.dll": "", "Lib/os.py": ""})
	in, err = DiscoverFor(filepath.Join(ft, "python3.13t.exe"), "windows")
	if err != nil {
		t.Fatal(err)
	}
	if in.ABIFlags != "t" || !in.Macros["Py_GIL_DISABLED"] || in.Library != filepath.Join(ft, "python313t.dll") {
		t.Fatalf("%+v", in)
	}
}

// A macOS framework build finds its library under the framework prefix
func TestDiscoverDarwinFramework(t *testing.T) {
	root := t.TempDir()
	fw := filepath.Join(root, "Library", "Frameworks")
	writeTree(t, root, map[string]string{
		"bin/python3.13":       "",
		"lib/python3.13/os.py": "",
		"lib/python3.13/_sysconfigdata__darwin_darwin.py":          "build_time_vars = {'ABIFLAGS': '', 'LDLIBRARY': 'Python.framework/Versions/3.13/Python',\n 'PYTHONFRAMEWORK': 'Python', 'PYTHONFRAMEWORKPREFIX': '" + fw + "', 'Py_ENABLE_SHARED': 0}\n",
		"Library/Frameworks/Python.framework/Versions/3.13/Python": "",
	})
	in, err := DiscoverFor(filepath.Join(root, "bin", "python3.13"), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if in.Library != filepath.Join(fw, "Python.framework", "Versions", "3.13", "Python") || !in.Macros["__APPLE__"] {
		t.Fatalf("%+v", in)
	}
}
