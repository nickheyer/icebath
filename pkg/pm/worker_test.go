package pm

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Worker child of the re-executed test binary, optionally with a probe replacing an operation
func ieWorkerHelper(args []string) int {
	if len(args) > 0 && args[0] == "register-regtool" {
		MustRegister(&ieFakeTool{BinaryPackage: ieBinary("regtool", "bin/regtool")})
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "stdio-probe") {
		streaming := args[0] == "stdio-probe-streaming"
		workerOperations["prune_cache"] = &workerOperation{bootstrap: bootstrapAlways, run: func(ctx context.Context, arguments map[string]any, hooks workerHooks) (any, error) {
			cmd := exec.Command("sh", "-c", "input=$(cat); [ -z \"$input\" ] && echo EOF_OK")
			cmd.Stdin = os.Stdin
			var captured bytes.Buffer
			cmd.Stdout = &captured
			if streaming {
				cmd.Stdout = io.MultiWriter(os.Stdout, &captured)
			}
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil || !strings.Contains(captured.String(), "EOF_OK") {
					return nil, fmt.Errorf("child saw stdin %q %v", captured.String(), err)
				}
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				return nil, errors.New("child stdin never reached EOF")
			}
			return "child completed", nil
		}}
	}
	return WorkerMain()
}

// Points the client's worker spawn at this test binary in worker mode
func ieUseTestWorker(t *testing.T, args ...string) {
	t.Helper()
	encoded, _ := json.Marshal(args)
	t.Setenv(ieHelperEnv, "worker")
	t.Setenv(ieHelperEnv+"_ARGS", string(encoded))
	previous := workerArgv
	workerArgv = func() ([]string, error) {
		return []string{os.Args[0], "-test.run=^TestIEHelperProcess$", "-test.count=1"}, nil
	}
	t.Cleanup(func() { workerArgv = previous })
}

// Pins the catalogue node package to a loopback zip whose binary is body and returns the target and relative path
func ieNodeArchive(t *testing.T, server *rangeServer, body []byte) (string, string) {
	t.Helper()
	target, _ := CurrentTarget()
	p, err := GetPackage("node")
	if err != nil {
		t.Fatal(err)
	}
	relative, _ := filepath.Rel(".", p.Binary(".", target))
	var payload bytes.Buffer
	zw := zip.NewWriter(&payload)
	header := &zip.FileHeader{Name: filepath.ToSlash(filepath.Join("node-package", relative)), Method: zip.Store}
	header.SetMode(0o755)
	w, _ := zw.CreateHeader(header)
	w.Write(body)
	zw.Close()
	server.set("/node.zip", payload.Bytes())
	lock := NewLockfile(LockfilePath())
	lock.SetPin("node", "1", map[string]any{target: map[string]any{"url": server.url("/node.zip"), "sha256": hexSHA256(payload.Bytes())}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	return target, relative
}

// Isolated home and store with a private lockfile, the worker client fixture
func ieClientEnv(t *testing.T) string {
	t.Helper()
	tmp := isolateHome(t)
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "tools"))
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	previous := lockfilePath
	lockfilePath = func() string { return lockPath }
	t.Cleanup(func() { lockfilePath = previous })
	return tmp
}

func TestWorkerIsolatedWorkerPreservesInstallError(t *testing.T) {
	ieClientEnv(t)
	ieUseTestWorker(t)
	ctx := context.Background()
	_, local := Ensure(ctx, "node", EnsureOptions{Explicit: true})
	_, wire := request(ctx, "ensure", map[string]any{"name": "node", "explicit": true}, workerHooks{}, "")
	for i, err := range []error{local, wire} {
		var ie *InstallError
		if !errors.As(err, &ie) || ie.Package != "node" || ie.Cause != "not in the lockfile" || ie.Remedy != "add it with `icebath pm lock --bump`" {
			t.Fatalf("route %d got %v", i, err)
		}
	}
	if factsPath, _ := FactsPath(ctx); pathExists(factsPath) {
		t.Fatal("the refusal wrote facts")
	}
}

