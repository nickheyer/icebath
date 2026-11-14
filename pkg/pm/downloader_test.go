package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Runs one download as a separate process when the test binary is re-executed as a helper child
func init() {
	if os.Getenv("DLT_HELPER") != "download" {
		return
	}
	if ms := os.Getenv("DLT_CHECKPOINT_MS"); ms != "" {
		n, err := strconv.Atoi(ms)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		checkpointInterval = time.Duration(n) * time.Millisecond
	}
	source := Source{URL: os.Getenv("DLT_URL"), Dest: os.Getenv("DLT_DEST"), SHA256: os.Getenv("DLT_SHA")}
	_, err := NewDownload([]Source{source}, DownloadOptions{PartialsDir: os.Getenv("DLT_PARTIALS"), Connections: 1}).Run(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Child download process with its combined output
type dltChild struct {
	cmd    *exec.Cmd
	output bytes.Buffer
}

// Starts this test binary as a child process that downloads one source with one connection
func dltStartChild(t *testing.T, rawURL, dest, partials, sha string, checkpoint time.Duration) *dltChild {
	t.Helper()
	child := &dltChild{cmd: exec.Command(os.Args[0], "-test.run=^$")}
	child.cmd.Env = append(os.Environ(), "DLT_HELPER=download", "DLT_URL="+rawURL, "DLT_DEST="+dest, "DLT_PARTIALS="+partials, "DLT_SHA="+sha)
	if checkpoint > 0 {
		child.cmd.Env = append(child.cmd.Env, fmt.Sprintf("DLT_CHECKPOINT_MS=%d", checkpoint.Milliseconds()))
	}
	child.cmd.Stdout = &child.output
	child.cmd.Stderr = &child.output
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return child
}

// Waits for the child, killing it once the timeout passes, and returns its exit code and output
func (c *dltChild) finish(timeout time.Duration) (int, string) {
	done := make(chan struct{})
	go func() {
		c.cmd.Wait()
		close(done)
	}()
	if !dltWaitClosed(done, timeout) {
		c.cmd.Process.Kill()
		<-done
	}
	return c.cmd.ProcessState.ExitCode(), c.output.String()
}

// Replaces a package variable for the test and restores it when the test ends
func dltSwap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// Applies a configuration change to the range server under its lock
func dltConfigure(s *rangeServer, change func(*rangeServer)) {
	s.mu.Lock()
	change(s)
	s.mu.Unlock()
}

// Serves a request with the range server's default behaviour from inside an intercept
func dltServeDefault(s *rangeServer, w http.ResponseWriter, r *http.Request) {
	payloads, abortAfter, slow, noRange, etags, chunk, _ := s.config()
	s.serve(w, r, payloads, abortAfter, slow, noRange, etags, chunk)
}

// Sends an error status with its standard reason the way send_error does
func dltSendError(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

var dltRangePattern = regexp.MustCompile(`^bytes=(\d+)-(\d+)$`)

// Inclusive bounds of a request's byte range header
func dltBounds(t *testing.T, r *http.Request) (int64, int64) {
	m := dltRangePattern.FindStringSubmatch(r.Header.Get("Range"))
	if m == nil {
		t.Errorf("request without a byte range: %q", r.Header.Get("Range"))
		return 0, 0
	}
	start, _ := strconv.ParseInt(m[1], 10, 64)
	end, _ := strconv.ParseInt(m[2], 10, 64)
	return start, end
}

// Seed bytes repeated n times
func dltPayload(n int, seed string) []byte { return bytes.Repeat([]byte(seed), n) }

// Every byte value in order, the Python bytes of range 256
func dltBytes256() []byte {
	out := make([]byte, 256)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

// Coverage report keyed by destination
func dltRanges(sets []RangeSet) map[string][]Range {
	out := map[string][]Range{}
	for _, rs := range sets {
		out[rs.Key] = rs.Ranges
	}
	return out
}

// Sum of covered bytes over every destination of a report
func dltCovered(sets []RangeSet) int64 {
	var n int64
	for _, rs := range sets {
		n += coveredBytes(rs.Ranges)
	}
	return n
}

// One progress callback invocation
type dltTick struct {
	Done, Total int64
	Ranges      []RangeSet
}

// Progress callback that records every tick
func dltRecorder(ticks *[]dltTick) ProgressFunc {
	return func(done, total int64, ranges []RangeSet) error {
		*ticks = append(*ticks, dltTick{done, total, ranges})
		return nil
	}
}

// Names in a directory other than the lock directory
func dltNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != ".locks" {
			names = append(names, e.Name())
		}
	}
	return names
}

// Reports whether any name carries one of the suffixes
func dltAnySuffix(names []string, suffix string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, suffix) })
}

