//go:build !windows

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const lr5RoleEnv = "ICEBATH_LR5_ROLE"

// Helper roles this test binary plays when re-executed, a fake llama-server among them
func TestLocalRuntimeSupervisorHelper(t *testing.T) {
	root := os.Getenv("ICEBATH_LRT_HELPER_ROOT")
	switch os.Getenv(lr5RoleEnv) {
	case "":
		return
	case "crashing-router":
		lr3Record(root, "crashing-router")
		_ = lr3HelperCmd(root, "grandchild").Start()
		lr3Wait(lr3Exists(filepath.Join(root, "grandchild.json")), 15*time.Second)
		os.Exit(1)
	case "boot-exit":
		sleeper := lr3HelperCmd(root, "sleeper")
		sleeper.Env = append(sleeper.Env, lr5RoleEnv+"=")
		if err := sleeper.Start(); err != nil {
			os.Exit(3)
		}
		sup, err := NewLlamaServerSupervisor(filepath.Join(root, "llama-server"), root, 4, 59990, nil, "", "")
		if err != nil {
			os.Exit(4)
		}
		sup.proc = lrWatchProcess(sleeper)
		lr3Wait(lr3Exists(filepath.Join(root, "sleeper.json")), 15*time.Second)
		bootMu.Lock()
		localSupervisor = sup
		bootMu.Unlock()
		stopAtExit()
		if err := LocalRuntimeAtExit(); err != nil {
			os.Exit(5)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

// Serves the llama-server router surface the supervisor speaks on the port its argv names
func lr5FakeLlamaServer(args []string) {
	opts := map[string]string{}
	for i, a := range args {
		if a == "--help" {
			fmt.Println("-lm, --load-mode MODE  dio: use DirectIO if available")
			os.Exit(0)
		}
		if strings.HasPrefix(a, "--") && i+1 < len(args) {
			opts[a] = args[i+1]
		}
	}
	var mu sync.Mutex
	loaded := map[string]bool{}
	key := opts["--api-key"]
	send := func(w http.ResponseWriter, code int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { send(w, 200, map[string]any{"status": "ok"}) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			send(w, 401, map[string]any{})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/models":
			var data []any
			for _, p := range stagedIn(opts["--models-dir"], true) {
				id := ModelIDFromStem(pyPathStem(p))
				status := "unloaded"
				if loaded[id] {
					status = "loaded"
				}
				data = append(data, map[string]any{"id": id, "status": map[string]any{"value": status}})
			}
			send(w, 200, map[string]any{"data": data})
		case "/models/load":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			loaded[body["model"]] = true
			send(w, 200, map[string]any{"success": true})
		case "/v1/chat/completions":
			send(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "Paris"}}}})
		case "/slots":
			send(w, 200, []any{map[string]any{"id": 0, "is_processing": false}})
		case "/metrics":
			w.WriteHeader(200)
			_, _ = w.Write([]byte("llamacpp:requests_processing 0\n"))
		default:
			send(w, 404, map[string]any{})
		}
	})
	l, err := net.Listen("tcp", "127.0.0.1:"+opts["--port"])
	if err != nil {
		os.Exit(6)
	}
	_ = http.Serve(l, mux)
	os.Exit(7)
}

// Fake llama-server link to this test binary, spawned without an exec chain the spawn record could race
func lr5LlamaLink(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "engine", "llama-server")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// Serves as the fake llama-server when the fixture link starts this test binary under that name
func dispatchFakeLlamaServerEntry() {
	if filepath.Base(os.Args[0]) == "llama-server" {
		lr5FakeLlamaServer(os.Args[1:])
	}
}

func lr5ResetSupervisorSeams(t *testing.T) {
	t.Helper()
	prevSpawn, prevDIO, prevSpawnFn, prevHealth, prevReap, prevWait := supSpawnServer, supDirectIOArgs, supSpawnFn, supWaitHealthFn, supReapFn, supBackoffWait
	prevBackoff, prevPoll := lrRestartBackoff, supWatchPoll
	lrDirectIOMu.Lock()
	lrDirectIOCache = map[string][]string{}
	lrDirectIOMu.Unlock()
	t.Cleanup(func() {
		supSpawnServer, supDirectIOArgs, supSpawnFn, supWaitHealthFn, supReapFn, supBackoffWait = prevSpawn, prevDIO, prevSpawnFn, prevHealth, prevReap, prevWait
		lrRestartBackoff, supWatchPoll = prevBackoff, prevPoll
		lrDirectIOMu.Lock()
		lrDirectIOCache = map[string][]string{}
		lrDirectIOMu.Unlock()
	})
}