func TestWorkerRefusedOrAlreadyPausedInstallDoesNotAcquireRuntime(t *testing.T) {
	ieClientEnv(t)
	previous := workerArgv
	workerArgv = func() ([]string, error) { t.Fatal("refusal started a worker"); return nil, nil }
	defer func() { workerArgv = previous }()
	t.Setenv(EnvDisableLazyInstalls, "1")
	_, err := Ensure(context.Background(), "node", EnsureOptions{})
	ieWantInstallError(t, err, "lazy installs are disabled")
	paused := NewPauseEvent()
	paused.Set()
	if _, err := Ensure(context.Background(), "node", EnsureOptions{Explicit: true, PauseEvent: paused}); !IsDownloadKind(err, DownloadWasPaused) {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerArtifactLifecycleKeepsIdentityAndRelaysProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX node fixture")
	}
	for _, operation := range []string{"ensure", "stage_only"} {
		t.Run(operation, func(t *testing.T) {
			ieClientEnv(t)
			ieUseTestWorker(t)
			ctx := context.Background()
			server := newRangeServer(t)
			body := []byte("#!/bin/sh\nexit 0\n")
			target, relative := ieNodeArchive(t, server, body)
			var stages [][]any
			var downloads []ieTick
			hooks := workerHooks{progress: func(stage string, done, total int64, label string) error {
				stages = append(stages, []any{stage, done, total, label})
				return nil
			}}
			arguments := map[string]any{"name": "node", "target": target}
			if operation == "ensure" {
				arguments = map[string]any{"name": "node", "explicit": true}
				hooks.downloadProgress = func(done, total int64, ranges []RangeSet) error {
					downloads = append(downloads, ieTick{done, total, ranges})
					return nil
				}
			}
			factsPath, _ := FactsPath(ctx)
			storeRoot, _ := StoreRoot(ctx)
			result, err := request(ctx, operation, arguments, hooks, "")
			if err != nil {
				t.Fatal(err)
			}
			var entry string
			if operation == "stage_only" {
				entry = pyStr(result)
				if pathExists(factsPath) {
					t.Fatal("staging published host identity")
				}
			} else {
				if result != nil {
					t.Fatalf("ensure crossed the wire with a result %v", result)
				}
				facts, _ := NewFacts(factsPath, false)
				fact, _ := facts.Get("node")
				entry = filepath.Join(storeRoot, pyStr(fact["entry"]))
				last := downloads[len(downloads)-1]
				if len(downloads) == 0 || last.done != last.total || len(last.ranges) == 0 {
					t.Fatalf("download progress %v", downloads)
				}
				base := map[string]string{"PATH": "/caller/bin", "CALLER": "kept", "PYTHONPATH": "/caller/dependencies"}
				runner, err := Ensure(ctx, "node", EnsureOptions{BaseEnv: base})
				if err != nil || runner.Env["CALLER"] != "kept" || runner.Env["PYTHONPATH"] != base["PYTHONPATH"] || !strings.HasSuffix(runner.Env["PATH"], base["PATH"]) {
					t.Fatalf("runner %v %v", runner, err)
				}
			}
			if !bytes.Equal(readFile(t, filepath.Join(entry, relative)), body) || len(stages) == 0 || stages[0][0] != "download" {
				t.Fatalf("stages %v", stages)
			}
			install := func() (string, error) {
				if operation == "stage_only" {
					out, err := request(ctx, "stage_only", map[string]any{"name": "node", "target": target}, workerHooks{}, "")
					return pyStr(out), err
				}
				_, err := request(ctx, "ensure", map[string]any{"name": "node", "explicit": true}, workerHooks{}, "")
				return entry, err
			}
			firstTree, _ := TreeDigest(entry)
			server.mu.Lock()
			server.payloads = map[string][]byte{}
			server.mu.Unlock()
			if again, err := install(); err != nil || again != entry {
				t.Fatalf("warm reinstall %s %v", again, err)
			}
			if tree, _ := TreeDigest(entry); tree != firstTree {
				t.Fatal("a warm reinstall changed the entry")
			}
			replacement := []byte("#!/bin/sh\nexit 0\n# repinned\n")
			ieNodeArchive(t, server, replacement)
			if again, err := install(); err != nil || again != entry || !bytes.Equal(readFile(t, filepath.Join(entry, relative)), replacement) {
				t.Fatalf("repin %v", err)
			}
			if matches, _ := filepath.Glob(filepath.Join(storeRoot, "fetch-*")); len(matches) > 0 {
				t.Fatalf("archives kept %v", matches)
			}
			goodTree, _ := TreeDigest(entry)
			previousFacts := ieMaybeRead(factsPath)
			server.set("/node.zip", []byte("not an archive"))
			lock := NewLockfile(LockfilePath())
			lock.SetPin("node", "1", map[string]any{target: map[string]any{"url": server.url("/node.zip"), "sha256": hexSHA256([]byte("not an archive"))}})
			lock.Save()
			if _, err := install(); err == nil || !strings.Contains(err.Error(), "zip") {
				t.Fatalf("broken archive got %v", err)
			}
			if tree, _ := TreeDigest(entry); tree != goodTree || !bytes.Equal(ieMaybeRead(factsPath), previousFacts) {
				t.Fatal("a failed repin changed the entry or facts")
			}
		})
	}
}

