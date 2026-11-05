package pm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const peLDFlags = " -shared  -Wl,-z,noexecstack -Wl,-z,relro -Wl,-z,now -Wl,--build-id=sha1   -Wl,--exclude-libs,ALL"

var pePBSLinux = map[string]string{
	"CC": "clang -pthread", "CXX": "clang++ -pthread",
	"LDSHARED": "clang -pthread" + peLDFlags, "LDCXXSHARED": "clang++ -pthread" + peLDFlags,
	"AR": "/tools/llvm/bin/llvm-ar",
}

// Directory of empty executable shell scripts
func peExecutables(t *testing.T, dir string, names ...string) string {
	t.Helper()
	for _, name := range names {
		writeFile(t, filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755)
	}
	return dir
}

// Build environment a managed environment gets for an env and optional interpreter
func peManagedEnv(t *testing.T, destination, python string, env map[string]string, cache string) map[string]string {
	t.Helper()
	e, err := ManagedEnvironment(context.Background(), destination, ManagedEnvironmentOptions{Python: python, Env: env, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return e.Env
}

func TestBuildCompilersPinnedInterpreterBuildsWithGccWhenItsClangIsMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	root := t.TempDir()
	recorded, _ := json.Marshal(pePBSLinux)
	pinned := filepath.Join(root, "pinned-python")
	writeFile(t, pinned, []byte("#!/bin/sh\necho '"+string(recorded)+"'\n"), 0o755)
	peToolchain(t, filepath.Join(root, "uv"), pinned)
	gccOnly := peExecutables(t, filepath.Join(root, "gcc-only"), "gcc", "g++", "ar")
	built := peManagedEnv(t, filepath.Join(root, "gcc"), "", map[string]string{"PATH": gccOnly}, root)
	got := map[string]string{}
	for k := range pePBSLinux {
		got[k] = built[k]
	}
	want := map[string]string{"CC": "gcc -pthread", "CXX": "g++ -pthread", "LDSHARED": "gcc -pthread" + peLDFlags,
		"LDCXXSHARED": "g++ -pthread" + peLDFlags, "AR": "ar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	chosen := peManagedEnv(t, filepath.Join(root, "chosen"), "", map[string]string{"PATH": gccOnly, "CXX": "clang++"}, root)
	if chosen["CXX"] != "clang++" || chosen["CC"] != "gcc -pthread" {
		t.Fatalf("chosen %v", chosen)
	}
	if _, ok := chosen["LDCXXSHARED"]; ok {
		t.Fatal("linker derived from a compiler the user chose")
	}
	withClang := peExecutables(t, filepath.Join(root, "with-clang"), "clang", "clang++", "gcc", "g++")
	kept := peManagedEnv(t, filepath.Join(root, "kept"), "", map[string]string{"PATH": withClang}, root)
	for _, k := range []string{"CC", "CXX", "LDSHARED", "LDCXXSHARED"} {
		if _, ok := kept[k]; ok {
			t.Fatalf("%s substituted on a host with the recorded compilers", k)
		}
	}
	other := peManagedEnv(t, filepath.Join(root, "other"), peHostPython(t), map[string]string{"PATH": gccOnly}, root)
	if !reflect.DeepEqual(other, map[string]string{"PATH": gccOnly}) {
		t.Fatalf("other interpreter %v", other)
	}
}

func TestBuildCompilersBuildFailureWithoutAnyCompilerNamesTheInstall(t *testing.T) {
	missing := "error: Failed to build `python-olm==3.2.16`\n  ├─▶ The build backend returned an error\n" +
		"  ╰─▶ Call to `setuptools.build_meta.build_wheel` failed (exit status: 1)\n" +
		"      error: [Errno 2] No such file or directory: 'clang++'\n"
	var failure *BuildFailure
	if err := ClassifyUVFailure("pip", 1, missing); !errors.As(err, &failure) || !strings.Contains(failure.Remedy, "build-essential") {
		t.Fatalf("missing compiler: %#v", err)
	}
	remedy := func(output string) string {
		var failure *BuildFailure
		if !errors.As(ClassifyUVFailure("pip", 1, output), &failure) {
			t.Fatalf("not a build failure: %s", output)
		}
		return failure.Remedy
	}
	if strings.Contains(remedy(strings.Replace(missing, "'clang++'", "'libolm/include/olm.h'", 1)), "build-essential") {
		t.Fatal("a missing header named a compiler install")
	}
	if !strings.Contains(remedy(strings.Replace(missing, "[Errno 2] No such file or directory: 'clang++'", "command 'clang++' failed: No such file or directory", 1)), "build-essential") {
		t.Fatal("an absent compiler command lost its install hint")
	}
	if strings.Contains(remedy(strings.Replace(missing, "[Errno 2] No such file or directory: 'clang++'", "command '/usr/bin/gcc' failed with exit code 1", 1)), "build-essential") {
		t.Fatal("a compiler that ran named an install")
	}
	if got := MissingCompilerRemedy("No CMAKE_CXX_COMPILER could be found"); !strings.HasPrefix(got, "this package compiles native code and no compiler was found: ") {
		t.Fatalf("cmake %q", got)
	}
}

// Writes a gzip tarball of a directory under its base name
func peTarDir(t *testing.T, dir, archive string) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(filepath.Dir(dir), path)
		header, _ := tar.FileInfoHeader(info, "")
		header.Name = filepath.ToSlash(rel)
		tw.WriteHeader(header)
		if info.Mode().IsRegular() {
			data, _ := os.ReadFile(path)
			tw.Write(data)
		}
		return nil
	})
	tw.Close()
	gz.Close()
	f.Close()
}