// Spawn double recording argv and env, then starting a disposable sleeper in place of the engine
func lr5RecordingSpawn(root string, argv *[][]string, envs *[][]string) func(*exec.Cmd, bool) (*WindowsJob, error) {
	return func(cmd *exec.Cmd, owned bool) (*WindowsJob, error) {
		*argv = append(*argv, append([]string{}, cmd.Args...))
		*envs = append(*envs, append([]string{}, cmd.Env...))
		helper := lr3HelperCmd(root, "sleeper")
		cmd.Path, cmd.Args, cmd.Env, cmd.Dir = helper.Path, helper.Args, helper.Env, ""
		return nil, cmd.Start()
	}
}

// Help printing engine script that logs every probe argv to a file
func lr5HelpScript(t *testing.T, help string, failFirst bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	exe, probes := filepath.Join(dir, "engine", "llama-server"), filepath.Join(dir, "probes.txt")
	fail := ""
	if failFirst {
		fail = fmt.Sprintf("if [ ! -e %s ]; then : > %s; exit 1; fi\n", strconv.Quote(dir+"/failed"), strconv.Quote(dir+"/failed"))
	}
	lr5Touch(t, exe, fmt.Sprintf("#!/bin/sh\necho \"$0 $*\" >> %s\n%sprintf '%%s\\n' %s\n", strconv.Quote(probes), fail, strconv.Quote(help)))
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, probes
}

func lr5Lines(path string) []string {
	raw, _ := os.ReadFile(path)
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(raw)), " ", "|"))
}