func TestWorkerPauseEventReachesRunningWorkerWithoutHanging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX node fixture")
	}
	for _, fromProgress := range []bool{false, true} {
		t.Run(fmt.Sprint(fromProgress), func(t *testing.T) {
			ieClientEnv(t)
			ieUseTestWorker(t)
			ctx := context.Background()
			server := newRangeServer(t)
			ieNodeArchive(t, server, append([]byte("#!/bin/sh\nexit 0\n#"), bytes.Repeat([]byte("x"), 8<<20)...))
			server.mu.Lock()
			server.slowPerChunk = 30 * time.Millisecond
			server.chunk = 64 << 10
			server.mu.Unlock()
			pause := NewPauseEvent()
			hooks := workerHooks{pause: pause}
			done := make(chan struct{})
			if fromProgress {
				hooks.progress = func(stage string, transferred, total int64, label string) error {
					if stage == "download" && transferred > 0 {
						pause.Set()
					}
					return nil
				}
			} else {
				go func() {
					defer close(done)
					deadline := time.Now().Add(10 * time.Second)
					for time.Now().Before(deadline) {
						for _, seen := range server.seen() {
							if seen.End > 1 {
								pause.Set()
								return
							}
						}
						time.Sleep(10 * time.Millisecond)
					}
					t.Error("worker never began transferring")
				}()
			}
			_, err := request(ctx, "ensure", map[string]any{"name": "node", "explicit": true}, hooks, "")
			if !IsDownloadKind(err, DownloadWasPaused) {
				t.Fatalf("got %v", err)
			}
			if !fromProgress {
				<-done
			}
			factsPath, _ := FactsPath(ctx)
			facts, _ := NewFacts(factsPath, false)
			if _, ok := facts.Get("node"); ok {
				t.Fatal("a paused install recorded facts")
			}
		})
	}
}

func TestWorkerInstalledToolNeedsNoWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX node fixture")
	}
	ieClientEnv(t)
	ctx := context.Background()
	server := newRangeServer(t)
	ieNodeArchive(t, server, []byte("#!/bin/sh\nexit 0\n"))
	if _, err := Ensure(ctx, "node", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	previous := workerArgv
	workerArgv = func() ([]string, error) { t.Fatal("hot path started a worker"); return nil, nil }
	defer func() { workerArgv = previous }()
	runner, err := Ensure(ctx, "node", EnsureOptions{BaseEnv: map[string]string{"PATH": "caller"}})
	if err != nil || !strings.HasSuffix(runner.Env["PATH"], "caller") {
		t.Fatalf("runner %v %v", runner, err)
	}
}