// Reports whether a path exists
func dltExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports whether a path is a regular file
func dltIsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Polls a condition until it holds or the timeout passes
func dltWaitUntil(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Waits for a channel to close within the timeout
func dltWaitClosed(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Keeps every computed retry delay but waits a thousandth of it, pause still interrupting
func dltFastBackoff(t *testing.T) {
	original := downloadWaitRetry
	dltSwap(t, &downloadWaitRetry, func(d *Download, delay time.Duration) error { return original(d, delay/1000) })
}

// Retry delays a download asked to wait
type dltWaits struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (w *dltWaits) add(d time.Duration) {
	w.mu.Lock()
	w.delays = append(w.delays, d)
	w.mu.Unlock()
}

func (w *dltWaits) list() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.delays)
}

// Replaces every download backoff with a recorder that returns at once
func dltRecordWaits(t *testing.T) *dltWaits {
	waits := &dltWaits{}
	dltSwap(t, &downloadWaitRetry, func(_ *Download, delay time.Duration) error { waits.add(delay); return nil })
	return waits
}

// Thread safe list of strings a handler records
type dltLog struct {
	mu    sync.Mutex
	items []string
}

func (l *dltLog) add(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, s)
	return len(l.items)
}

func (l *dltLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.items)
}

// Slog handler that keeps every record for assertions
type dltLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *dltLogs) Enabled(context.Context, slog.Level) bool { return true }

func (l *dltLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.records = append(l.records, r)
	l.mu.Unlock()
	return nil
}

func (l *dltLogs) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *dltLogs) WithGroup(string) slog.Handler { return l }

// Reports whether a record at the level carries the message fragment
func (l *dltLogs) has(level slog.Level, fragment string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.records, func(r slog.Record) bool { return r.Level == level && strings.Contains(r.Message, fragment) })
}

// Routes the default logger into a recorder until the test ends
func dltCaptureLogs(t *testing.T) *dltLogs {
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	logs := &dltLogs{}
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return logs
}

// Barrier of a fixed party count that breaks for every waiter once one waiter times out
type dltBarrier struct {
	mu      sync.Mutex
	parties int
	arrived int
	done    chan struct{}
	broken  chan struct{}
	once    sync.Once
}

func newDltBarrier(parties int) *dltBarrier {
	return &dltBarrier{parties: parties, done: make(chan struct{}), broken: make(chan struct{})}
}

// Waits for every party and reports false when the barrier broke
func (b *dltBarrier) wait(timeout time.Duration) bool {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.parties {
		close(b.done)
	}
	b.mu.Unlock()
	select {
	case <-b.broken:
		return false
	default:
	}
	select {
	case <-b.done:
		return true
	case <-b.broken:
		return false
	case <-time.After(timeout):
		b.once.Do(func() { close(b.broken) })
		return false
	}
}