func TestLocalRuntimeSpawnSelectsSupportedDirectIOOption(t *testing.T) {
	for _, tc := range []struct {
		help     string
		expected []string
	}{{"-lm, --load-mode MODE\n  dio: use DirectIO if available", []string{"--load-mode", "dio"}},
		{"-dio, --direct-io  use DirectIO if available", []string{"-dio"}}} {
		lr5Home(t)
		lr5ResetSupervisorSeams(t)
		exe, probes := lr5HelpScript(t, tc.help, false)
		var argv, envs [][]string
		supSpawnServer = lr5RecordingSpawn(t.TempDir(), &argv, &envs)
		sup, err := NewLlamaServerSupervisor(exe, filepath.Join(t.TempDir(), "models"), 4, 19001, nil, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.spawn(); err != nil {
			t.Fatal(err)
		}
		_ = sup.Stop()
		cmd := argv[0]
		idx := -1
		for i, a := range cmd {
			if a == tc.expected[0] {
				idx = i
			}
		}
		if idx < 0 || !reflect.DeepEqual(cmd[idx:idx+len(tc.expected)], tc.expected) {
			t.Fatal(cmd)
		}
		if tc.expected[0] == "--load-mode" {
			for _, a := range cmd {
				if a == "-dio" {
					t.Fatal(cmd)
				}
			}
		}
		if got := lr5Lines(probes); !reflect.DeepEqual(got, []string{exe + "|--help"}) {
			t.Fatal(got)
		}
	}
}

func TestLocalRuntimeDirectIOProbeIsReusedForTheSameEngine(t *testing.T) {
	lr5ResetSupervisorSeams(t)
	exe, probes := lr5HelpScript(t, "--load-mode MODE", false)
	for i := 0; i < 2; i++ {
		if got, err := lrDirectIOArgs(exe); err != nil || !reflect.DeepEqual(got, []string{"--load-mode", "dio"}) {
			t.Fatal(got, err)
		}
	}
	if got := lr5Lines(probes); len(got) != 1 {
		t.Fatal(got)
	}
}

func TestLocalRuntimeFailedEngineProbeCanBeRetried(t *testing.T) {
	lr5ResetSupervisorSeams(t)
	exe, _ := lr5HelpScript(t, "--load-mode MODE", true)
	if _, err := lrDirectIOArgs(exe); err == nil || !strings.Contains(err.Error(), "returned non-zero exit status 1.") {
		t.Fatal(err)
	}
	if got, err := lrDirectIOArgs(exe); err != nil || !reflect.DeepEqual(got, []string{"--load-mode", "dio"}) {
		t.Fatal(got, err)
	}
}

func TestLocalRuntimeEngineWithoutDirectIOUsesItsSupportedDefaults(t *testing.T) {
	lr5ResetSupervisorSeams(t)
	exe, _ := lr5HelpScript(t, "--model FILE", false)
	if got, err := lrDirectIOArgs(exe); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

func TestLocalRuntimeSupervisorSpawnsLlamaServerWithoutCredentialsButKeepsRuntimeEnv(t *testing.T) {
	for k, v := range map[string]string{"FIRECRAWL_API_KEY": "fc-secret", "SOME_TOKEN": "t", "DB_PASSWORD": "p", "CUDA_VISIBLE_DEVICES": "0", "OMP_NUM_THREADS": "4"} {
		t.Setenv(k, v)
	}
	lr5Home(t)
	lr5ResetSupervisorSeams(t)
	supDirectIOArgs = func(string) ([]string, error) { return []string{}, nil }
	var argv, envs [][]string
	supSpawnServer = lr5RecordingSpawn(t.TempDir(), &argv, &envs)
	sup, err := NewLlamaServerSupervisor(filepath.Join(t.TempDir(), "engine", "llama-server"), filepath.Join(t.TempDir(), "models"), 4, 19002, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.spawn(); err != nil {
		t.Fatal(err)
	}
	_ = sup.Stop()
	env := lrEnvMap(envs[0])
	for _, k := range []string{"FIRECRAWL_API_KEY", "SOME_TOKEN", "DB_PASSWORD"} {
		if _, ok := env[k]; ok {
			t.Fatal(k)
		}
	}
	if env["CUDA_VISIBLE_DEVICES"] != "0" || env["OMP_NUM_THREADS"] != "4" || env["PATH"] != os.Getenv("PATH") {
		t.Fatal(env)
	}
}

func TestLocalRuntimeSpawnStateRecordsProcessIncarnations(t *testing.T) {
	lr5Home(t)
	root := t.TempDir()
	sup, err := NewLlamaServerSupervisor(root, root, 4, 59998, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := lr3HelperCmd(root, "sleeper")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sup.proc = lrWatchProcess(cmd)
	defer func() { _ = cmd.Process.Kill() }()
	if err := sup.writeState(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(localRuntimeStatePath())
	state, _ := moaJSONLoads(string(raw))
	m := state.(map[string]any)
	ps, _ := openPSProcess(cmd.Process.Pid)
	self, _ := openPSProcess(os.Getpid())
	created, _ := ps.CreateTime()
	exe, _ := ps.Exe()
	selfCreated, _ := self.CreateTime()
	if m["create_time"] != created || m["owner_pid"] != os.Getpid() || m["owner_create_time"] != selfCreated || m["executable"] != exe {
		t.Fatal(m, created, selfCreated, exe)
	}
	if sup.State()["pid"] != cmd.Process.Pid || sup.State()["owner_pid"] != os.Getpid() {
		t.Fatal(sup.State())
	}
	if !isModernLocalRuntimeState(m) || recordedProcess(m) == nil {
		t.Fatal("a spawn record must be modern and name its live router")
	}
}

func TestLocalRuntimeShutdownDuringBackoffCannotRestartOrRemoveAnotherServer(t *testing.T) {
	lr5Home(t)
	lr5ResetSupervisorSeams(t)
	root := t.TempDir()
	sup, err := NewLlamaServerSupervisor(root, root, 4, 59998, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	crashed := exec.Command("true")
	if err := crashed.Start(); err != nil {
		t.Fatal(err)
	}
	sup.proc = lrWatchProcess(crashed)
	<-sup.proc.done
	other := `{"pid": 202, "base_url": "http://127.0.0.1:59997/v1", "api_key": "test-only"}`
	lr5Touch(t, localRuntimeStatePath(), other)
	spawned := false
	supSpawnFn = func(*LlamaServerSupervisor) error { spawned = true; return nil }
	supWaitHealthFn = func(*LlamaServerSupervisor, time.Duration) error { return nil }
	supReapFn = func(*LlamaServerSupervisor) error { return nil }
	supBackoffWait = func(s *LlamaServerSupervisor, _ time.Duration) bool { _ = s.Stop(); return true }
	sup.watch()
	if spawned {
		t.Fatal("stop during restart backoff resurrected the runtime")
	}
	if raw, _ := os.ReadFile(localRuntimeStatePath()); string(raw) != other {
		t.Fatal(string(raw))
	}
}

func TestLocalRuntimeReplacementStopsOnlyAnOrphanedRouter(t *testing.T) {
	for _, owner := range []string{"exited", "alive"} {
		t.Run(owner, func(t *testing.T) {
			lr5Home(t)
			root := t.TempDir()
			holderCmd, routerCmd := lr3HelperCmd(root, "holder"), lr3HelperCmd(root, "router-sleeper")
			if err := holderCmd.Start(); err != nil {
				t.Fatal(err)
			}
			if err := routerCmd.Start(); err != nil {
				t.Fatal(err)
			}
			holder, router := lrWatchProcess(holderCmd), lrWatchProcess(routerCmd)
			defer func() { _ = holder.Kill(); _ = router.Kill() }()
			hp, _ := openPSProcess(holder.Pid())
			rp, _ := openPSProcess(router.Pid())
			hc, _ := hp.CreateTime()
			rc, _ := rp.CreateTime()
			rexe, _ := rp.Exe()
			state := lrObject{"base_url", "http://127.0.0.1:59997/v1", "api_key", "test-only", "pid", router.Pid(), "create_time", rc,
				"start_time", lrStartTimeValue(router.Pid()), "executable", rexe, "owner_pid", holder.Pid(), "owner_create_time", hc,
				"owner_start_time", lrStartTimeValue(holder.Pid())}
			lr5Touch(t, localRuntimeStatePath(), lrIndentedJSON(state))
			if owner == "exited" {
				_ = holder.Kill()
				_ = holder.Wait(5 * time.Second)
			}
			if err := stopStateServer(); err != nil {
				t.Fatal(err)
			}
			if owner == "exited" {
				if err := router.Wait(10 * time.Second); err != nil {
					t.Fatal("the orphaned router kept running")
				}
				if live, _ := bootStateEndpoint(context.Background()); live {
					t.Fatal("a stopped router still resolves")
				}
			} else if _, exited := router.Poll(); exited {
				t.Fatal("a live owner's router was stopped")
			}
		})
	}
}

func TestLocalRuntimeTheProcessThatBootsTheServerStopsItOnACleanExit(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocalRuntimeSupervisorHelper$", "-test.v=false")
	cmd.Env = append(os.Environ(), lr5RoleEnv+"=boot-exit", "ICEBATH_LRT_HELPER_ROOT="+root, "ICEBATH_HOME="+filepath.Join(root, "home"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	sleeper := lr3Read(t, filepath.Join(root, "sleeper.json"))
	if !lr3Wait(func() bool { return !lr3Alive(sleeper) }, 10*time.Second) {
		lr3Kill(sleeper)
		t.Fatal("the booting process exited and left its server running")
	}
}

// Starts this binary as a router in its own owned group that spawns a model child and then crashes
func lr5CrashedRouter(t *testing.T, root string) (*lrServerProc, lr3Identity) {
	t.Helper()
	cmd := lr3HelperCmd(root, "crashing-router")
	cmd.Env = append(cmd.Env, lr5RoleEnv+"=crashing-router")
	cmd.Args = []string{os.Args[0], "-test.run=^TestLocalRuntimeSupervisorHelper$", "-test.v=false"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tools.OwnProcessGroups([]int{cmd.Process.Pid})
	proc := lrWatchProcess(cmd)
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		tools.ReleaseProcessGroups([]int{cmd.Process.Pid})
	})
	if err := proc.Wait(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	model := lr3Read(t, filepath.Join(root, "grandchild.json"))
	if pgid, _ := syscall.Getpgid(model.PID); pgid != cmd.Process.Pid {
		t.Fatalf("model child left the router group: %d", pgid)
	}
	return proc, model
}

func TestLocalRuntimeAGracefulStopStopsTheRouter(t *testing.T) {
	lr5Home(t)
	root := t.TempDir()
	sup, err := NewLlamaServerSupervisor(root, root, 4, 59998, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := lr3HelperCmd(root, "sleeper")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tools.OwnProcessGroups([]int{cmd.Process.Pid})
	defer tools.ReleaseProcessGroups([]int{cmd.Process.Pid})
	sup.proc = lrWatchProcess(cmd)
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	if !lr3Wait(func() bool { _, exited := sup.proc.Poll(); return exited }, 20*time.Second) {
		_ = cmd.Process.Kill()
		t.Fatal("stop() left the router running")
	}
	if tools.ProcessGroupSupervised(cmd.Process.Pid) {
		t.Fatal("a router this process stopped itself stayed registered with the owner-death supervisor")
	}
}

func TestLocalRuntimeStopAfterARouterCrashStillStopsItsSurvivingModelChild(t *testing.T) {
	lr5Home(t)
	root := t.TempDir()
	sup, err := NewLlamaServerSupervisor(root, root, 4, 59997, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	proc, model := lr5CrashedRouter(t, root)
	sup.proc = proc
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	if !lr3Wait(func() bool { return !lr3Alive(model) }, 10*time.Second) {
		t.Fatal("stop() left the crashed router's surviving model child running")
	}
	if tools.ProcessGroupSupervised(proc.Pid()) {
		t.Fatal("the stopped router's group stayed registered with the owner-death supervisor")
	}
}

func TestLocalRuntimeACrashRestartStopsTheOldRoutersSurvivors(t *testing.T) {
	lr5Home(t)
	lr5ResetSupervisorSeams(t)
	root := t.TempDir()
	lrRestartBackoff = []time.Duration{0}
	sup, err := NewLlamaServerSupervisor(root, root, 4, 59996, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	supReapFn = func(*LlamaServerSupervisor) error { return nil }
	supWaitHealthFn = func(*LlamaServerSupervisor, time.Duration) error { return nil }
	supSpawnFn = func(s *LlamaServerSupervisor) error { s.mu.Lock(); s.proc = nil; s.mu.Unlock(); return nil }
	proc, model := lr5CrashedRouter(t, root)
	sup.proc = proc
	sup.watch()
	if !lr3Wait(func() bool { return !lr3Alive(model) }, 10*time.Second) {
		t.Fatal("the restart left the crashed router's surviving model child running")
	}
	if tools.ProcessGroupSupervised(proc.Pid()) {
		t.Fatal("the crashed router's group stayed registered after the restart")
	}
}

func TestLocalRuntimeACrashedRoutersEmptyGroupIsReleasedBeforeTheRestartBackoff(t *testing.T) {
	lr5Home(t)
	lr5ResetSupervisorSeams(t)
	root := t.TempDir()
	sup, err := NewLlamaServerSupervisor(root, root, 4, 59995, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tools.OwnProcessGroups([]int{cmd.Process.Pid})
	defer tools.ReleaseProcessGroups([]int{cmd.Process.Pid})
	proc := lrWatchProcess(cmd)
	if err := proc.Wait(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	var registeredDuringBackoff []bool
	supBackoffWait = func(*LlamaServerSupervisor, time.Duration) bool {
		registeredDuringBackoff = append(registeredDuringBackoff, tools.ProcessGroupSupervised(cmd.Process.Pid))
		return true
	}
	sup.proc = proc
	sup.watch()
	if len(registeredDuringBackoff) != 1 || registeredDuringBackoff[0] {
		t.Fatalf("the empty group stayed registered through the backoff: %v", registeredDuringBackoff)
	}
}

func TestLocalRuntimeManagedServerBootsServesRestartsAndStops(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	lr5ResetSupervisorSeams(t)
	script := lr5LlamaLink(t)
	lr5Touch(t, filepath.Join(localModelsDir(), "tiny-Q4_K_M.gguf"), "")
	bootInstalledEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b1", Binary: script}, nil
	}
	bootAdmittedModelsMax = func(_ string, configured int, _ string) int { return configured }
	bootGeneratePresets = func(string, string, int, string) string { return "" }
	bootSweeperSleep = func(time.Duration) { time.Sleep(20 * time.Millisecond) }
	lrRestartBackoff, supWatchPoll = []time.Duration{0}, 50*time.Millisecond
	port, err := lrFreePort()
	if err != nil {
		t.Fatal(err)
	}
	sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true, "port": port}}, false)
	if err != nil || sup == nil {
		t.Fatal(sup, err)
	}
	defer func() { _ = ShutdownLocalRuntime() }()
	if getLocalSupervisor() != sup || servingEngine() == nil || sup.Port != port {
		t.Fatal("boot did not publish its supervisor")
	}
	if live, _ := bootStateEndpoint(context.Background()); !live {
		t.Fatal("the spawn record does not resolve")
	}
	if ready, err := sup.EnsureModelReady("tiny-Q4_K_M", 60*time.Second); !ready || err != nil {
		t.Fatal(ready, err)
	}
	if !sup.IsIdle("") {
		t.Fatal("an idle router must report idle")
	}
	first := sup.currentProc()
	_ = first.Kill()
	sup.SetPrimaryModel("tiny-Q4_K_M")
	if !lr3Wait(func() bool { p := sup.currentProc(); return p != first && p.IsRunning() && sup.IsIdle("") }, 30*time.Second) {
		t.Fatal("the watchdog did not restart the crashed router")
	}
	last := sup.currentProc()
	if err := ShutdownLocalRuntime(); err != nil {
		t.Fatal(err)
	}
	if !lr3Wait(func() bool { _, exited := last.Poll(); return exited }, 20*time.Second) || getLocalSupervisor() != nil {
		t.Fatal("shutdown left the router running")
	}
	if !lrPathExists(localRuntimeStatePath()) {
		t.Fatal("stop removed the state file")
	}
}