func TestWorkerInvalidArgumentsKeepTheEngineExceptionType(t *testing.T) {
	ieClientEnv(t)
	ieUseTestWorker(t)
	ctx := context.Background()
	err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{}, Repair: true})
	if errorClass(err) != "ValueError" || !strings.Contains(err.Error(), "repair restores") {
		t.Fatalf("local got %v", err)
	}
	_, err = request(ctx, "sync_venv", map[string]any{"extras": []any{}, "repair": true}, workerHooks{}, "")
	if errorClass(err) != "ValueError" || !strings.Contains(err.Error(), "repair restores") {
		t.Fatalf("wire got %v", err)
	}
	if _, err := Ensure(ctx, "no-such-package", EnsureOptions{Explicit: true}); errorClass(err) != "KeyError" {
		t.Fatalf("got %v", err)
	}
	if _, err := request(ctx, "ensure", map[string]any{"name": "node", "explicit": true, "bogus": 1}, workerHooks{}, ""); errorClass(err) != "TypeError" ||
		err.Error() != "ensure() got an unexpected keyword argument 'bogus'" {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerDeathReportsTransportFailure(t *testing.T) {
	ieClientEnv(t)
	previous := workerArgv
	workerArgv = func() ([]string, error) { return []string{"sh", "-c", "exit 7"}, nil }
	defer func() { workerArgv = previous }()
	_, err := request(context.Background(), "ensure", map[string]any{"name": "node", "explicit": true}, workerHooks{}, "")
	ieWantInstallError(t, err, "worker exited without a result")
}

func TestWorkerUnknownOperationIsNotDispatched(t *testing.T) {
	for _, operation := range []string{"activate", "stage_manager_runtime"} {
		for _, route := range []string{"client", "wire"} {
			t.Run(operation+"-"+route, func(t *testing.T) {
				tmp := ieClientEnv(t)
				arguments := map[string]any{"destination": filepath.Join(tmp, "unused")}
				ctx := context.Background()
				if route == "client" {
					previous := workerArgv
					workerArgv = func() ([]string, error) { t.Fatal("unsupported operation started a worker"); return nil, nil }
					defer func() { workerArgv = previous }()
					_, err := request(ctx, operation, arguments, workerHooks{}, "")
					if errorClass(err) != "KeyError" || !strings.Contains(err.Error(), operation) {
						t.Fatalf("got %v", err)
					}
				} else {
					ieUseTestWorker(t)
					response := ieRawWorker(t, map[string]any{"id": "unsupported", "operation": operation, "arguments": arguments,
						"callbacks": []any{}, "packages": []any{}, "context": map[string]any{"repo": RepoRoot(), "lockfile": LockfilePath()}}, false)
					payload, _ := response["error"].(map[string]any)
					if payload["type"] != "KeyError" || !strings.Contains(pyStr(payload["message"]), operation) {
						t.Fatalf("response %v", response)
					}
				}
				if factsPath, _ := FactsPath(ctx); pathExists(factsPath) {
					t.Fatal("an unknown operation wrote facts")
				}
			})
		}
	}
}

// Sends one raw request line to a test worker child and returns its result response, keeping stdin open when asked
func ieRawWorker(t *testing.T, message map[string]any, holdStdin bool) map[string]any {
	t.Helper()
	argv, _ := workerArgv()
	cmd := exec.Command(argv[0], argv[1:]...)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(message)
	stdin.Write(append(line, '\n'))
	if !holdStdin {
		defer stdin.Close()
	}
	reader := bufio.NewReader(stdout)
	result := make(chan map[string]any, 1)
	go func() {
		for {
			raw, err := reader.ReadBytes('\n')
			if err != nil {
				result <- nil
				return
			}
			var response map[string]any
			json.Unmarshal(raw, &response)
			if response["type"] == "result" {
				result <- response
				return
			}
		}
	}()
	select {
	case response := <-result:
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("worker exited %v: %s", err, diagnostics.String())
			}
		case <-time.After(30 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("worker never exited with stdin open: %s", diagnostics.String())
		}
		stdin.Close()
		if response == nil {
			t.Fatalf("no result: %s", diagnostics.String())
		}
		return response
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("worker hung: %s", diagnostics.String())
	}
	return nil
}

func TestWorkerChildrenGetEOFWhileControlPipeStaysOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell probe")
	}
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			tmp := ieClientEnv(t)
			mode := "stdio-probe"
			if streaming {
				mode = "stdio-probe-streaming"
			}
			ieUseTestWorker(t, mode)
			response := ieRawWorker(t, map[string]any{"id": "stdio-probe", "operation": "prune_cache", "arguments": map[string]any{"cache": tmp},
				"callbacks": []any{}, "packages": []any{}, "context": map[string]any{"repo": RepoRoot(), "lockfile": LockfilePath()}}, true)
			if response["result"] != "child completed" {
				t.Fatalf("response %v", response)
			}
		})
	}
}

