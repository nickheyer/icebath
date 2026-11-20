package codeexec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Contract faithful run to completion backend answering each command from a handler table
type scriptedEnv struct {
	*shellEnv
	mu       sync.Mutex
	handlers []scriptHandler
	commands []string
	stdins   []string
}

type scriptHandler struct {
	needle string
	fn     func(cmd string) (*tools.ExecResult, error)
}

func newScriptedEnv(t *testing.T, handlers ...scriptHandler) *scriptedEnv {
	return &scriptedEnv{shellEnv: newShellEnv(t, t.TempDir()), handlers: handlers}
}

func (s *scriptedEnv) Execute(_ context.Context, command string, opts tools.ExecOptions) (*tools.ExecResult, error) {
	s.mu.Lock()
	s.commands = append(s.commands, command)
	stdin := ""
	if opts.Stdin != nil {
		stdin = *opts.Stdin
	}
	s.stdins = append(s.stdins, stdin)
	handlers := append([]scriptHandler(nil), s.handlers...)
	s.mu.Unlock()
	for _, h := range handlers {
		if strings.Contains(command, h.needle) {
			return h.fn(command)
		}
	}
	return &tools.ExecResult{}, nil
}

func (s *scriptedEnv) TempDir() string { return "/tmp" }

func (s *scriptedEnv) setHandlers(h ...scriptHandler) {
	s.mu.Lock()
	s.handlers = h
	s.mu.Unlock()
}

func (s *scriptedEnv) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *scriptedEnv) count(needle string) int {
	n := 0
	for _, c := range s.seen() {
		if strings.Contains(c, needle) {
			n++
		}
	}
	return n
}

func reply(out string, code int) func(string) (*tools.ExecResult, error) {
	return func(string) (*tools.ExecResult, error) { return &tools.ExecResult{Output: out, ReturnCode: code}, nil }
}

// Healthy kernel handlers, spawn answering a PID, liveness ALIVE and each result cat the next canned payload
func spawnOKHandlers(cells ...map[string]any) []scriptHandler {
	var mu sync.Mutex
	pending := append([]map[string]any(nil), cells...)
	cat := func(string) (*tools.ExecResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(pending) == 0 {
			return &tools.ExecResult{}, nil
		}
		body, _ := json.Marshal(pending[0])
		pending = pending[1:]
		return &tools.ExecResult{Output: string(body)}, nil
	}
	return []scriptHandler{{"nohup", reply("PID:4242\n", 0)}, {"kill -0", reply("ALIVE\n", 0)}, {"cat ", cat}}
}

func cellPayload(status, stdout string, count int) map[string]any {
	return map[string]any{"id": "000001", "status": status, "stdout": stdout, "stderr": "", "stdout_clipped": false,
		"stderr_clipped": false, "traceback": "", "execution_count": count}
}

// Patches the file ship and RPC poll loop away and starts from an empty remote registry
func remoteKernelEnv(t *testing.T) *tools.Env {
	t.Helper()
	env := testEnv(t)
	ShutdownAllRemoteKernels()
	ship, poll := shipFileToRemote, remotePollLoop
	shipFileToRemote = func(context.Context, tools.Environment, string, string, bool) error { return nil }
	remotePollLoop = func(context.Context, *tools.Env, tools.Environment, string, string, *rpcBudget, <-chan struct{}, string, dispatchFunc) {
	}
	t.Cleanup(func() {
		shipFileToRemote, remotePollLoop = ship, poll
		ShutdownAllRemoteKernels()
	})
	return env
}

type remoteOpts struct {
	task    string
	reset   bool
	timeout float64
	tools   []string
}

func runRemote(t *testing.T, ctx context.Context, env *tools.Env, envr tools.Environment, o remoteOpts) (*pyjson.Object, error) {
	t.Helper()
	if o.task == "" {
		o.task = "t1"
	}
	if o.timeout == 0 {
		o.timeout = 10
	}
	if o.tools == nil {
		o.tools = []string{"read_file"}
	}
	return executeInRemoteKernel(ctx, env, envr, "print(1)", remoteCell{envType: "ssh", taskEnvID: o.task,
		sandboxTools: o.tools, timeout: o.timeout, maxToolCalls: 5, reset: o.reset, idleExit: big.NewInt(1800)})
}