func TestParallelRequestsOverlapWithoutExceedingLimit(t *testing.T) {
	for _, connections := range []int{2, 8} {
		t.Run(strconv.Itoa(connections), func(t *testing.T) {
			tmp := t.TempDir()
			total := int64(8 * (4 << 20))
			payload := dltPayload(int(total), "x")
			barrier := newDltBarrier(connections)
			var mu sync.Mutex
			active, peak := 0, 0
			var broken []string
			srv := newRangeServer(t)
			srv.set("/big", payload)
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Header.Get("Range") == "bytes=0-0" {
						return false
					}
					mu.Lock()
					active++
					peak = max(peak, active)
					mu.Unlock()
					defer func() {
						mu.Lock()
						active--
						mu.Unlock()
					}()
					if !barrier.wait(5 * time.Second) {
						mu.Lock()
						broken = append(broken, r.Header.Get("Range"))
						mu.Unlock()
					}
					dltServeDefault(srv, w, r)
					return true
				}
			})
			dest := filepath.Join(tmp, "big.bin")
			_, err := NewDownload([]Source{{URL: srv.url("/big"), Dest: dest, SHA256: hexSHA256(payload)}},
				DownloadOptions{Connections: connections, PartialsDir: filepath.Join(tmp, "partials")}).Run(nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, dest), payload) {
				t.Fatal("destination bytes differ from the payload")
			}
			mu.Lock()
			if len(broken) > 0 {
				t.Errorf("range requests were serialized instead of overlapping: %v", broken)
			}
			mu.Unlock()
			if !dltWaitUntil(func() bool { mu.Lock(); defer mu.Unlock(); return active == 0 }, 5*time.Second) {
				t.Error("a range handler never finished")
			}
			mu.Lock()
			if peak != connections {
				t.Errorf("peak %d, want %d", peak, connections)
			}
			mu.Unlock()
			var expected, got [][2]int64
			for i := int64(0); i < int64(connections); i++ {
				expected = append(expected, [2]int64{i * total / int64(connections), (i+1)*total/int64(connections) - 1})
			}
			for _, r := range srv.seen() {
				got = append(got, [2]int64{r.Start, r.End})
			}
			slices.SortFunc(got, func(a, b [2]int64) int { return cmpInt64(a[0], b[0]) })
			if !slices.Equal(got, expected) {
				t.Errorf("ranges %v, want %v", got, expected)
			}
		})
	}
}

