package pm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Three served tools installed into a temp store, returning their binaries, packages and the counted verdicts
func svCheckedStore(t *testing.T) ([]string, map[string]*BinaryPackage, *[][]string) {
	t.Helper()
	tmp := isolateHome(t)
	e := &ieEnv{tmp: tmp, runtime: filepath.Join(tmp, "store"), docroot: sttMkdir(t, filepath.Join(tmp, "www")), lockPath: filepath.Join(tmp, "lock.json")}
	t.Setenv(EnvRuntimeDir, e.runtime)
	peSwap(t, &repoRoot, func() string { return filepath.Join(tmp, "repo") })
	peSwap(t, &lockfilePath, func() string { return e.lockPath })
	ieEmptyLock(t, e.lockPath)
	server := httptest.NewServer(http.FileServer(http.Dir(e.docroot)))
	t.Cleanup(server.Close)
	e.baseURL = server.URL
	registered := map[string]*BinaryPackage{}
	var list []Package
	for _, name := range []string{"first", "second", "third"} {
		p := ieBinary(name, "bin/"+name)
		registered[name] = &p
		list = append(list, &p)
	}
	ieSwapRegistry(t, list...)
	var binaries []string
	for _, name := range []string{"first", "second", "third"} {
		e.pin(t, name, "1.0", e.tar(t, name+"-1.0.tar.gz", map[string]string{"bin/" + name: "fixture tool"}))
		if _, err := e.ensure(t, name, EnsureOptions{Explicit: true}); err != nil {
			t.Fatal(err)
		}
		installed, err := Installed(context.Background(), name, false)
		if err != nil || installed == nil || installed.Binary == "" {
			t.Fatalf("%s installed %v %v", name, installed, err)
		}
		binaries = append(binaries, installed.Binary)
	}
	checks := &[][]string{}
	peSwap(t, &activationCheck, func(ctx context.Context, includeVenv bool) ([]string, error) {
		problems, err := Check(ctx, includeVenv)
		*checks = append(*checks, append([]string{}, problems...))
		return problems, err
	})
	return binaries, registered, checks
}

func TestSVActivateMovesStoreDirsAheadOfUserDirs(t *testing.T) {
	binaries, _, checks := svCheckedStore(t)
	var store []string
	for _, binary := range binaries {
		store = append(store, filepath.Dir(binary))
	}
	userDir := string(os.PathSeparator) + filepath.Join("user", "bin")
	t.Setenv("PATH", strings.Join([]string{userDir, store[1]}, string(os.PathListSeparator)))
	if problems, err := Activate(context.Background(), false); err != nil || len(problems) != 0 {
		t.Fatalf("activate gave %v %v", problems, err)
	}
	if !reflect.DeepEqual(*checks, [][]string{{}}) {
		t.Fatalf("checks %v", *checks)
	}
	if got := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)); !reflect.DeepEqual(got, append(store, userDir)) {
		t.Fatalf("PATH %v", got)
	}
}

func TestSVDriftActivatesIntactChainsAndReportsTheLiveVerdict(t *testing.T) {
	binaries, registered, checks := svCheckedStore(t)
	t.Setenv("PATH", os.Getenv("PATH"))
	userDir := string(os.PathSeparator) + filepath.Join("user", "bin")
	registered["third"].Deps = []string{"second"}
	ctx := context.Background()
	if problems, err := Activate(ctx, false); err != nil || len(problems) != 0 {
		t.Fatalf("healthy activate gave %v %v", problems, err)
	}
	t.Setenv("PATH", userDir)
	if err := os.Remove(binaries[1]); err != nil {
		t.Fatal(err)
	}
	problems, err := Activate(ctx, false)
	if err != nil || !reflect.DeepEqual(problems, []string{"second: not installed or outdated"}) {
		t.Fatalf("damaged activate gave %v %v", problems, err)
	}
	if !reflect.DeepEqual(*checks, [][]string{{}, problems}) {
		t.Fatalf("checks %v", *checks)
	}
	if got := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)); !reflect.DeepEqual(got, []string{filepath.Dir(binaries[0]), userDir}) {
		t.Fatalf("PATH %v", got)
	}
}