func TestWorkerReceiptCrossesTheBoundaryByCorrelation(t *testing.T) {
	ieClientEnv(t)
	ctx := WithReceiptScope(context.Background())
	id := "lazy-disabled-sync"
	restore := WorkerContext(ctx, &id)
	defer restore()
	t.Setenv(EnvDisableLazyInstalls, "1")
	err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{}, Plugins: &Members{Dirs: []MemberDir{}}})
	ie := ieWantInstallError(t, err, "lazy installs are disabled")
	if ie.Package != "pm-runtime" {
		t.Fatalf("refusal %+v", ie)
	}
	result := LastForUpdate(ctx, id, true)
	if result == nil || result["outcome"] != "failed" {
		t.Fatalf("receipt %v", result)
	}
	receipts, _ := filepath.Glob(filepath.Join(receiptDir(ctx), "pm_*.json"))
	if len(receipts) != 1 {
		t.Fatalf("receipts %v", receipts)
	}
	if factsPath, _ := FactsPath(ctx); pathExists(factsPath) {
		t.Fatal("a refused sync wrote facts")
	}
}

func TestWorkerSlowObserverDoesNotHoldRangeWriters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX node fixture")
	}
	ieClientEnv(t)
	server := newRangeServer(t)
	ieNodeArchive(t, server, append([]byte("#!/bin/sh\nexit 0\n#"), bytes.Repeat([]byte("x"), 64<<20)...))
	var dones []int64
	var advanced []bool
	var last ieTick
	observe := func(done, total int64, ranges []RangeSet) error {
		dones = append(dones, done)
		last = ieTick{done, total, ranges}
		if done == 0 || len(advanced) > 0 {
			return nil
		}
		parts, _ := filepath.Glob(filepath.Join(PartialsRoot(), "*.part"))
		if len(parts) != 1 {
			t.Fatalf("parts %v", parts)
		}
		position := 7*total/8 + (1 << 20)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if f, err := os.Open(parts[0]); err == nil {
				b := make([]byte, 1)
				f.ReadAt(b, position)
				f.Close()
				if b[0] == 'x' {
					advanced = append(advanced, true)
					return nil
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		advanced = append(advanced, false)
		return nil
	}
	if _, err := Ensure(context.Background(), "node", EnsureOptions{Explicit: true, DownloadProgress: observe}); err != nil {
		t.Fatal(err)
	}
	if len(advanced) != 1 || !advanced[0] {
		t.Fatal("range writers stalled until the progress callback returned")
	}
	for i := 1; i < len(dones); i++ {
		if dones[i] < dones[i-1] {
			t.Fatalf("progress went backwards %v", dones)
		}
	}
	if last.done != last.total {
		t.Fatalf("last %v", last)
	}
}

func TestWorkerObserverFailurePreservesResumableBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX node fixture")
	}
	for _, action := range []string{"pause", "error"} {
		t.Run(action, func(t *testing.T) {
			ieClientEnv(t)
			ctx := context.Background()
			server := newRangeServer(t)
			ieNodeArchive(t, server, append([]byte("#!/bin/sh\nexit 0\n#"), bytes.Repeat([]byte("x"), 32<<20)...))
			pause := NewPauseEvent()
			failure := &PyError{Class: "ValueError", Message: "observer failed"}
			var seen []int64
			observe := func(done, total int64, ranges []RangeSet) error {
				seen = append(seen, done)
				if done > 0 {
					if action == "pause" {
						pause.Set()
					} else {
						return failure
					}
				}
				return nil
			}
			_, err := Ensure(ctx, "node", EnsureOptions{Explicit: true, PauseEvent: pause, DownloadProgress: observe})
			if action == "pause" && !IsDownloadKind(err, DownloadWasPaused) || action == "error" && err != error(failure) {
				t.Fatalf("got %v", err)
			}
			factsPath, _ := FactsPath(ctx)
			facts, _ := NewFacts(factsPath, false)
			if _, ok := facts.Get("node"); ok {
				t.Fatal("an interrupted install recorded facts")
			}
			sidecars, _ := filepath.Glob(filepath.Join(PartialsRoot(), "*.ranges"))
			if len(sidecars) != 1 {
				t.Fatalf("sidecars %v", sidecars)
			}
			var record struct{ Ranges [][2]int64 }
			json.Unmarshal(readFile(t, sidecars[0]), &record)
			var durable int64
			for _, r := range record.Ranges {
				durable += r[1] - r[0]
			}
			if durable < seen[len(seen)-1] || durable == 0 {
				t.Fatalf("durable %d after %v", durable, seen)
			}
			pause.Clear()
			var resumed []int64
			var resumedLast ieTick
			if _, err := Ensure(ctx, "node", EnsureOptions{Explicit: true, PauseEvent: pause, DownloadProgress: func(done, total int64, ranges []RangeSet) error {
				resumed = append(resumed, done)
				resumedLast = ieTick{done, total, ranges}
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			if resumed[0] != durable || resumedLast.done != resumedLast.total {
				t.Fatalf("resume %v from %d", resumed, durable)
			}
			if parts, _ := filepath.Glob(filepath.Join(PartialsRoot(), "*.part")); len(parts) > 0 {
				t.Fatalf("parts left %v", parts)
			}
			facts, _ = NewFacts(factsPath, false)
			if _, ok := facts.Get("node"); !ok {
				t.Fatal("the resumed install recorded no facts")
			}
		})
	}
}

func TestWorkerColdBootstrapNeverCreatesAPrivateCache(t *testing.T) {
	tmp := ieClientEnv(t)
	cache := filepath.Join(tmp, "shared-cache")
	before := os.Environ()
	err := PruneCache(context.Background(), PruneCacheOptions{Cache: cache})
	if err == nil {
		t.Fatal("prune ran without a pinned uv")
	}
	if pathExists(filepath.Join(home.Root(), "cache", "uv")) {
		t.Fatal("the facade bootstrapped an unshared private cache")
	}
	if strings.Join(os.Environ(), "\n") != strings.Join(before, "\n") {
		t.Fatal("the facade changed the process environment")
	}
}

func TestWorkerRegisteredPackageInstallsArchiveInRealWorker(t *testing.T) {
	for _, operation := range []string{"ensure", "stage_only"} {
		t.Run(operation, func(t *testing.T) {
			tmp := ieClientEnv(t)
			ieUseTestWorker(t, "register-regtool")
			ctx := context.Background()
			e := &ieEnv{tmp: tmp, docroot: filepath.Join(tmp, "www"), lockPath: LockfilePath()}
			server := newRangeServer(t)
			digest := e.tar(t, "regtool-1.0.tar.gz", map[string]string{"bin/regtool": "#!/bin/sh\nexit 0\n"})
			server.set("/regtool-1.0.tar.gz", readFile(t, filepath.Join(e.docroot, "regtool-1.0.tar.gz")))
			lock := NewLockfile(LockfilePath())
			lock.SetPin("regtool", "1.0", map[string]any{"any": map[string]any{"url": server.url("/regtool-1.0.tar.gz"), "sha256": digest}})
			lock.Save()
			ieSwapRegistry(t, &ieFakeTool{BinaryPackage: ieBinary("regtool", "bin/regtool")})
			target, _ := CurrentTarget()
			arguments := map[string]any{"name": "regtool", "explicit": true}
			if operation == "stage_only" {
				target = "linux-arm64-bionic"
				arguments = map[string]any{"name": "regtool", "target": target}
			}
			if _, err := request(ctx, operation, arguments, workerHooks{}, ""); err != nil {
				t.Fatal(err)
			}
			root, _ := StoreRoot(ctx)
			if !isRegularFile(filepath.Join(root, "regtool-1.0-"+target, "bin", "regtool")) {
				t.Fatal("the worker did not realize the registered package")
			}
		})
	}
}

func TestWorkerDefinitionTheBinaryDoesNotCarryIsAPackageError(t *testing.T) {
	for _, definition := range []PackageDefinition{
		{Name: "node", Module: "example.com/plugins/collision", Qualname: "Other"},
		{Name: "no-such-tool", Module: "example.com/plugins/tool", Qualname: "Tool"},
	} {
		t.Run(definition.Name, func(t *testing.T) {
			ieClientEnv(t)
			ieUseTestWorker(t)
			response := ieRawWorker(t, map[string]any{"id": "definitions", "operation": "ensure", "arguments": map[string]any{"name": "node"},
				"callbacks": []any{}, "packages": []any{map[string]any{"name": definition.Name, "module": definition.Module, "qualname": definition.Qualname}},
				"context": map[string]any{"repo": RepoRoot(), "lockfile": LockfilePath()}}, false)
			payload, _ := response["error"].(map[string]any)
			if payload["type"] != "InstallError" || payload["package"] != definition.Name ||
				!strings.Contains(pyStr(payload["cause"]), "cannot load package definition") || !strings.Contains(pyStr(payload["remedy"]), "keep the package definition") {
				t.Fatalf("response %v", response)
			}
		})
	}
}