func TestProgressCarriesOverallAndRanges(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	p1, p2 := dltPayload(1000, "x"), dltPayload(2000, "y")
	srv.set("/a", p1)
	srv.set("/b", p2)
	d1, d2 := filepath.Join(tmp, "a.bin"), filepath.Join(tmp, "b.bin")
	dl := NewDownload([]Source{
		{URL: srv.url("/a"), Dest: d1, SHA256: hexSHA256(p1)},
		{URL: srv.url("/b"), Dest: d2, SHA256: hexSHA256(p2)},
	}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")})
	var seen []dltTick
	if _, err := dl.Run(dltRecorder(&seen)); err != nil {
		t.Fatal(err)
	}
	final := seen[len(seen)-1]
	if final.Done != 3000 || final.Total != 3000 {
		t.Fatalf("final tick %d of %d", final.Done, final.Total)
	}
	ranges := dltRanges(final.Ranges)
	if !slices.Equal(ranges[d1], []Range{{0, 1000}}) || !slices.Equal(ranges[d2], []Range{{0, 2000}}) {
		t.Fatalf("final ranges %v", ranges)
	}
}

func TestHashMismatchRaisesAndDeletesPart(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	srv.set("/f", dltPayload(1<<20, "x"))
	dest := filepath.Join(tmp, "f.bin")
	partials := filepath.Join(tmp, "partials")
	_, err := NewDownload([]Source{{URL: srv.url("/f"), Dest: dest, SHA256: strings.Repeat("0", 64)}},
		DownloadOptions{PartialsDir: partials}).Run(nil)
	if !IsDownloadKind(err, DownloadHashFailed) {
		t.Fatalf("want a hash error, got %v", err)
	}
	if dltExists(dest) {
		t.Fatal("destination published despite the hash mismatch")
	}
	if names := dltNames(t, partials); len(names) > 0 {
		t.Fatalf("partials left behind: %v", names)
	}
}

func TestNoHashAcceptsAnyBytesOfRightSize(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	payload := dltPayload(1<<20, "x")
	srv.set("/f", payload)
	dest := filepath.Join(tmp, "f.bin")
	moved, err := NewDownload([]Source{{URL: srv.url("/f"), Dest: dest}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(moved, []string{dest}) || !bytes.Equal(readFile(t, dest), payload) {
		t.Fatalf("moved %v", moved)
	}
}

func TestPartialsNeverInScratchOrDest(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	srv.set("/p", dltPayload(1<<20, "x"))
	dest := filepath.Join(tmp, "p.bin")
	partials := filepath.Join(tmp, "partials")
	if _, err := NewDownload([]Source{{URL: srv.url("/p"), Dest: dest}}, DownloadOptions{PartialsDir: partials}).Run(nil); err != nil {
		t.Fatal(err)
	}
	if !dltExists(dest) {
		t.Fatal("destination missing")
	}
	if names := dltNames(t, partials); len(names) > 0 {
		t.Fatalf("partials left behind: %v", names)
	}
	for _, name := range dltNames(t, tmp) {
		if ext := filepath.Ext(name); ext == ".part" || ext == ".ranges" {
			t.Fatalf("partial %s beside the destination", name)
		}
	}
}

func TestCompletedDestIsSkipped(t *testing.T) {
	for _, hashed := range []bool{false, true} {
		t.Run(strconv.FormatBool(hashed), func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := dltPayload(1<<20, "x")
			srv.set("/s", payload)
			dest := filepath.Join(tmp, "s.bin")
			writeFile(t, dest, payload, 0o666)
			sha := ""
			if hashed {
				sha = hexSHA256(payload)
			}
			if _, err := NewDownload([]Source{{URL: srv.url("/s"), Dest: dest, SHA256: sha}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil); err != nil {
				t.Fatal(err)
			}
			if seen := srv.seen(); len(seen) > 0 {
				t.Fatalf("completed destination fetched again: %v", seen)
			}
		})
	}
}

func TestStaleDestWithWrongHashIsRefetched(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	payload := dltPayload(1<<20, "x")
	srv.set("/s", payload)
	dest := filepath.Join(tmp, "s.bin")
	writeFile(t, dest, dltPayload(1<<20, "wrong"), 0o666)
	if _, err := NewDownload([]Source{{URL: srv.url("/s"), Dest: dest, SHA256: hexSHA256(payload)}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, dest), payload) {
		t.Fatal("stale destination was not replaced")
	}
	if len(srv.seen()) == 0 {
		t.Fatal("no fetch happened")
	}
}

func TestRedirectToNonHTTPSRefused(t *testing.T) {
	request := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	redirect := func(from, to string) error {
		return httpsRedirectPolicy(request(to), []*http.Request{request(from)})
	}
	if err := redirect("https://example.com/a", "http://example.com/b"); !isDownloadError(err) {
		t.Fatalf("plain http redirect allowed: %v", err)
	}
	if err := redirect("https://example.com/a", "https://example.com/b"); err != nil {
		t.Fatalf("https redirect refused: %v", err)
	}
	if err := redirect("https://example.com/a", "http://127.0.0.1:8000/b"); !isDownloadError(err) {
		t.Fatalf("https origin landed on loopback: %v", err)
	}
	if err := redirect("http://127.0.0.1:8000/a", "http://localhost:8000/b"); err != nil {
		t.Fatalf("loopback to loopback refused: %v", err)
	}
}

func TestRefusesNonHTTPSNonLoopback(t *testing.T) {
	tmp := t.TempDir()
	_, err := NewDownload([]Source{{URL: "http://example.com/x", Dest: filepath.Join(tmp, "x.bin")}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil)
	if err == nil || isDownloadError(err) || err.Error() != "refusing non-https url: http://example.com/x" {
		t.Fatalf("want the value error refusing plain http, got %v", err)
	}
}

func TestNoRangeFallbackDownloadsFullBody(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	dltConfigure(srv, func(s *rangeServer) { s.noRange = true })
	payload := dltPayload(1<<20, "x")
	srv.set("/nr", payload)
	dest := filepath.Join(tmp, "nr.bin")
	var seen []dltTick
	moved, err := NewDownload([]Source{{URL: srv.url("/nr"), Dest: dest, SHA256: hexSHA256(payload)}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(dltRecorder(&seen))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(moved, []string{dest}) || !bytes.Equal(readFile(t, dest), payload) {
		t.Fatalf("moved %v", moved)
	}
	if ranges := srv.seen(); len(ranges) > 0 {
		t.Fatalf("Range used against a server that ignores it: %v", ranges)
	}
	last := seen[len(seen)-1]
	if last.Done != int64(len(payload)) || last.Total != int64(len(payload)) {
		t.Fatalf("last tick %d of %d", last.Done, last.Total)
	}
	if got := dltRanges(last.Ranges)[dest]; !slices.Equal(got, []Range{{0, int64(len(payload))}}) {
		t.Fatalf("last ranges %v", got)
	}
}

func TestNoRangeShortBodyRaisesAndLeavesNoDest(t *testing.T) {
	dltFastBackoff(t)
	tmp := t.TempDir()
	srv := newRangeServer(t)
	dltConfigure(srv, func(s *rangeServer) {
		s.noRange = true
		s.abortAfter = 1 << 20
	})
	srv.set("/ns", dltPayload(2<<20, "x"))
	dest := filepath.Join(tmp, "ns.bin")
	partials := filepath.Join(tmp, "partials")
	_, err := NewDownload([]Source{{URL: srv.url("/ns"), Dest: dest}}, DownloadOptions{PartialsDir: partials}).Run(nil)
	if !isDownloadError(err) {
		t.Fatalf("want a download error, got %v", err)
	}
	if dltExists(dest) {
		t.Fatal("short body published")
	}
	names := dltNames(t, partials)
	if !dltAnySuffix(names, ".part") || !dltAnySuffix(names, ".ranges") {
		t.Fatalf("partial not kept: %v", names)
	}
}

func TestResumeAcrossPlanSkipsCompletedSource(t *testing.T) {
	dltFastBackoff(t)
	tmp := t.TempDir()
	srv := newRangeServer(t)
	totalB := int64(8 << 20)
	pa, pb := dltPayload(1<<20, "a"), dltPayload(int(totalB), "b")
	srv.set("/a", pa)
	srv.set("/b", pb)
	da, db := filepath.Join(tmp, "a.bin"), filepath.Join(tmp, "b.bin")
	partials := filepath.Join(tmp, "partials")
	plan := []Source{{URL: srv.url("/a"), Dest: da, SHA256: hexSHA256(pa)}, {URL: srv.url("/b"), Dest: db, SHA256: hexSHA256(pb)}}

	dltConfigure(srv, func(s *rangeServer) { s.abortAfter = 2 << 20 })
	if _, err := NewDownload(plan, DownloadOptions{PartialsDir: partials, Connections: 1}).Run(nil); err == nil {
		t.Fatal("aborted transfer reported success")
	}
	sidecars, _ := filepath.Glob(filepath.Join(partials, "*.ranges"))
	if len(sidecars) != 1 {
		t.Fatalf("sidecars %v", sidecars)
	}
	var record struct{ Ranges [][2]int64 }
	if err := json.Unmarshal(readFile(t, sidecars[0]), &record); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(record.Ranges, [][2]int64{{0, 2 << 20}}) {
		t.Fatalf("sidecar ranges %v", record.Ranges)
	}
	if !bytes.Equal(readFile(t, da), pa) || dltExists(db) {
		t.Fatal("first source not published or second source published early")
	}

	dltConfigure(srv, func(s *rangeServer) { s.abortAfter = -1 })
	var ticks []dltTick
	if _, err := NewDownload(plan, DownloadOptions{PartialsDir: partials, Connections: 1}).Run(dltRecorder(&ticks)); err != nil {
		t.Fatal(err)
	}
	first := ticks[0]
	if first.Done != int64(len(pa))+(2<<20) || first.Total != int64(len(pa)+len(pb)) {
		t.Fatalf("first tick %d of %d", first.Done, first.Total)
	}
	ranges := dltRanges(first.Ranges)
	if len(ranges) != 2 || !slices.Equal(ranges[da], []Range{{0, int64(len(pa))}}) || !slices.Equal(ranges[db], []Range{{0, 2 << 20}}) {
		t.Fatalf("first tick ranges %v", ranges)
	}
	if left, _ := filepath.Glob(filepath.Join(partials, "*.ranges")); len(left) > 0 {
		t.Fatalf("sidecars left %v", left)
	}
	if left, _ := filepath.Glob(filepath.Join(partials, "*.part")); len(left) > 0 {
		t.Fatalf("parts left %v", left)
	}
	if !bytes.Equal(readFile(t, da), pa) || !bytes.Equal(readFile(t, db), pb) {
		t.Fatal("destinations differ from their payloads")
	}
	var aReqs, bReqs []rangeSeen
	for _, r := range srv.seen() {
		switch r.Path {
		case "/a":
			aReqs = append(aReqs, r)
		case "/b":
			bReqs = append(bReqs, r)
		}
	}
	if len(aReqs) != 1 {
		t.Fatalf("source a fetched %d times", len(aReqs))
	}
	if len(bReqs) < 2 || bReqs[0].Start != 0 || bReqs[1].Start != 2<<20 {
		t.Fatalf("source b requests %v", bReqs)
	}
}

func TestPauseMidPlanResumesToCompletion(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	pa, pb := dltPayload(1<<20, "a"), dltPayload(32<<20, "b")
	srv.set("/a", pa)
	srv.set("/b", pb)
	da, db := filepath.Join(tmp, "a.bin"), filepath.Join(tmp, "b.bin")
	partials := filepath.Join(tmp, "partials")
	plan := []Source{{URL: srv.url("/a"), Dest: da, SHA256: hexSHA256(pa)}, {URL: srv.url("/b"), Dest: db, SHA256: hexSHA256(pb)}}
	dl := NewDownload(plan, DownloadOptions{PartialsDir: partials})
	_, err := dl.Run(func(done, total int64, ranges []RangeSet) error {
		if len(dltRanges(ranges)[db]) > 0 {
			dl.Pause()
		}
		return nil
	})
	if !IsDownloadKind(err, DownloadWasPaused) {
		t.Fatalf("want a pause, got %v", err)
	}
	if !bytes.Equal(readFile(t, da), pa) || dltExists(db) {
		t.Fatal("first source not published or paused source published")
	}
	names := dltNames(t, partials)
	if !dltAnySuffix(names, ".part") || !dltAnySuffix(names, ".ranges") {
		t.Fatalf("paused partial not kept: %v", names)
	}
	dltConfigure(srv, func(s *rangeServer) { s.slowPerChunk = 0 })
	if _, err := NewDownload(plan, DownloadOptions{PartialsDir: partials}).Run(nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, da), pa) || !bytes.Equal(readFile(t, db), pb) {
		t.Fatal("resumed plan did not complete")
	}
}

// Rename that refuses the first two publications of a staged download with an access error
func dltHoldFirstTwo(refusals *[]string) func(string, string) error {
	return func(src, dst string) error {
		if strings.HasSuffix(src, ".download") && len(*refusals) < 2 {
			*refusals = append(*refusals, src)
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EACCES}
		}
		return os.Rename(src, dst)
	}
}

func TestReplaceWaitsOutATransientHold(t *testing.T) {
	tmp := t.TempDir()
	staged, dest := filepath.Join(tmp, "model.download"), filepath.Join(tmp, "model.gguf")
	writeFile(t, staged, []byte("weights"), 0o666)
	var refusals []string
	dltSwap(t, &renameFile, dltHoldFirstTwo(&refusals))
	if err := ReplaceWhenReleased(staged, dest, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 2 || string(readFile(t, dest)) != "weights" || dltExists(staged) {
		t.Fatalf("refusals %d", len(refusals))
	}
}

func TestReplaceGivesUpWithAPlainLanguageError(t *testing.T) {
	tmp := t.TempDir()
	staged, dest := filepath.Join(tmp, "model.download"), filepath.Join(tmp, "model.gguf")
	writeFile(t, staged, []byte("weights"), 0o666)
	dltSwap(t, &renameFile, func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EACCES}
	})
	err := ReplaceWhenReleased(staged, dest, 300*time.Millisecond)
	if err == nil {
		t.Fatal("a hold that never releases was published")
	}
	if !strings.Contains(err.Error(), "model.download") || !strings.Contains(strings.ToLower(err.Error()), "try again") {
		t.Fatalf("message %q", err)
	}
	if !errors.Is(errors.Unwrap(err), fs.ErrPermission) {
		t.Fatalf("cause %v is not the permission error", errors.Unwrap(err))
	}
	if dltExists(dest) || string(readFile(t, staged)) != "weights" {
		t.Fatal("finished bytes not kept for the retry")
	}
}

func TestDownloadPublishesThroughAHeldFinishedFile(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	body := dltPayload(4<<20, "m")
	srv.set("/held", body)
	dest := filepath.Join(tmp, "held.gguf")
	var refusals []string
	dltSwap(t, &renameFile, dltHoldFirstTwo(&refusals))
	partials := filepath.Join(tmp, "partials")
	if _, err := NewDownload([]Source{{URL: srv.url("/held"), Dest: dest, SHA256: hexSHA256(body)}}, DownloadOptions{PartialsDir: partials}).Run(nil); err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 2 || !bytes.Equal(readFile(t, dest), body) {
		t.Fatalf("refusals %d", len(refusals))
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "*.download")); len(left) > 0 {
		t.Fatalf("staged files left %v", left)
	}
	for _, name := range dltNames(t, partials) {
		if ext := filepath.Ext(name); ext == ".part" || ext == ".ranges" {
			t.Fatalf("partial %s left", name)
		}
	}
}

func TestDownloadReportsAHoldThatNeverReleases(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	body := dltPayload(1<<20, "h")
	srv.set("/stuck", body)
	dest := filepath.Join(tmp, "stuck.gguf")
	partials := filepath.Join(tmp, "partials")
	t.Cleanup(func() { os.Chmod(partials, 0o755) })
	dltSwap(t, &ReplaceWhenReleased, func(staged, target string, timeout time.Duration) error {
		if err := os.Chmod(partials, 0o555); err != nil {
			return err
		}
		return fmt.Errorf("%s could not be renamed into place", filepath.Base(staged))
	})
	_, err := NewDownload([]Source{{URL: srv.url("/stuck"), Dest: dest, SHA256: hexSHA256(body)}}, DownloadOptions{PartialsDir: partials}).Run(nil)
	if err == nil || !strings.Contains(err.Error(), "could not be renamed into place") {
		t.Fatalf("want the rename error, got %v", err)
	}
	if dltExists(dest) {
		t.Fatal("destination published")
	}
}

func TestSwapRenameFilePublishesThroughTheReplacementAndRestores(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	payload := dltPayload(64, "y")
	srv.set("/f", payload)
	var renames []string
	prev := SwapRenameFile(func(oldpath, newpath string) error {
		renames = append(renames, filepath.Base(newpath))
		return os.Rename(oldpath, newpath)
	})
	dest := filepath.Join(tmp, "f.bin")
	_, err := NewDownload([]Source{{URL: srv.url("/f"), Dest: dest}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil)
	restored := SwapRenameFile(prev)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(renames, "f.bin") || renames[len(renames)-1] != "f.bin" || !bytes.Equal(readFile(t, dest), payload) {
		t.Fatalf("renames %v", renames)
	}
	if restored == nil || fmt.Sprintf("%p", renameFile) != fmt.Sprintf("%p", prev) {
		t.Fatal("previous rename not restored")
	}
}

func TestSwapDownloadWaitRetryTakesEveryBackoffAndRestores(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	srv.set("/f", []byte("payload"))
	var failuresMu sync.Mutex
	failures := 0
	dltConfigure(srv, func(s *rangeServer) {
		s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
			failuresMu.Lock()
			defer failuresMu.Unlock()
			if failures < 2 {
				failures++
				dltSendError(w, 503)
				return true
			}
			return false
		}
	})
	var waits []time.Duration
	prev := SwapDownloadWaitRetry(func(_ *Download, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	})
	_, err := NewDownload([]Source{{URL: srv.url("/f"), Dest: filepath.Join(tmp, "f")}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials"), Connections: 1}).Run(nil)
	SwapDownloadWaitRetry(prev)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(waits, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("waits %v", waits)
	}
}
