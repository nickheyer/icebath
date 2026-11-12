package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const configLoadersConfig = "platform_toolsets: {cli: []}\n" +
	"model: {provider: custom, default: m, base_url: \"http://127.0.0.1:9/v1\"}\n"

// Writes a one message prefill file whose user content is the mark
func configLoadersPrefill(t *testing.T, home, name, mark string) {
	t.Helper()
	raw, err := json.Marshal([]any{map[string]any{"role": "user", "content": mark}})
	if err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, home, name, string(raw))
}

type configLoadersOwner struct {
	ctx  context.Context
	a    *SessionAuthority
	home string
}

// Owner over a profile whose config names prefill.json, the file holding FIRST
func newConfigLoadersOwner(t *testing.T, extra string) configLoadersOwner {
	t.Helper()
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	ctx, a, home := envRefsOwner(t, configLoadersConfig+extra, "")
	configLoadersPrefill(t, home, "prefill.json", "FIRST")
	return configLoadersOwner{ctx, a, home}
}

func (o configLoadersOwner) params(cwd, requestID string) map[string]any {
	return map[string]any{"request_id": requestID, "source": "cli", "cwd": cwd, "toolsets": []any{}}
}

// Created session and its stored policy, or the create refusal
func (o configLoadersOwner) create(t *testing.T, requestID string) (SessionRef, *LocalSessionPolicy, error) {
	t.Helper()
	ref, err := createLocalSession(o.ctx, o.a, envRefsActor(o.a), o.params(o.home, requestID), localCreateOptions{})
	if err != nil {
		return SessionRef{}, nil, err
	}
	return ref, o.policy(t, ref.SessionID), nil
}