func mustRemote(t *testing.T, ctx context.Context, env *tools.Env, envr tools.Environment, o remoteOpts) *pyjson.Object {
	t.Helper()
	result, err := runRemote(t, ctx, env, envr, o)
	if err != nil || result == nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	return result
}

func remoteInfo(t *testing.T, result *pyjson.Object) *pyjson.Object {
	t.Helper()
	info, _ := objGet(result, "kernel", nil).(*pyjson.Object)
	if info == nil {
		t.Fatalf("no kernel info in %v", result)
	}
	return info
}

func remoteKeys() []remoteKernelKey {
	remoteRegistry.mu.Lock()
	defer remoteRegistry.mu.Unlock()
	out := make([]remoteKernelKey, 0, len(remoteRegistry.kernels))
	for k := range remoteRegistry.kernels {
		out = append(out, k)
	}
	return out
}

func remoteOwners() map[string]bool {
	owners := map[string]bool{}
	for _, k := range remoteKeys() {
		owners[k.owner] = true
	}
	return owners
}

func TestRemoteFirstCallSpawnsSecondReuses(t *testing.T) {
	env := remoteKernelEnv(t)
	healthy := spawnOKHandlers(cellPayload("ok", "one\n", 1), cellPayload("ok", "two\n", 2))
	envr := newScriptedEnv(t, scriptHandler{"", func(cmd string) (*tools.ExecResult, error) {
		if strings.HasPrefix(cmd, "rm -f") && strings.Contains(cmd, "cell_res_") {
			return nil, errors.New("ssh dropped after the cell ran")
		}
		for _, h := range healthy {
			if strings.Contains(cmd, h.needle) {
				return h.fn(cmd)
			}
		}
		return &tools.ExecResult{}, nil
	}})
	first := mustRemote(t, t.Context(), env, envr, remoteOpts{})
	if objGet(first, "status", nil) != "success" || objGet(remoteInfo(t, first), "reused", nil) != false {
		t.Fatalf("first call %v", first)
	}
	second := mustRemote(t, t.Context(), env, envr, remoteOpts{})
	info := remoteInfo(t, second)
	if objGet(info, "reused", nil) != true || fmt.Sprint(objGet(info, "execution_count", nil)) != "2" {
		t.Fatalf("second call %v", second)
	}
	if n := envr.count("nohup"); n != 1 {
		t.Fatalf("spawned %d times", n)
	}
}

func TestRemoteSpawnFailureFailsOpen(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, scriptHandler{"nohup", reply("sh: cannot fork\n", 1)})
	if result, err := runRemote(t, t.Context(), env, envr, remoteOpts{}); result != nil || err != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if len(remoteKeys()) != 0 {
		t.Fatal("failed spawn left a registry entry")
	}
}

func TestRemoteResetKillsAndRespawns(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "", 1), cellPayload("ok", "", 1))...)
	mustRemote(t, t.Context(), env, envr, remoteOpts{})
	info := remoteInfo(t, mustRemote(t, t.Context(), env, envr, remoteOpts{reset: true}))
	if objGet(info, "state_reset", nil) != true || objGet(info, "reused", nil) != false {
		t.Fatalf("reset info %v", info)
	}
	if n := envr.count("nohup"); n != 2 {
		t.Fatalf("spawned %d times", n)
	}
}

func TestRemoteDeadKernelIsReportedAndRespawned(t *testing.T) {
	env := remoteKernelEnv(t)
	original := spawnOKHandlers(cellPayload("ok", "", 1), cellPayload("ok", "", 1))
	envr := newScriptedEnv(t, original...)
	mustRemote(t, t.Context(), env, envr, remoteOpts{})
	probes := 0
	flaky := func(string) (*tools.ExecResult, error) {
		probes++
		if probes == 1 {
			return &tools.ExecResult{ReturnCode: 1}, nil
		}
		return &tools.ExecResult{Output: "ALIVE\n"}, nil
	}
	envr.setHandlers(scriptHandler{"kill -0", flaky}, original[0], original[2])
	result := mustRemote(t, t.Context(), env, envr, remoteOpts{})
	info := remoteInfo(t, result)
	note, _ := objGet(info, "note", "").(string)
	if objGet(result, "status", nil) != "success" || objGet(info, "state_lost", nil) != true ||
		!strings.Contains(note, "state from earlier calls was lost") {
		t.Fatalf("respawn result %v", result)
	}
}

