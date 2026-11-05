package lsp

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Write latency budget and the verdict cache cap
const (
	WriteBudget  = 80 * time.Millisecond
	InlineGrace  = 80 * time.Millisecond
	verdictCap   = 256
	spawnCeiling = 60 * time.Second
)

const editCheckLogger = "agent.lsp.edit_check"

// Note is one undelivered diagnostics note with the pre edit text its verdict was diffed against
type Note struct {
	Text         string
	BaselineText *string
}

// CheckResult is one post write verdict, HasVerdict false meaning no verdict in budget
type CheckResult struct {
	Diagnostics  []map[string]any
	HasVerdict   bool
	BaselineText *string
	Superseded   bool
	Carry        []Note
}

// One running check of a file
type inflightCheck struct {
	done         chan struct{}
	result       *CheckResult
	err          error
	cancelled    bool
	baselineText *string
	carry        []Note
	superseded   bool
}

// Key of the newest unfinished check one session runs on one file
type ownerPath struct{ owner, path string }

// Cached last verdict of a file keyed by the digest of the text it judged
type verdictEntry struct {
	path   string
	digest string
	diags  []map[string]any
}

// Loop side state of the edit checks of one service
type editChecks struct {
	verdicts      *list.List
	verdictsIndex map[string]*list.Element
	tail          map[string]*inflightCheck
	inflight      map[ownerPath]*inflightCheck
	newestMu      sync.Mutex
	newest        map[string]*string
}

// Newest scheduled content of a file, nil when unknown
func (e *editChecks) newestFor(absPath string) *string {
	e.newestMu.Lock()
	defer e.newestMu.Unlock()
	return e.newest[absPath]
}

func newEditChecks() *editChecks {
	return &editChecks{verdicts: list.New(), verdictsIndex: map[string]*list.Element{},
		tail: map[string]*inflightCheck{}, inflight: map[ownerPath]*inflightCheck{}, newest: map[string]*string{}}
}

// Records the verdict on a text as the newest entry, evicting the oldest past the cap
func (e *editChecks) remember(absPath, text string, diags []map[string]any) {
	if el, ok := e.verdictsIndex[absPath]; ok {
		e.verdicts.Remove(el)
	}
	e.verdictsIndex[absPath] = e.verdicts.PushBack(&verdictEntry{absPath, digestText(text), append([]map[string]any{}, diags...)})
	for e.verdicts.Len() > verdictCap {
		front := e.verdicts.Front()
		delete(e.verdictsIndex, front.Value.(*verdictEntry).path)
		e.verdicts.Remove(front)
	}
}

// Cached verdict on exactly this text, false when the cache holds another
func (e *editChecks) recall(absPath, text string) ([]map[string]any, bool) {
	el, ok := e.verdictsIndex[absPath]
	if !ok {
		return nil, false
	}
	hit := el.Value.(*verdictEntry)
	if hit.digest != digestText(text) {
		return nil, false
	}
	return append([]map[string]any{}, hit.diags...), true
}

// InlineWait returns how long a write may still wait for its verdict, the grace when its start is unknown
func InlineWait(started time.Time) time.Duration {
	if started.IsZero() {
		return InlineGrace
	}
	return max(0, min(InlineGrace, time.Until(started.Add(WriteBudget))))
}

func digestText(text string) string {
	sum := sha256.Sum256([]byte(AsDiskRead(text)))
	return hex.EncodeToString(sum[:])
}

// EditFuture is one scheduled post write check
type EditFuture struct {
	mu        sync.Mutex
	done      chan struct{}
	result    *CheckResult
	err       error
	cancelled bool
	callbacks []func(*CheckResult, bool, error)
}

// Wait returns the verdict within the timeout, ready false while the check still runs
func (f *EditFuture) Wait(timeout time.Duration) (*CheckResult, bool, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-f.done:
	case <-t.C:
		select {
		case <-f.done:
		default:
			return nil, false, nil
		}
	}
	if f.cancelled {
		return nil, true, context.Canceled
	}
	return f.result, true, f.err
}

// OnDone runs fn once when the check finishes or is cancelled, at once when it already has
func (f *EditFuture) OnDone(fn func(res *CheckResult, cancelled bool, err error)) {
	f.mu.Lock()
	select {
	case <-f.done:
		f.mu.Unlock()
		fn(f.result, f.cancelled, f.err)
		return
	default:
	}
	f.callbacks = append(f.callbacks, fn)
	f.mu.Unlock()
}

// Resolves the future and runs its callbacks outside the loop
func (f *EditFuture) resolve(res *CheckResult, cancelled bool, err error) {
	f.mu.Lock()
	f.result, f.cancelled, f.err = res, cancelled, err
	close(f.done)
	callbacks := f.callbacks
	f.callbacks = nil
	f.mu.Unlock()
	for _, fn := range callbacks {
		fn(res, cancelled, err)
	}
}