func (o configLoadersOwner) policy(t *testing.T, sid string) *LocalSessionPolicy {
	t.Helper()
	raw, _ := recoverReceipt(t, o.ctx, o.a.DB, sid).Get("policy")
	policy, err := restorePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func (o configLoadersOwner) stage(t *testing.T, cwd, requestID string) *state.LocalCreateReceipt {
	t.Helper()
	ref, staged, err := stageLocalSession(o.ctx, o.a, envRefsActor(o.a), o.params(cwd, requestID), localCreateOptions{})
	if err != nil || staged == nil {
		t.Fatalf("staging answered %v %v %v", ref, staged, err)
	}
	return staged
}

// Off loop commit and publication every async create runs after staging
func (o configLoadersOwner) commit(t *testing.T, staged *state.LocalCreateReceipt) (SessionRef, error) {
	t.Helper()
	if _, err := o.a.DB.CommitLocalSession(o.ctx, o.a.Epoch, *staged); err != nil {
		return SessionRef{}, err
	}
	sid, err := receiptField(staged.Receipt, "session_id")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := restoreLocalSession(o.ctx, o.a, pyStrOf(sid))
	if err != nil {
		t.Fatal(err)
	}
	return ref, nil
}

// Prefill content an in process build hands its agent, the session prefill or none when empty
func (o configLoadersOwner) inProcess(t *testing.T, policy *LocalSessionPolicy, sid string) []any {
	t.Helper()
	prefill, err := SessionPrefill(o.ctx, o.a, policy, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefill) == 0 {
		return nil
	}
	return prefill
}

// Prefill content the managed worker inputs carry for the session
func (o configLoadersOwner) worker(t *testing.T, policy *LocalSessionPolicy, sid string) []any {
	t.Helper()
	inputs, err := constructInputs(o.ctx, o.a, policy, sid)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := inputs.Get("prefill_messages")
	messages, _ := value.([]any)
	return messages
}

func configLoadersFirst(t *testing.T, messages []any) any {
	t.Helper()
	if len(messages) == 0 {
		t.Fatalf("no prefill messages")
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("prefill message %#v", messages[0])
	}
	return message["content"]
}

// Prefill a managed worker and an in process build of the session get, which must agree
func (o configLoadersOwner) seen(t *testing.T, sid string) any {
	t.Helper()
	policy := o.policy(t, sid)
	worker := configLoadersFirst(t, o.worker(t, policy, sid))
	if inProcess := configLoadersFirst(t, o.inProcess(t, policy, sid)); inProcess != worker {
		t.Fatalf("in process prefill %v, worker %v", inProcess, worker)
	}
	return worker
}

func (o configLoadersOwner) held() []string {
	o.a.mu.Lock()
	defer o.a.mu.Unlock()
	out := make([]string, 0, len(o.a.localPrefill))
	for sid := range o.a.localPrefill {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

func (o configLoadersOwner) holds(sid string) bool {
	o.a.mu.Lock()
	defer o.a.mu.Unlock()
	_, held := o.a.localPrefill[sid]
	return held
}

func configLoadersReason(t *testing.T, err error) string {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a runtime store refusal, got %v", err)
	}
	return refusal.Reason
}

// Goroutine id of the caller, read from its own stack header
func configLoadersGoroutine() int64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	fields := strings.Fields(strings.TrimPrefix(string(buf), "goroutine "))
	id, _ := strconv.ParseInt(fields[0], 10, 64)
	return id
}

func (o configLoadersOwner) mutate(t *testing.T, ref SessionRef, requestID, operation string) *pyjson.Object {
	t.Helper()
	result, err := mutateSession(o.ctx, o.a, envRefsActor(o.a), ref, pyjson.ObjectOf(
		"session_id", ref.SessionID, "request_id", requestID, "expected_revision", 0, "expected_generation", 0,
		"operation", operation, "payload", pyjson.ObjectOf()))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Installs the session deletion hooks the process wiring installs from gateway and tools
func configLoadersDeleteHooks(t *testing.T) {
	saved, savedFilter, savedNames, savedReferenced := state.ReleaseUnheldMedia, state.SpilloverSQLFilter, state.SpilloverNamesInText, state.ReferencedSpilloverNames
	state.ReleaseUnheldMedia = ReleaseUnheldMediaRetainingHistory
	state.SpilloverSQLFilter = tools.SpilloverSQLFilter
	state.SpilloverNamesInText = tools.SpilloverNamesInText
	state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
	t.Cleanup(func() {
		state.ReleaseUnheldMedia, state.SpilloverSQLFilter, state.SpilloverNamesInText, state.ReferencedSpilloverNames = saved, savedFilter, savedNames, savedReferenced
	})
}

// Launch params of a custom route session in the directory
func configLoadersLaunch(cwd string) map[string]any {
	return map[string]any{"cwd": cwd, "model": "m", "provider": "custom", "base_url": "http://127.0.0.1:9/v1"}
}

func configLoadersBuilt(t *testing.T, ctx context.Context, params, cfg map[string]any) *LocalSessionPolicy {
	t.Helper()
	policy, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestEachNewSessionReadsItsProfilesPrefillOnceAndWorkersMatch(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	ctx, a, home := envRefsOwner(t, "", "")
	o := configLoadersOwner{ctx, a, home}
	configLoadersPrefill(t, home, "prefill.json", "FIRST")
	launch := configLoadersLaunch(home)
	first := configLoadersBuilt(t, ctx, launch, map[string]any{"prefill_messages_file": "prefill.json"})
	if got := o.worker(t, first, "sid-1"); !reflect.DeepEqual(got, []any{map[string]any{"role": "user", "content": "FIRST"}}) {
		t.Fatalf("first worker prefill %#v", got)
	}
	configLoadersPrefill(t, home, "prefill.json", "EDITED")
	configLoadersPrefill(t, home, "moved.json", "MOVED")
	if got := configLoadersFirst(t, o.worker(t, first, "sid-1")); got != "FIRST" {
		t.Fatalf("open session worker prefill %v", got)
	}
	if got := configLoadersFirst(t, o.inProcess(t, first, "sid-1")); got != "FIRST" {
		t.Fatalf("open session prefill %v", got)
	}
	if got := configLoadersFirst(t, o.inProcess(t, first, "sid-2")); got != "EDITED" {
		t.Fatalf("new session prefill %v", got)
	}
	moved := configLoadersBuilt(t, ctx, launch, map[string]any{"agent": map[string]any{"prefill_messages_file": filepath.Join(home, "moved.json")}})
	if got := configLoadersFirst(t, o.worker(t, moved, "sid-3")); got != "MOVED" {
		t.Fatalf("moved session prefill %v", got)
	}
	if got, err := sessionPrefill(ctx, a, configLoadersBuilt(t, ctx, launch, map[string]any{}), "sid-4"); err != nil || got != nil {
		t.Fatalf("unset prefill %#v %v", got, err)
	}
	bypass := map[string]any{"ignore_user_config": true}
	for k, v := range launch {
		bypass[k] = v
	}
	configOnly, err := bypassPolicy(ctx, bypass, &RecoveredSecrets{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sessionPrefill(ctx, a, configOnly, "sid-5"); err != nil || got != nil {
		t.Fatalf("bypass prefill %#v %v", got, err)
	}
}

func TestAnOpenSessionsPrefillSurvivesCachePressureAndFileEdits(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	ctx, a, home := envRefsOwner(t, "", "")
	o := configLoadersOwner{ctx, a, home}
	configLoadersPrefill(t, home, "prefill.json", "ORIGINAL")
	policy := configLoadersBuilt(t, ctx, configLoadersLaunch(home), map[string]any{"prefill_messages_file": "prefill.json"})
	if got := configLoadersFirst(t, o.inProcess(t, policy, "open")); got != "ORIGINAL" {
		t.Fatalf("open session prefill %v", got)
	}
	configLoadersPrefill(t, home, "prefill.json", "EDITED")
	for n := 0; n < 600; n++ {
		if _, err := sessionPrefill(ctx, a, policy, "other-"+strconv.Itoa(n)); err != nil {
			t.Fatal(err)
		}
	}
	if got := configLoadersFirst(t, o.worker(t, policy, "open")); got != "ORIGINAL" {
		t.Fatalf("worker prefill after pressure %v", got)
	}
	if got := configLoadersFirst(t, o.inProcess(t, policy, "open")); got != "ORIGINAL" {
		t.Fatalf("in process prefill after pressure %v", got)
	}
}

func TestPrefillIsReadWhenTheSessionIsCreatedAndDroppedWithIt(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	ctx, a, home := envRefsOwner(t, configLoadersConfig+"prefill_messages_file: prefill.json\n", "")
	o := configLoadersOwner{ctx, a, home}
	configLoadersPrefill(t, home, "prefill.json", "AT_CREATE")
	ref, policy, err := o.create(t, "prefill")
	if err != nil {
		t.Fatal(err)
	}
	configLoadersPrefill(t, home, "prefill.json", "BEFORE_FIRST_TURN")
	if got := configLoadersFirst(t, o.worker(t, policy, ref.SessionID)); got != "AT_CREATE" {
		t.Fatalf("worker prefill %v", got)
	}
	if got := configLoadersFirst(t, o.inProcess(t, policy, ref.SessionID)); got != "AT_CREATE" {
		t.Fatalf("in process prefill %v", got)
	}
	if err := retireLiveSessions(ctx, a, []string{ref.SessionID}, false); err != nil {
		t.Fatal(err)
	}
	if o.holds(ref.SessionID) {
		t.Fatalf("retired session %s still holds its prefill", ref.SessionID)
	}
}

func TestThePrefillFileIsReadByTheOffLoopCommitNotWhileStaging(t *testing.T) {
	o := newConfigLoadersOwner(t, "prefill_messages_file: prefill.json\n")
	var mu sync.Mutex
	var readers []int64
	real := LoadPrefillMessages
	LoadPrefillMessages = func(ctx context.Context, filePath, baseDir string) []any {
		mu.Lock()
		readers = append(readers, configLoadersGoroutine())
		mu.Unlock()
		return real(ctx, filePath, baseDir)
	}
	reads := func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64{}, readers...)
	}
	staged := o.stage(t, o.home, "slow-mount")
	if got := reads(); len(got) != 0 {
		t.Fatalf("staging read the prefill file %v", got)
	}
	if _, err := trackedWrite(o.ctx, o.a, func(ctx context.Context) (any, error) {
		return o.a.DB.CommitLocalSession(ctx, o.a.Epoch, *staged)
	}, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := reads(); len(got) != 1 || got[0] == configLoadersGoroutine() {
		t.Fatalf("prefill readers %v, caller %d", got, configLoadersGoroutine())
	}
	sid, err := receiptField(staged.Receipt, "session_id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreLocalSession(o.ctx, o.a, pyStrOf(sid)); err != nil {
		t.Fatal(err)
	}
	if got := o.seen(t, pyStrOf(sid)); got != "FIRST" || len(reads()) != 1 {
		t.Fatalf("seen %v after %d reads", got, len(reads()))
	}
}

func TestOverlappingExactCreatesKeepTheCommittedAttemptsPrefill(t *testing.T) {
	for _, winner := range []int{0, 1} {
		t.Run("winner_"+strconv.Itoa(winner), func(t *testing.T) {
			o := newConfigLoadersOwner(t, "prefill_messages_file: prefill.json\n")
			staged := []*state.LocalCreateReceipt{o.stage(t, o.home, "overlap"), o.stage(t, o.home, "overlap")}
			ordered := staged
			if winner == 1 {
				ordered = []*state.LocalCreateReceipt{staged[1], staged[0]}
			}
			first, err := o.commit(t, ordered[0])
			if err != nil {
				t.Fatal(err)
			}
			configLoadersPrefill(t, o.home, "prefill.json", "SECOND")
			second, err := o.commit(t, ordered[1])
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("refs differ %v %v", first, second)
			}
			configLoadersPrefill(t, o.home, "prefill.json", "LATER")
			if got := o.seen(t, first.SessionID); got != "FIRST" {
				t.Fatalf("seen %v", got)
			}
			if got := o.held(); !reflect.DeepEqual(got, []string{first.SessionID}) {
				t.Fatalf("held prefill %v", got)
			}
		})
	}
}

func TestAConflictingLosingCreateNeverReplacesTheWinnersPrefill(t *testing.T) {
	o := newConfigLoadersOwner(t, "prefill_messages_file: prefill.json\n")
	winner := o.stage(t, o.home, "conflict")
	loser := o.stage(t, t.TempDir(), "conflict")
	ref, err := o.commit(t, winner)
	if err != nil {
		t.Fatal(err)
	}
	configLoadersPrefill(t, o.home, "prefill.json", "LOSER")
	_, err = o.commit(t, loser)
	if reason := configLoadersReason(t, err); reason != "invalid_params" {
		t.Fatalf("losing commit refused %q", reason)
	}
	if got := o.seen(t, ref.SessionID); got != "FIRST" {
		t.Fatalf("seen %v", got)
	}
	if got := o.held(); !reflect.DeepEqual(got, []string{ref.SessionID}) {
		t.Fatalf("held prefill %v", got)
	}
}

func TestACreateRefusedByARetirementTombstoneHoldsNoPrefill(t *testing.T) {
	o := newConfigLoadersOwner(t, "prefill_messages_file: prefill.json\n")
	configLoadersDeleteHooks(t)
	ref, _, err := o.create(t, "gone")
	if err != nil {
		t.Fatal(err)
	}
	o.mutate(t, ref, "delete", "delete")
	if o.holds(ref.SessionID) {
		t.Fatalf("deleted session %s still holds its prefill", ref.SessionID)
	}
	_, _, err = o.create(t, "gone")
	if reason := configLoadersReason(t, err); reason != "not_found" {
		t.Fatalf("retried create refused %q", reason)
	}
	if o.holds(ref.SessionID) {
		t.Fatalf("refused create left a prefill for %s", ref.SessionID)
	}
}

func TestRetriedRestoredAndBranchedSessionsLoadPrefillOnceAndKeepIt(t *testing.T) {
	o := newConfigLoadersOwner(t, "prefill_messages_file: prefill.json\n")
	ref, _, err := o.create(t, "kept")
	if err != nil {
		t.Fatal(err)
	}
	configLoadersPrefill(t, o.home, "prefill.json", "RETRY")
	retried, _, err := o.create(t, "kept")
	if err != nil || !reflect.DeepEqual(retried, ref) {
		t.Fatalf("exact retry answered %v %v", retried, err)
	}
	if got := o.seen(t, ref.SessionID); got != "FIRST" {
		t.Fatalf("retried session seen %v", got)
	}

	o.a.mu.Lock()
	clear(o.a.localPrefill)
	o.a.mu.Unlock()
	if _, err := restoreLocalSession(o.ctx, o.a, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	configLoadersPrefill(t, o.home, "prefill.json", "RESTORED")
	if got := o.seen(t, ref.SessionID); got != "RESTORED" {
		t.Fatalf("restored session seen %v", got)
	}
	configLoadersPrefill(t, o.home, "prefill.json", "AFTER_RESTORE")
	if got := o.seen(t, ref.SessionID); got != "RESTORED" {
		t.Fatalf("restored session reread %v", got)
	}

	result := o.mutate(t, ref, "branch", "branch")
	branched, _ := result.Get("branched_session_id")
	child := pyStrOf(branched)
	configLoadersPrefill(t, o.home, "prefill.json", "BRANCHED")
	if got := o.seen(t, child); got != "BRANCHED" {
		t.Fatalf("branch seen %v", got)
	}
	configLoadersPrefill(t, o.home, "prefill.json", "AFTER_BRANCH")
	if got := o.seen(t, child); got != "BRANCHED" {
		t.Fatalf("branch reread %v", got)
	}
	if got := o.seen(t, ref.SessionID); got != "RESTORED" {
		t.Fatalf("parent seen %v", got)
	}
}