func TestRemoteCellTimeoutKillsKernelAndReports(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, scriptHandler{"nohup", reply("PID:77\n", 0)}, scriptHandler{"kill -0", reply("ALIVE\n", 0)},
		scriptHandler{"cat ", reply("", 0)})
	result := mustRemote(t, t.Context(), env, envr, remoteOpts{timeout: 2})
	if objGet(result, "status", nil) != "timeout" || objGet(remoteInfo(t, result), "state_lost", nil) != true {
		t.Fatalf("timeout result %v", result)
	}
	if len(remoteKeys()) != 0 {
		t.Fatal("timed out kernel still registered")
	}
	if envr.count("kill ") == 0 {
		t.Fatal("kernel was not killed on the remote")
	}
}

func TestRemoteChangedToolSetSpawnsKernelWithFreshStubs(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "", 1), cellPayload("ok", "", 1))...)
	mustRemote(t, t.Context(), env, envr, remoteOpts{tools: []string{"read_file"}})
	mustRemote(t, t.Context(), env, envr, remoteOpts{tools: []string{"web_search"}})
	keys := remoteKeys()
	if len(keys) != 2 || envr.count("nohup") != 2 {
		t.Fatalf("keys %v", keys)
	}
	sets := map[string]bool{}
	for _, k := range keys {
		sets[k.tools] = true
	}
	if !sets["read_file"] || !sets["web_search"] {
		t.Fatalf("tool sets %v", sets)
	}
}

func TestRemoteDelegatedChildrenGetTheirOwnKernels(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "", 1), cellPayload("ok", "", 1))...)
	mustRemote(t, t.Context(), env, envr, remoteOpts{task: "conv"})
	childCtx, child := childScope(env, "child-9")
	mustRemote(t, childCtx, child, envr, remoteOpts{task: "conv"})
	if len(remoteKeys()) != 2 || envr.count("nohup") != 2 {
		t.Fatalf("keys %v", remoteKeys())
	}
}

func TestRemoteOwnerDisposalReapsOnlyThatOwner(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "", 1), cellPayload("ok", "", 1))...)
	mustRemote(t, t.Context(), env, envr, remoteOpts{task: "owner-a"})
	mustRemote(t, t.Context(), env, envr, remoteOpts{task: "owner-b"})
	if len(remoteKeys()) != 2 {
		t.Fatal("want two kernels")
	}
	shutdownRemoteKernelsForOwner("owner-a")
	if keys := remoteKeys(); len(keys) != 1 || keys[0].owner != "owner-b" {
		t.Fatalf("remaining %v", keys)
	}
}

func TestRemoteIdleExpiredKernelIsReapedOnNextCall(t *testing.T) {
	env := remoteKernelEnv(t)
	envr := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "", 1), cellPayload("ok", "", 1))...)
	mustRemote(t, t.Context(), env, envr, remoteOpts{task: "stale", tools: []string{}})
	if len(remoteKeys()) != 1 {
		t.Fatal("want one kernel")
	}
	remoteRegistry.mu.Lock()
	for _, k := range remoteRegistry.kernels {
		k.lastUsed = k.lastUsed.Add(-2000 * time.Second)
	}
	remoteRegistry.mu.Unlock()
	mustRemote(t, t.Context(), env, envr, remoteOpts{task: "fresh", tools: []string{}})
	if owners := remoteOwners(); owners["stale"] || !owners["fresh"] {
		t.Fatalf("owners %v", owners)
	}
}

func withLifecycleLimits(t *testing.T, limit, idle int) {
	prev := lifecycleLimits
	lifecycleLimits = func(*tools.Env) (int, int) { return limit, idle }
	t.Cleanup(func() { lifecycleLimits = prev })
}