func TestBuildCompilersManagedBuildArchivesNativeCode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("platforms macos")
	}
	isolateHome(t)
	root := t.TempDir()
	peToolchain(t, peUV(t), peHostPython(t))
	source := filepath.Join(root, "native_archive_probe-1.0")
	peWrite(t, filepath.Join(source, "pyproject.toml"), "[build-system]\nrequires=[]\nbuild-backend=\"backend\"\nbackend-path=[\".\"]\n")
	peWrite(t, filepath.Join(source, "answer.c"), "int answer(void) { return 42; }\n")
	peWrite(t, filepath.Join(source, "probe.c"), "#include <Python.h>\nextern int answer(void);\n"+
		"static PyObject *value(PyObject *self, PyObject *args) { return PyLong_FromLong(answer()); }\n"+
		"static PyMethodDef methods[] = {{\"value\", value, METH_NOARGS, \"answer\"}, {NULL,NULL,0,NULL}};\n"+
		"static struct PyModuleDef module = {PyModuleDef_HEAD_INIT, \"native_archive_probe\", NULL, -1, methods};\n"+
		"PyMODINIT_FUNC PyInit_native_archive_probe(void) { return PyModule_Create(&module); }\n")
	peWrite(t, filepath.Join(source, "backend.py"), `import os, shlex, subprocess, sys, sysconfig
from pathlib import Path
from zipfile import ZipFile

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    cc = shlex.split(os.environ.get("CC") or sysconfig.get_config_var("CC"))
    ar = shlex.split(os.environ.get("AR") or sysconfig.get_config_var("AR"))
    subprocess.run(cc + ["-fPIC", "-c", "answer.c", "-o", "answer.o"], check=True)
    subprocess.run(ar + ["rcs", "libanswer.a", "answer.o"], check=True)
    extension = "native_archive_probe" + sysconfig.get_config_var("EXT_SUFFIX")
    subprocess.run(cc + ["-bundle", "-undefined", "dynamic_lookup",
        "-I" + sysconfig.get_path("include"), "probe.c", "libanswer.a", "-o", extension], check=True)
    platform = sysconfig.get_platform().replace("-", "_").replace(".", "_")
    abi = "cp" + str(sys.version_info.major) + str(sys.version_info.minor)
    tag = abi + "-" + abi + "-" + platform
    name = "native_archive_probe-1.0-" + tag + ".whl"
    dist = "native_archive_probe-1.0.dist-info"
    entries = {
        extension: Path(extension).read_bytes(),
        dist + "/METADATA": "Metadata-Version: 2.1\nName: native-archive-probe\nVersion: 1.0\n",
        dist + "/WHEEL": "Wheel-Version: 1.0\nRoot-Is-Purelib: false\nTag: " + tag + "\n",
    }
    entries[dist + "/RECORD"] = "".join(path + ",,\n" for path in entries)
    with ZipFile(Path(wheel_directory) / name, "w") as wheel:
        for path, body in entries.items():
            wheel.writestr(path, body)
    return name
`)
	archive := filepath.Join(root, "native_archive_probe-1.0.tar.gz")
	peTarDir(t, source, archive)
	env := pyEnviron()
	delete(env, "AR")
	delete(env, "CC")
	executable, err := BuildRequirementsEnvironment(context.Background(), BuildRequirementsEnvironmentOptions{
		Requirements: []string{"native-archive-probe @ file://" + filepath.ToSlash(archive)}, Out: filepath.Join(root, "built"),
		Python: peHostPython(t), Env: env, Cache: filepath.Join(root, "cache"), Offline: true, Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := peRun(t, []string{executable, "-I", "-c", "import native_archive_probe; print(native_archive_probe.value())"}, root, env); got != "42" {
		t.Fatalf("probe %q", got)
	}
}

func TestBuildCompilersArchiverDefaultsPreserveExplicitAndCustomToolchains(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("platforms macos")
	}
	root := t.TempDir()
	pinned := peHostPython(t)
	peToolchain(t, filepath.Join(root, "uv"), pinned)
	alias := filepath.Join(root, "python-alias")
	if err := os.Symlink(pinned, alias); err != nil {
		t.Fatal(err)
	}
	caller := map[string]string{"AR": "/custom/ar", "CC": "/custom/clang"}
	if got := peManagedEnv(t, filepath.Join(root, "explicit"), alias, caller, root); !reflect.DeepEqual(got, caller) {
		t.Fatalf("explicit %v", got)
	}
	if got := peManagedEnv(t, filepath.Join(root, "default"), alias, map[string]string{}, root); !reflect.DeepEqual(got, map[string]string{"AR": "/usr/bin/ar"}) {
		t.Fatalf("default %v", got)
	}
	if got := peManagedEnv(t, filepath.Join(root, "custom"), filepath.Join(root, "custom-python"), map[string]string{}, root); len(got) != 0 {
		t.Fatalf("custom %v", got)
	}
	if !reflect.DeepEqual(caller, map[string]string{"AR": "/custom/ar", "CC": "/custom/clang"}) {
		t.Fatal("caller env mutated")
	}
}
