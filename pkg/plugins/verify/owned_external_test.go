package verify_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	_ "github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
)

const (
	envServe      = "ICEBATH_VERIFY_TEST_SERVE_PORT"
	envOwner      = "ICEBATH_VERIFY_TEST_OWNER"
	envChild      = "ICEBATH_VERIFY_TEST_CHILD"
	envGrandchild = "ICEBATH_VERIFY_TEST_GRANDCHILD"
)

// Dispatches the prune entry and the server, owner and tree child modes before the suite runs
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry()
	if out := os.Getenv(envGrandchild); out != "" {
		recordAndIdle(out, "grandchild")
	}
	if out := os.Getenv(envChild); out != "" {
		grand := exec.Command(os.Args[0], "-test.run=^$")
		grand.Env = append(os.Environ(), envGrandchild+"="+out)
		if err := grand.Start(); err != nil {
			os.Exit(3)
		}
		recordAndIdle(out, "child")
	}
	if port := os.Getenv(envServe); port != "" {
		_ = http.ListenAndServe("127.0.0.1:"+port, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		os.Exit(4)
	}
	if out := os.Getenv(envOwner); out != "" {
		start := envChild + "=" + pyshlex.Quote(out) + " exec " + pyshlex.Quote(os.Args[0]) + " -test.run='^$'"
		opts := verify.NewVerifyOptions()
		opts.Phases, opts.ReadyTimeout = []string{"start"}, 120
		go func() {
			_, _ = verify.RunVerify(context.Background(), out, &verify.Recipe{Name: "x", Start: &start, Port: intp(1), ReadinessPath: "/"}, opts)
		}()
		time.Sleep(300 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func intp(n int) *int { return &n }

// Writes this process identity receipt then idles until killed
func recordAndIdle(out, role string) {
	birth, _ := proc.Birth(os.Getpid())
	data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "created": birth})
	_ = os.WriteFile(filepath.Join(out, role+".json.tmp"), data, 0o644)
	_ = os.Rename(filepath.Join(out, role+".json.tmp"), filepath.Join(out, role+".json"))
	time.Sleep(300 * time.Second)
	os.Exit(0)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func runStart(t *testing.T, r *verify.Recipe, readyTimeout float64, portOverride *int) *verify.VerifyResult {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	opts := verify.NewVerifyOptions()
	opts.Phases, opts.ReadyTimeout, opts.PortOverride = []string{"start"}, readyTimeout, portOverride
	res, err := verify.RunVerify(context.Background(), t.TempDir(), r, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReadinessAgainstLiveServer(t *testing.T) {
	port := freePort(t)
	start := fmt.Sprintf("%s=%d exec %s -test.run='^$'", envServe, port, pyshlex.Quote(os.Args[0]))
	res := runStart(t, &verify.Recipe{Name: "x", Start: &start, Port: intp(port), ReadinessPath: "/"}, 15, nil)
	if res.Readiness == nil || !res.Readiness.Ready || *res.Readiness.StatusCode != 200 || !res.OK() {
		t.Fatalf("readiness %#v", res.Readiness)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d/", port); res.Readiness.URL != want {
		t.Fatalf("url %q want %q", res.Readiness.URL, want)
	}
}

func TestReadinessTimeoutWhenNothingListens(t *testing.T) {
	port := freePort(t)
	start := "sleep 30"
	res := runStart(t, &verify.Recipe{Name: "x", Start: &start, Port: intp(port), ReadinessPath: "/"}, 1.5, nil)
	if res.Readiness == nil || res.Readiness.Ready || res.OK() {
		t.Fatalf("readiness %#v", res.Readiness)
	}
	if res.Readiness.Error == nil || *res.Readiness.Error != "<urlopen error [Errno 111] Connection refused>" {
		t.Fatalf("error %v", res.Readiness.Error)
	}
}

func TestPortOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	start := "sleep 30"
	res := runStart(t, &verify.Recipe{Name: "x", Start: &start, Port: intp(1), ReadinessPath: "/"}, 10, &port)
	if !res.Readiness.Ready || *res.Readiness.StatusCode != 204 {
		t.Fatalf("readiness %#v", res.Readiness)
	}
}

func TestVerifyStartPhaseOwnsThenReleasesTheAppGroup(t *testing.T) {
	var owned, released []int
	prevSpawn, prevRelease := verify.SpawnOwned, verify.ReleaseProcessGroupOnceEmpty
	t.Cleanup(func() { verify.SpawnOwned, verify.ReleaseProcessGroupOnceEmpty = prevSpawn, prevRelease })
	verify.SpawnOwned = func(ctx context.Context, cmd *exec.Cmd, purpose string) error {
		err := prevSpawn(ctx, cmd, purpose)
		if err == nil && purpose == "verify-app" {
			owned = append(owned, cmd.Process.Pid)
		}
		return err
	}
	verify.ReleaseProcessGroupOnceEmpty = func(pgid int, poll func(), timeout time.Duration) bool {
		ok := prevRelease(pgid, poll, timeout)
		if ok {
			released = append(released, pgid)
		}
		return ok
	}
	start := "sleep 30"
	res := runStart(t, &verify.Recipe{Name: "x", Start: &start, Port: intp(1), ReadinessPath: "/"}, 0.3, nil)
	if res.Readiness.Ready || len(owned) != 1 || fmt.Sprint(released) != fmt.Sprint(owned) {
		t.Fatalf("ready %v owned %v released %v", res.Readiness.Ready, owned, released)
	}
}

type identity struct {
	Pid     int     `json:"pid"`
	Created float64 `json:"created"`
}

func readReceipt(t *testing.T, path string) identity {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			var id identity
			if err := json.Unmarshal(raw, &id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt %s never appeared", filepath.Base(path))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Zombie aware liveness of a recorded process identity
func alive(id identity) bool {
	birth, err := proc.Birth(id.Pid)
	return err == nil && birth == id.Created && !proc.Zombie(id.Pid)
}

func TestSigkilledVerifyOwnerTakesItsAppTreeAndSparesALiveOwners(t *testing.T) {
	tmp := t.TempDir()
	owners := map[string]*exec.Cmd{}
	ids := map[string][]identity{}
	t.Cleanup(func() {
		for _, tree := range ids {
			for _, id := range tree {
				if alive(id) {
					_ = syscall.Kill(id.Pid, syscall.SIGKILL)
				}
			}
		}
		for _, owner := range owners {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	for _, name := range []string{"dead", "live"} {
		out := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Join(tmp, "home-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(out, 0o755); err != nil {
			t.Fatal(err)
		}
		owner := exec.Command(os.Args[0], "-test.run=^$")
		owner.Env = append(os.Environ(), envOwner+"="+out, "ICEBATH_HOME="+filepath.Join(tmp, "home-"+name, ".icebath"),
			"HOME="+filepath.Join(tmp, "home-"+name))
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		owners[name] = owner
		ids[name] = []identity{readReceipt(t, filepath.Join(out, "child.json")), readReceipt(t, filepath.Join(out, "grandchild.json"))}
	}
	for _, tree := range ids {
		for _, id := range tree {
			if !alive(id) {
				t.Fatalf("process %d not alive before the kill", id.Pid)
			}
		}
	}
	_ = owners["dead"].Process.Kill()
	_ = owners["dead"].Wait()
	deadline := time.Now().Add(15 * time.Second)
	for alive(ids["dead"][0]) || alive(ids["dead"][1]) {
		if time.Now().After(deadline) {
			t.Fatal("the verify-app tree outlived its kill -9'd owner")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, id := range ids["live"] {
		if !alive(id) {
			t.Fatalf("a live owner's verify-app process %d was killed", id.Pid)
		}
	}
}