// Schedules the check of an already written file on the service loop, nil when the loop is gone
func startEditCheck(s *Service, path string, preText, postText *string, owner string, carry []Note) (*EditFuture, error) {
	if !s.l.running() {
		return nil, nil
	}
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return nil, err
	}
	s.checks.newestMu.Lock()
	if postText != nil {
		s.checks.newest[absPath] = postText
	} else {
		delete(s.checks.newest, absPath)
	}
	s.checks.newestMu.Unlock()
	fut := &EditFuture{done: make(chan struct{})}
	carry = append([]Note{}, carry...)
	var res *CheckResult
	var checkErr error
	t := s.l.spawn(context.Background(), func(ctx context.Context) {
		defer func() {
			if r := recover(); r != nil {
				res, checkErr = nil, panicError(r)
			}
		}()
		res, checkErr = serializedCheck(ctx, s, absPath, preText, postText, owner, carry)
	})
	go func() {
		<-t.done
		if checkErr != nil && isCancellation(checkErr) {
			fut.resolve(nil, true, nil)
			return
		}
		fut.resolve(res, false, checkErr)
	}()
	return fut, nil
}

// Runs after any unfinished check of the file, taking over the same session's unfinished check
func serializedCheck(ctx context.Context, s *Service, absPath string, preText, postText *string, owner string, carry []Note) (res *CheckResult, err error) {
	checks := s.checks
	baselineText := preText
	if len(carry) > 0 {
		baselineText = carry[0].BaselineText
	}
	tail := checks.tail[absPath]
	if tail != nil && isClosed(tail.done) {
		tail = nil
	}
	var prev *inflightCheck
	if owner != "" {
		prev = checks.inflight[ownerPath{owner, absPath}]
	}
	if prev != nil && !isClosed(prev.done) {
		prev.superseded = true
		baselineText = prev.baselineText
		carry = append(append([]Note{}, prev.carry...), carry...)
	} else {
		prev = nil
	}
	me := &inflightCheck{done: make(chan struct{}), baselineText: baselineText, carry: carry}
	checks.tail[absPath] = me
	if owner != "" {
		checks.inflight[ownerPath{owner, absPath}] = me
	}
	defer func() {
		if checks.tail[absPath] == me {
			delete(checks.tail, absPath)
			checks.newestMu.Lock()
			if postText != nil && checks.newest[absPath] == postText {
				delete(checks.newest, absPath)
			}
			checks.newestMu.Unlock()
		}
		if owner != "" && checks.inflight[ownerPath{owner, absPath}] == me {
			delete(checks.inflight, ownerPath{owner, absPath})
		}
		me.result, me.err = res, err
		me.cancelled = err != nil && isCancellation(err)
		close(me.done)
	}()
	var prevResult *CheckResult
	for _, w := range []*inflightCheck{prev, tail} {
		if w != nil {
			if err := s.l.await(ctx, w.done); err != nil {
				return nil, err
			}
		}
	}
	if prev != nil && !prev.cancelled && prev.err == nil {
		prevResult = prev.result
		if !prevResult.HasVerdict {
			prevResult = nil
		}
	}
	if me.superseded {
		if prevResult != nil {
			return &CheckResult{Diagnostics: prevResult.Diagnostics, HasVerdict: true, BaselineText: prevResult.BaselineText, Superseded: true}, nil
		}
		return &CheckResult{BaselineText: baselineText, Superseded: true}, nil
	}
	budget := s.waitBudgetOnLoop(absPath)
	type verdict struct {
		diags []map[string]any
		ok    bool
	}
	v, cerr := waitFor(ctx, 2*budget+spawnCeiling, func(ctx context.Context) (verdict, error) {
		d, ok, err := runCheck(ctx, s, absPath, baselineText, postText, budget)
		return verdict{d, ok}, err
	})
	if cerr != nil {
		if isCancellation(cerr) {
			return nil, cerr
		}
		s.rt.logf(slog.LevelDebug, editCheckLogger, "LSP edit check failed for %s", absPath)
		if err := s.markBrokenForFileAsync(ctx, absPath, cerr); err != nil {
			return nil, err
		}
		v = verdict{}
	}
	switch {
	case v.ok:
		res = &CheckResult{Diagnostics: v.diags, HasVerdict: true, BaselineText: baselineText}
	case prevResult != nil:
		res = &CheckResult{Diagnostics: prevResult.Diagnostics, HasVerdict: true, BaselineText: prevResult.BaselineText}
	default:
		res = &CheckResult{BaselineText: baselineText, Carry: carry}
	}
	res.Superseded = me.superseded
	if me.superseded {
		res.Carry = nil
	}
	return res, nil
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Verdict on the edit's own bytes, then when it has diagnostics the pre edit verdict to filter by
func runCheck(ctx context.Context, s *Service, absPath string, baselineText, postText *string, budget time.Duration) ([]map[string]any, bool, error) {
	client, err := s.getOrSpawn(ctx, absPath)
	if err != nil || client == nil {
		return nil, false, err
	}
	srv := s.serverFor(absPath)
	serverID := "lsp"
	if srv != nil {
		serverID = srv.ServerID
	}
	languageID := LanguageIDFor(absPath, srv)
	checks := s.checks
	useSync := syncSupported(client)
	_, opened, err := client.documentText(absPath)
	if err != nil {
		return nil, false, err
	}
	if client.seedsFirstPush() && !opened && !useSync {
		openText := baselineText
		if baselineText == nil {
			openText = postText
		}
		version, err := client.openFile(ctx, absPath, languageID, openText)
		if err != nil {
			return nil, false, err
		}
		var seed []map[string]any
		seeded := false
		if version == 0 {
			if seed, seeded, err = client.waitForSeed(ctx, absPath, budget); err != nil {
				return nil, false, err
			}
		}
		if baselineText == nil {
			if err := resyncDisk(ctx, s, client, absPath, languageID); err != nil {
				return nil, false, err
			}
			s.touch(client)
			return report(s.rt, serverID, absPath, seed, seeded)
		}
		if seeded {
			checks.remember(absPath, *baselineText, seed)
		}
	}
	version, err := client.openFile(ctx, absPath, languageID, postText)
	if err != nil {
		return nil, false, err
	}
	if err := client.saveFile(ctx, absPath); err != nil {
		return nil, false, err
	}
	diags, ok, err := checkVerdict(ctx, s, client, absPath, version, budget, useSync)
	if err != nil {
		return nil, false, err
	}
	s.touch(client)
	if !ok {
		if err := resyncDisk(ctx, s, client, absPath, languageID); err != nil {
			return nil, false, err
		}
		return report(s.rt, serverID, absPath, nil, false)
	}
	synced, hasSynced, err := client.documentText(absPath)
	if err != nil {
		return nil, false, err
	}
	verdictDiags := diags
	if len(diags) > 0 && baselineText != nil && hasSynced {
		baseline, cached := checks.recall(absPath, *baselineText)
		if !cached {
			baseline = []map[string]any{}
			baseVersion, err := client.openFile(ctx, absPath, languageID, baselineText)
			if err != nil {
				return nil, false, err
			}
			base, baseOK, err := checkVerdict(ctx, s, client, absPath, baseVersion, budget, useSync)
			if err != nil {
				return nil, false, err
			}
			if baseOK {
				baseline = base
			}
		}
		shift, err := BuildLineShift(s.rt, *baselineText, synced)
		if err != nil {
			return nil, false, err
		}
		shifted, err := ShiftBaseline(baseline, shift)
		if err != nil {
			return nil, false, err
		}
		seen := map[string]bool{}
		for _, d := range shifted {
			k, err := DiagnosticKey(d)
			if err != nil {
				return nil, false, err
			}
			seen[k] = true
		}
		kept := []map[string]any{}
		for _, d := range diags {
			k, err := DiagnosticKey(d)
			if err != nil {
				return nil, false, err
			}
			if !seen[k] {
				kept = append(kept, d)
			}
		}
		diags = kept
	}
	if hasSynced {
		checks.remember(absPath, synced, verdictDiags)
	}
	if err := resyncDisk(ctx, s, client, absPath, languageID); err != nil {
		return nil, false, err
	}
	return report(s.rt, serverID, absPath, diags, true)
}

// Leaves the server holding the newest content written to the file, rereading it when unknown
func resyncDisk(ctx context.Context, s *Service, client *Client, absPath, languageID string) error {
	newest := s.checks.newestFor(absPath)
	if newest == nil {
		if _, err := client.openFile(ctx, absPath, languageID, nil); err != nil {
			if isCancellation(err) {
				return err
			}
			s.rt.logf(slog.LevelDebug, editCheckLogger, "LSP resync of %s failed", absPath)
		}
		return nil
	}
	text, ok, err := client.documentText(absPath)
	if err != nil {
		return err
	}
	if !ok || AsDiskRead(*newest) != text {
		_, err := client.openFile(ctx, absPath, languageID, newest)
		return err
	}
	return nil
}

// Diagnostics on the text synced at a version, the synchronous request first where the server has one
func checkVerdict(ctx context.Context, s *Service, client *Client, absPath string, version int, budget time.Duration, useSync bool) ([]map[string]any, bool, error) {
	if useSync {
		diags, ok, err := syncRequest(ctx, client, absPath, budget)
		if err != nil {
			if _, timeout := err.(*TimeoutError); timeout {
				return nil, false, nil
			}
			return nil, false, err
		}
		if ok {
			return diags, true, nil
		}
	}
	fresh, err := clientWaitForDiagnostics(client, ctx, absPath, version, s.waitMode, budget)
	if err != nil || !fresh {
		return nil, false, err
	}
	diags, err := client.diagnosticsFor(absPath, true)
	if err != nil {
		return nil, false, err
	}
	return diags, true, nil
}

// Logs the verdict outcome and passes it through
func report(rt *Runtime, serverID, absPath string, diags []map[string]any, ok bool) ([]map[string]any, bool, error) {
	switch {
	case !ok:
		logTimeout(rt, serverID, absPath, "fresh diagnostics")
	case len(diags) > 0:
		logDiagnostics(rt, serverID, absPath, len(diags))
	default:
		logClean(rt, serverID, absPath)
	}
	return diags, ok, nil
}