func TestRemoteOverCapEvictsLeastRecentlyUsed(t *testing.T) {
	env := remoteKernelEnv(t)
	withLifecycleLimits(t, 2, 1800)
	var cells []map[string]any
	for range 10 {
		cells = append(cells, cellPayload("ok", "", 1))
	}
	envr := newScriptedEnv(t, spawnOKHandlers(cells...)...)
	for _, owner := range []string{"owner-0", "owner-1", "owner-2"} {
		mustRemote(t, t.Context(), env, envr, remoteOpts{task: owner, tools: []string{}})
	}
	if owners := remoteOwners(); len(owners) != 2 || owners["owner-0"] || !owners["owner-1"] || !owners["owner-2"] {
		t.Fatalf("owners %v", owners)
	}
}

func TestRemoteEvictionSkipsKernelsWithARunningCell(t *testing.T) {
	env := remoteKernelEnv(t)
	withLifecycleLimits(t, 1, 1800)
	gate := make(chan struct{})
	slowCat := func(string) (*tools.ExecResult, error) {
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
		body, _ := json.Marshal(cellPayload("ok", "", 1))
		return &tools.ExecResult{Output: string(body)}, nil
	}
	busy := newScriptedEnv(t, scriptHandler{"nohup", reply("PID:4242\n", 0)}, scriptHandler{"kill -0", reply("ALIVE\n", 0)},
		scriptHandler{"cat ", slowCat})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runRemote(t, context.Background(), env, busy, remoteOpts{task: "busy"})
	}()
	for {
		remoteRegistry.mu.Lock()
		attached := false
		for _, k := range remoteRegistry.kernels {
			attached = attached || k.attached > 0
		}
		remoteRegistry.mu.Unlock()
		if attached {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	settled := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "", 1))...)
	mustRemote(t, t.Context(), env, settled, remoteOpts{task: "settled"})
	if !remoteOwners()["busy"] {
		t.Fatal("busy kernel was evicted mid cell")
	}
	close(gate)
	<-done
	if busy.count("kill 4242") != 0 {
		t.Fatal("busy kernel was killed mid cell")
	}
}

// Writes a code_execution config section into the test home
func writeCodeExecConfig(t *testing.T, env *tools.Env, section map[string]any) {
	t.Helper()
	writeKernelConfig(t, env, section)
}

func TestExecuteRemoteUsesKernelResult(t *testing.T) {
	env := testEnv(t)
	writeCodeExecConfig(t, env, map[string]any{"timeout": 30, "max_tool_calls": 5})
	envr := newScriptedEnv(t, scriptHandler{"command -v python3", reply("OK\n", 0)})
	withRemoteEntry(t, envr, func(context.Context, *tools.Env, tools.Environment, string, remoteCell) (*pyjson.Object, error) {
		info := pyjson.NewObject()
		info.Set("reused", true)
		info.Set("remote", true)
		info.Set("execution_count", 3)
		fake := pyjson.NewObject()
		for _, kv := range [][2]any{{"status", "success"}, {"stdout", "kernel says hi\n"}, {"stderr", ""}, {"traceback", ""}, {"tool_calls_made", 0}, {"kernel", info}} {
			fake.Set(kv[0].(string), kv[1])
		}
		return fake, nil
	})
	result := decodeResult(t, executeRemote(t.Context(), env, "print()", "t", []string{"read_file"}, false))
	if result["status"] != "success" || !strings.Contains(outputOf(result), "kernel says hi") || kernelField(result, "execution_count") != float64(3) {
		t.Fatalf("result %v", result)
	}
}

// Routes the remote path to a fixed backend and kernel entry for one test
func withRemoteEntry(t *testing.T, envr tools.Environment, entry func(context.Context, *tools.Env, tools.Environment, string, remoteCell) (*pyjson.Object, error)) {
	t.Helper()
	prevEnv, prevEntry := getOrCreateEnv, executeInRemoteKernelFn
	getOrCreateEnv = func(context.Context, *tools.Env, string) (tools.Environment, string, error) { return envr, "ssh", nil }
	executeInRemoteKernelFn = entry
	t.Cleanup(func() { getOrCreateEnv, executeInRemoteKernelFn = prevEnv, prevEntry })
}

func TestExecuteRemoteFallsOpenToPerCall(t *testing.T) {
	env := remoteKernelEnv(t)
	writeCodeExecConfig(t, env, map[string]any{"timeout": 30, "max_tool_calls": 5})
	envr := newScriptedEnv(t, scriptHandler{"command -v python3", reply("OK\n", 0)}, scriptHandler{"python3 script.py", reply("per-call ran\n", 0)})
	withRemoteEntry(t, envr, func(context.Context, *tools.Env, tools.Environment, string, remoteCell) (*pyjson.Object, error) {
		return nil, nil
	})
	result := decodeResult(t, executeRemote(t.Context(), env, "print()", "t", []string{"read_file"}, false))
	if result["status"] != "success" || !strings.Contains(outputOf(result), "per-call ran") {
		t.Fatalf("result %v", result)
	}
}

func TestRemoteSpawnLocksDownDirsAndHidesToken(t *testing.T) {
	env := remoteKernelEnv(t)
	shipFileToRemote = shipFile
	envr := newScriptedEnv(t, spawnOKHandlers(cellPayload("ok", "hi\n", 1))...)
	result := mustRemote(t, t.Context(), env, envr, remoteOpts{})
	if objGet(result, "status", nil) != "success" {
		t.Fatalf("result %v", result)
	}
	remoteRegistry.mu.Lock()
	var kernel *remoteKernel
	for _, k := range remoteRegistry.kernels {
		kernel = k
	}
	remoteRegistry.mu.Unlock()
	commands := envr.seen()
	var spawnCmd, mkdirCmd string
	for _, c := range commands {
		if strings.Contains(c, kernel.rpcToken) {
			t.Fatal("rpc token appeared in a remote command line")
		}
		if spawnCmd == "" && strings.Contains(c, "nohup") {
			spawnCmd = c
		}
		if mkdirCmd == "" && strings.Contains(c, "mkdir -p") {
			mkdirCmd = c
		}
	}
	if strings.Contains(spawnCmd, "ICEBATH_RPC_TOKEN=") {
		t.Fatal("token rides the spawn command")
	}
	root := t.TempDir()
	local := root + kernel.kernelDir
	if out, err := exec.Command("bash", "-c", strings.ReplaceAll(mkdirCmd, kernel.kernelDir, local)).CombinedOutput(); err != nil {
		t.Fatalf("dir setup replay failed: %v %s", err, out)
	}
	for _, d := range []string{local, local + "/cells", local + "/rpc"} {
		st, err := os.Stat(d)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v err %v", d, st, err)
		}
	}
	envShip := ""
	envr.mu.Lock()
	for i, c := range envr.commands {
		if strings.Contains(c, "base64 -d") && strings.Contains(c, "kernel.env") && envr.stdins[i] != "" {
			envShip = envr.stdins[i]
		}
	}
	envr.mu.Unlock()
	decoded, err := base64.StdEncoding.DecodeString(envShip)
	if err != nil || !strings.Contains(string(decoded), "ICEBATH_RPC_TOKEN="+kernel.rpcToken) {
		t.Fatalf("env file %q err %v", decoded, err)
	}
	envr.setHandlers(append([]scriptHandler{{"cell_req_", reply("ENOSPC", 1)}}, envr.handlers...)...)
	if _, err := runRemote(t, t.Context(), env, envr, remoteOpts{timeout: 1}); err == nil || !strings.Contains(err.Error(), "ENOSPC") {
		t.Fatalf("failed cell ship err %v", err)
	}
	if len(remoteKeys()) != 0 {
		t.Fatal("kernel that missed a cell stayed registered")
	}
	blocked := newScriptedEnv(t, append([]scriptHandler{{"mkdir -p", reply("EACCES", 1)}}, spawnOKHandlers(cellPayload("ok", "", 1))...)...)
	if result, err := runRemote(t, t.Context(), env, blocked, remoteOpts{}); result != nil || err != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if blocked.count("base64 -d") != 0 {
		t.Fatalf("shipped into an unlocked dir: %v", blocked.seen())
	}
}
