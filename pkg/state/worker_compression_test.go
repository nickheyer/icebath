package state

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Generic JSON codec double standing for the agent's goal, heartbeat and loop state types
type jsonAutomationState struct{ obj *pyjson.Object }

func (s *jsonAutomationState) Status() any             { v, _ := s.obj.Get("status"); return v }
func (s *jsonAutomationState) SetStatus(status string) { s.obj.Set("status", status) }
func (s *jsonAutomationState) ToJSON() (string, error) {
	return pyjson.Dumps(s.obj, pyjson.EnsureASCII(false))
}

// Installs the JSON codec double for every automation family the agent wiring would decode
func installAutomationCodecDoubles() {
	for _, family := range []string{"goal", "heartbeat", "loop"} {
		WorkerAutomationCodecs[family] = func(raw string) (AutomationState, error) {
			decoded, err := pyjson.Loads(raw)
			if err != nil {
				return nil, pyValueError(err.Error())
			}
			return &jsonAutomationState{decoded.(*pyjson.Object)}, nil
		}
	}
}

// Boolean result of a facade call that must not fail
func mustBool(t *testing.T) func(bool, error) bool {
	return func(ok bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
}

func TestGuardRollbackAndStreaksAreDurableReceipts(t *testing.T) {
	db, store := workerFixture(t)
	defer db.Close()
	deadline := now() + 3600
	snapshot, err := store.getCompressionFailureCooldownRow("owned")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordCompressionFailureCooldown("owned", deadline, "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.recordCompressionFailureCooldown("owned", deadline-100, "latest"); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetCompressionFailureCooldownRow("owned")
	if err != nil || !pyjson.Equal(row.object(), pyjson.ObjectOf("session_exists", true, "cooldown_until", deadline, "error", "latest")) {
		t.Fatalf("cooldown row %v %v", row.object(), err)
	}
	if err := store.restoreCompressionFailureCooldownRow("owned", snapshot); err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetCompressionFailureCooldownRow("owned")
	if !pyjson.Equal(row.object(), snapshot) {
		t.Fatalf("restored %v != %v", row.object(), snapshot)
	}
	if err := store.recordCompressionFailureCooldown("owned", deadline, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.clearCompressionFailureCooldown("owned"); err != nil {
		t.Fatal(err)
	}
	if cooldown, err := store.getCompressionFailureCooldown("owned"); err != nil || cooldown != nil {
		t.Fatalf("cooldown %v %v", cooldown, err)
	}
	if err := store.setCompressionCounter("fallback", "owned", int64(4)); err != nil {
		t.Fatal(err)
	}
	if err := store.setCompressionCounter("ineffective", "owned", int64(3)); err != nil {
		t.Fatal(err)
	}
	if err := store.setCompressionCounter("recovery", "owned", deadline); err != nil {
		t.Fatal(err)
	}
	streak, _ := db.CompressionFallbackStreak("owned")
	ineffective, _ := db.CompressionIneffectiveCount("owned")
	recovery, _ := db.CompressionRecoveryDeadline("owned")
	if streak != 4 || ineffective != 3 || recovery != deadline {
		t.Fatalf("counters %d %d %v", streak, ineffective, recovery)
	}
	seq := store.nextSequence
	_, err = db.MutateWorkerExecution(context.Background(), store.rawReceipt("compression.cooldown.restore",
		map[string]any{"snapshot": map[string]any{"session_exists": false, "cooldown_until": nil, "error": nil}}))
	if err == nil || !strings.Contains(err.Error(), "cannot restore absent") {
		t.Fatalf("absent restore %v", err)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT last_sequence FROM worker_executions"), seq-1) {
		t.Fatal("failed receipt advanced the sequence")
	}
}

func TestCompressionLeaseCannotBeRevivedOrReleasedByOldHolder(t *testing.T) {
	ctx := context.Background()
	db, store := workerFixture(t)
	defer db.Close()
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "old")) || mustBool(t)(store.tryAcquireCompressionLock("owned", "new")) {
		t.Fatal("first acquire")
	}
	if err := db.writeSQL(ctx, "UPDATE compression_locks SET expires_at=0 WHERE session_id=?", []any{"owned"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.refreshCompressionLock("owned", "old")) {
		t.Fatal("holder refresh past expiry")
	}
	if err := db.writeSQL(ctx, "UPDATE compression_locks SET expires_at=0 WHERE session_id=?", []any{"owned"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "new")) || mustBool(t)(store.refreshCompressionLock("owned", "old")) {
		t.Fatal("expired lock takeover")
	}
	if err := store.releaseCompressionLock("owned", "old"); err != nil {
		t.Fatal(err)
	}
	if holder, err := store.getCompressionLockHolder("owned"); err != nil || holder != "new" {
		t.Fatalf("holder %v %v", holder, err)
	}
	for _, c := range []struct {
		change func(*WorkerReceipt)
		reason string
	}{
		{func(in *WorkerReceipt) { in.Epoch-- }, "stale_epoch"},
		{func(in *WorkerReceipt) { in.SessionID = "foreign" }, "permission_denied"},
	} {
		in := store.rawReceipt("compression.lock.release", map[string]any{"holder": "new"})
		c.change(&in)
		if r := receiptRefusal(t, db, in); r != c.reason {
			t.Fatalf("%s != %s", r, c.reason)
		}
	}
	if holder, _ := db.GetCompressionLockHolder("owned"); holder == nil || *holder != "new" {
		t.Fatal("refused releases dropped the lock")
	}
	if err := store.releaseCompressionLock("owned", "new"); err != nil {
		t.Fatal(err)
	}
	if holder, err := store.getCompressionLockHolder("owned"); err != nil || holder != nil {
		t.Fatalf("holder after release %v %v", holder, err)
	}
}

func TestRotationCleanupReleasesOnlyItsOriginalCompressionHolder(t *testing.T) {
	db, store := workerFixture(t)
	defer db.Close()
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "holder")) {
		t.Fatal("acquire")
	}
	if err := store.publishCompressionChild("owned", "child", "cli", textMessages("assistant", "summary"), publishOptions{compressionLockHolder: "holder"}); err != nil {
		t.Fatal(err)
	}
	if err := store.releaseCompressionLock("owned", "wrong"); err != nil {
		t.Fatal(err)
	}
	if holder, _ := db.GetCompressionLockHolder("owned"); holder == nil || *holder != "holder" {
		t.Fatal("wrong holder released the lock")
	}
	if err := store.releaseCompressionLock("owned", "holder"); err != nil {
		t.Fatal(err)
	}
	if holder, _ := db.GetCompressionLockHolder("owned"); holder != nil {
		t.Fatal("original holder could not release")
	}
}

func TestReopenOrphanedParentDoesNotStealLiveOrPublishedContinuations(t *testing.T) {
	ctx := context.Background()
	db, store := workerFixture(t)
	defer db.Close()
	if err := db.EndSession("owned", "compression"); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "holder")) || mustBool(t)(store.reopenOrphanedCompressionSession("owned")) {
		t.Fatal("reopen under a live lease")
	}
	if err := db.writeSQL(ctx, "UPDATE compression_locks SET expires_at=0 WHERE session_id=?", []any{"owned"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.reopenOrphanedCompressionSession("owned")) || rowValue(mustSession(t, db, "owned"), "ended_at") != nil {
		t.Fatal("reopen after expiry")
	}
	if mustBool(t)(store.refreshCompressionLock("owned", "holder")) {
		t.Fatal("expired holder refreshed after reopen")
	}
	if err := db.EndSession("owned", "compression"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(NewSession{ID: "published", Source: "cli", ParentSessionID: "owned"}); err != nil {
		t.Fatal(err)
	}
	if mustBool(t)(store.reopenOrphanedCompressionSession("owned")) || !pyjson.Equal(rowValue(mustSession(t, db, "owned"), "end_reason"), "compression") {
		t.Fatal("reopen stole a published continuation")
	}
}

func TestCompressionFlushReturnsCanonicalAnnotationsAndKeepsTurnGuard(t *testing.T) {
	ctx := context.Background()
	db, store := workerFixture(t)
	defer db.Close()
	if !mustBool(t)(store.tryAcquireSessionTurnLease("owned", "turn")) || !mustBool(t)(store.tryAcquireCompressionLock("owned", "compression")) {
		t.Fatal("leases")
	}
	rows := []map[string]any{{"role": "user", "content": "accepted", "api_content": " original wire ",
		"display_metadata": map[string]any{"_accepted_input_id": "input"}}}
	compression, turn := "compression", "turn"
	if n, err := store.appendMessagesBatchWith("owned", rows, appendOptions{compressionLockHolder: &compression, turnLeaseHolder: &turn, turnLeaseTTLSeconds: 42}); err != nil || n != 1 {
		t.Fatalf("append %d %v", n, err)
	}
	conversation, err := db.GetMessagesAsConversation(ctx, "owned", ConversationOptions{IncludeRowIDs: true})
	if err != nil || !pyjson.Equal(rows[0]["_row_id"], conversation[0]["_row_id"]) {
		t.Fatalf("row id %v %v %v", rows[0]["_row_id"], conversation, err)
	}
	plain, _ := db.GetMessagesAsConversation(ctx, "owned", ConversationOptions{})
	if !pyjson.Equal(plain[0]["api_content"], " original wire ") {
		t.Fatalf("api_content %v", plain[0])
	}
	_, err = db.MutateWorkerExecution(ctx, store.rawReceipt("compression.append", map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "stale"}},
		"compression_lock_holder": "compression", "turn_lease_holder": "stale", "turn_lease_ttl_seconds": int64(42)}))
	if err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("stale turn holder %v", err)
	}
	if len(messageContents(t, db, "owned")) != 1 {
		t.Fatal("stale append landed")
	}
}

func TestUsageReceiptIncludesRouteRowsAndRollsBackOnReceiptFailure(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "w", SessionID: "s", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "w", SessionID: "s", Generation: 0, Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	payload := pyjson.ObjectOf("input_tokens", int64(13), "output_tokens", int64(7), "api_call_count", int64(1),
		"model", "model-a", "billing_provider", "provider", "estimated_cost_usd", 0.25)
	args := scope
	args.Sequence, args.Operation, args.Payload = 1, "usage.main", payload
	receipt := mustReceipt(t, db, args)
	if again := mustReceipt(t, db, args); !pyjson.Equal(again, receipt) {
		t.Fatalf("replay %v", again)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "s"), "input_tokens"), 13) || !pyjson.Equal(scalarOf(t, db.conn, "SELECT input_tokens FROM session_model_usage"), 13) {
		t.Fatal("usage rows")
	}
	mustExecConn(t, db.conn, "CREATE TRIGGER deny_receipt BEFORE INSERT ON worker_receipts BEGIN SELECT RAISE(ABORT, 'deny receipt'); END")
	second := args
	second.Sequence = 2
	if _, err := db.MutateWorkerExecution(ctx, second); !isIntegrityError(err) || !strings.Contains(err.Error(), "deny receipt") {
		t.Fatalf("denied receipt %v", err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "s"), "input_tokens"), 13) {
		t.Fatal("rolled back usage landed")
	}
	mustExecConn(t, db.conn, "DROP TRIGGER deny_receipt")
	aux := scope
	aux.Sequence, aux.Operation, aux.Payload = 2, "usage.auxiliary", pyjson.ObjectOf("task", "compression", "model", "aux", "input_tokens", int64(3))
	mustReceipt(t, db, aux)
	mustReceipt(t, db, aux)
	if !pyjson.Equal(rowValue(mustSession(t, db, "s"), "input_tokens"), 13) || !pyjson.Equal(scalarOf(t, db.conn, "SELECT input_tokens FROM session_model_usage WHERE task='compression'"), 3) {
		t.Fatal("auxiliary usage rows")
	}
	bad := scope
	bad.Sequence, bad.Operation, bad.Payload = 3, "usage.main", pyjson.ObjectOf("input_tokens", int64(-1))
	if r := receiptRefusal(t, db, bad); r != "invalid_params" {
		t.Fatal(r)
	}
}

func TestWorkerVoiceTurnUsageCountsButNeverRewritesTheSessionRoute(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "s", Source: "cli", Model: "main-model"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "w", SessionID: "s", Generation: 0, Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	in := WorkerReceipt{Epoch: epoch, ExecutionID: "w", SessionID: "s", Generation: 0, Sequence: 1, Operation: "usage.main",
		Payload: pyjson.ObjectOf("input_tokens", int64(10), "api_call_count", int64(1), "model", "voice-model", "billing_provider", "voicep", "task", "voice_chat")}
	mustReceipt(t, db, in)
	row := mustSession(t, db, "s")
	if !pyjson.Equal(rowValue(row, "input_tokens"), 10) || !pyjson.Equal(rowValue(row, "model"), "main-model") {
		t.Fatalf("route rewritten %v", row)
	}
	byTask, err := db.AuxiliaryUsageByTask(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	voice, _ := byTask.Get("voice_chat")
	if voice == nil || !pyjson.Equal(rowValue(voice.(*pyjson.Object), "input_tokens"), 10) {
		t.Fatalf("per task row %v", byTask)
	}
}

func TestRotationPreservesAdmissionIdentityAndLostAckFifo(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "root", Source: "cli", SystemPrompt: "PREFIX"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	request := SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "root", RequestID: "original", Payload: admissionText("FIRST")}
	accepted, err := db.AdmitSessionInput(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	started, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "root"})
	if err != nil || started == nil {
		t.Fatalf("claim %v %v", started, err)
	}
	followerRequest := request
	followerRequest.RequestID, followerRequest.Payload = "follower", admissionText("SECOND")
	follower, err := db.AdmitSessionInput(ctx, followerRequest)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := exactInt(rowValue(started, "generation"))
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "admission-worker:" + objStr(accepted, "admission_id"), SessionID: "root", Generation: generation}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: scope.ExecutionID, SessionID: "root", Generation: generation, Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	store := newWorkerStore(t, db, scope)
	rpc := store.rpc
	for _, pair := range [][2]string{{"root", "child"}, {"child", "grandchild"}} {
		parent, child := pair[0], pair[1]
		if !mustBool(t)(store.tryAcquireCompressionLock(parent, "holder")) {
			t.Fatal("lock")
		}
		store.rpc = lostAckRPC(rpc, "lost publication reply")
		err := store.publishCompressionChild(parent, child, "cli", textMessages("assistant", "SUMMARY"), publishOptions{systemPrompt: "PREFIX", compressionLockHolder: "holder"})
		if err == nil || err.Error() != "lost publication reply" {
			t.Fatalf("lost ack %v", err)
		}
		if store.scope.SessionID != parent {
			t.Fatal("scope moved before the ack")
		}
		replay, err := db.AdmitSessionInput(ctx, request)
		if err != nil || objStr(replay, "admission_id") != objStr(accepted, "admission_id") {
			t.Fatalf("admission identity %v %v", replay, err)
		}
		if claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "root"}); err != nil || claim != nil {
			t.Fatalf("fifo unblocked %v %v", claim, err)
		}
		store.rpc = rpc
		if _, err := store.retryPending(); err != nil {
			t.Fatal(err)
		}
		if store.scope.SessionID != child {
			t.Fatalf("scope %s", store.scope.SessionID)
		}
		if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM sessions WHERE id=?", child), 1) {
			t.Fatal("child row")
		}
		row, _ := db.GetSessionAdmission(ctx, objStr(follower, "admission_id"))
		if objStr(row, "status") != "queued" {
			t.Fatalf("follower %v", row)
		}
	}
	row, _ := db.GetSessionAdmission(ctx, objStr(accepted, "admission_id"))
	if objStr(row, "target_session_id") != "root" {
		t.Fatalf("target %v", row)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT lineage_json FROM session_admissions WHERE admission_id=?", objStr(accepted, "admission_id")), `["root","child","grandchild"]`) {
		t.Fatal("lineage json")
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM session_admissions"), 2) {
		t.Fatal("admission count")
	}
}

func TestFifoStaysBlockedWhileItsWorkerLivesOnACompressionChild(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "root", Source: "cli", SystemPrompt: "PREFIX"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "root", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "root", Generation: 0, Kind: "compute", AdoptionSecret: "private", RequireIdle: true}); err != nil {
		t.Fatal(err)
	}
	store := newWorkerStore(t, db, scope)
	messages := textMessages("user", "kept")
	if _, err := store.appendMessagesBatch("root", messages); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.tryAcquireCompressionLock("root", "holder")) {
		t.Fatal("lock")
	}
	if err := store.publishCompressionChild("root", "child", "cli", messages, publishOptions{systemPrompt: "PREFIX", compressionLockHolder: "holder"}); err != nil {
		t.Fatal(err)
	}
	if store.scope.SessionID != "child" {
		t.Fatal("scope")
	}
	follower, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "root", RequestID: "follower", Payload: admissionText("next")})
	if err != nil {
		t.Fatal(err)
	}
	if claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "root"}); err != nil || claim != nil {
		t.Fatalf("fifo unblocked %v %v", claim, err)
	}
	if _, err := store.finish(); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "root"})
	if err != nil || claimed == nil || objStr(claimed, "admission_id") != objStr(follower, "admission_id") {
		t.Fatalf("follower claim %v %v", claimed, err)
	}
}

func TestCompactedMessagesKeepCurrentRowIDsAcrossRepeatedCompaction(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		if err := db.CreateSession(NewSession{ID: "root", Source: "cli", SystemPrompt: "PREFIX"}); err != nil {
			t.Fatal(err)
		}
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "root", Generation: 0}
		if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "root", Generation: 0, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
			t.Fatal(err)
		}
		store := newWorkerStore(t, db, scope)
		messages := textMessages("user", "retained current input")
		if _, err := store.appendMessagesBatch("root", messages); err != nil {
			t.Fatal(err)
		}
		for _, child := range []string{"child", "grandchild"} {
			parent := store.scope.SessionID
			oldID := messages[0]["_row_id"]
			if !mustBool(t)(store.tryAcquireCompressionLock(parent, "holder")) {
				t.Fatal("lock")
			}
			if rotate {
				if err := store.publishCompressionChild(parent, child, "cli", messages, publishOptions{systemPrompt: "PREFIX", compressionLockHolder: "holder"}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := store.archiveAndCompact(parent, messages, archiveOptions{lockHolder: "holder"}); err != nil {
				t.Fatal(err)
			}
			current := store.scope.SessionID
			if pyjson.Equal(messages[0]["_row_id"], oldID) {
				t.Fatalf("row id kept %v", oldID)
			}
			row, err := fetchOne(ctx, db.conn, "SELECT session_id,active FROM messages WHERE id=?", messages[0]["_row_id"])
			if err != nil || !pyjson.Equal(row, []any{current, int64(1)}) {
				t.Fatalf("row %v %v", row, err)
			}
		}
		db.Close()
	}
}

func TestOwnerMintedMessageUIDRoundTripsThroughAppendAndArchive(t *testing.T) {
	db, store := workerFixture(t)
	defer db.Close()
	messages := textMessages("user", "kept", "assistant", "reply")
	if _, err := store.appendMessagesBatch("owned", messages); err != nil {
		t.Fatal(err)
	}
	stored := func() []any {
		out := []any{}
		for _, row := range rowsOf(t, db, "SELECT message_uid FROM messages WHERE session_id=? AND active=1 ORDER BY id", "owned") {
			out = append(out, rowValue(row, "message_uid"))
		}
		return out
	}
	carried := func() []any {
		out := []any{}
		for _, m := range messages {
			out = append(out, m["message_uid"])
		}
		return out
	}
	first := stored()
	if !pyjson.Equal(carried(), first) {
		t.Fatalf("uids %v != %v", carried(), first)
	}
	for _, uid := range first {
		if !pyTruthy(uid) {
			t.Fatal("empty uid")
		}
	}
	for range 2 {
		if _, err := store.archiveAndCompact("owned", messages, archiveOptions{}); err != nil {
			t.Fatal(err)
		}
		if !pyjson.Equal(stored(), first) || !pyjson.Equal(carried(), first) {
			t.Fatalf("uids drifted %v %v %v", stored(), carried(), first)
		}
	}
}

func TestWatermarkedCompactionPreservesForeignTailAndRejectsStaleHolder(t *testing.T) {
	ctx := context.Background()
	db, store := workerFixture(t)
	defer db.Close()
	if _, err := store.appendMessagesBatch("owned", textMessages("user", "old", "assistant", "carry")); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "holder")) {
		t.Fatal("lock")
	}
	watermark, err := store.getActiveMessageWatermark("owned")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(ctx, "owned", []map[string]any{{"role": "user", "content": "foreign tail", "api_content": " EXACT ",
		"display_metadata": map[string]any{"_accepted_input_id": "input-tail"}}}, AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = db.MutateWorkerExecution(ctx, store.rawReceipt("compression.archive", map[string]any{
		"messages": []any{map[string]any{"role": "assistant", "content": "bad"}}, "model_config_patch": nil,
		"watermark": watermark, "lock_holder": "old", "tail_count": int64(0)}))
	if err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("stale holder %v", err)
	}
	archived, err := store.archiveAndCompact("owned", textMessages("assistant", "summary", "assistant", "carry"),
		archiveOptions{watermark: watermark, lockHolder: "holder", tailCount: 1, modelConfigPatch: map[string]any{"_proactive_prune_rearm_tokens": int64(42)}})
	if err != nil || !pyjson.Equal(archived, 3) {
		t.Fatalf("archive %v %v", archived, err)
	}
	rows, err := store.getMessagesAsConversation("owned", ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1].(*pyjson.Object)
	if !pyjson.Equal(rowValue(last, "content"), "foreign tail") || !pyjson.Equal(rowValue(last, "api_content"), " EXACT ") || !pyjson.Equal(rowValue(last, "display_metadata"), pyjson.ObjectOf("_accepted_input_id", "input-tail")) {
		t.Fatalf("tail %v", last)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM messages WHERE session_id='owned' AND compacted=1"), 1) {
		t.Fatal("compacted count")
	}
	if v, err := store.getSessionModelConfigValue("owned", "_proactive_prune_rearm_tokens"); err != nil || !pyjson.Equal(v, 42) {
		t.Fatalf("patch %v %v", v, err)
	}
}

func TestPublicationReplayAdvancesAssignmentOnceAndKeepsMarkers(t *testing.T) {
	ctx := context.Background()
	db, store := workerFixture(t)
	defer db.Close()
	if _, err := store.appendMessagesBatch("owned", textMessages("user", "before")); err != nil {
		t.Fatal(err)
	}
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "holder")) {
		t.Fatal("lock")
	}
	watermark, _ := store.getActiveMessageWatermark("owned")
	if _, err := db.AppendMessagesBatch(ctx, "owned", []map[string]any{{"role": "user", "content": "foreign", "api_content": " exact tail ",
		"display_metadata": map[string]any{"_accepted_input_id": "delivery"}}}, AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	original := store.rpc
	store.rpc = lostAckRPC(original, "lost-ack")
	err := store.publishCompressionChild("owned", "child", "cli", textMessages("assistant", "summary"),
		publishOptions{systemPrompt: "prefix", compressionLockHolder: "holder", watermark: watermark})
	if err == nil || err.Error() != "lost-ack" {
		t.Fatalf("lost ack %v", err)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT session_id FROM worker_executions"), "child") || !pyjson.Equal(rowValue(mustSession(t, db, "owned"), "end_reason"), "compression") {
		t.Fatal("publication did not land")
	}
	store.rpc = original
	if _, err := store.retryPending(); err != nil {
		t.Fatal(err)
	}
	if store.scope.SessionID != "child" {
		t.Fatal("scope")
	}
	if _, err := store.appendMessagesBatch("child", textMessages("assistant", "after")); err != nil {
		t.Fatal(err)
	}
	rows, err := store.getMessagesAsConversation("child", ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contents := []any{}
	for _, r := range rows {
		contents = append(contents, rowValue(r.(*pyjson.Object), "content"))
	}
	if !pyjson.Equal(contents, []any{"summary", "foreign", "after"}) {
		t.Fatalf("contents %v", contents)
	}
	second := rows[1].(*pyjson.Object)
	if !pyjson.Equal(rowValue(second, "api_content"), " exact tail ") || !pyjson.Equal(rowValue(second, "display_metadata"), pyjson.ObjectOf("_accepted_input_id", "delivery")) {
		t.Fatalf("markers %v", second)
	}
	lineage, err := store.lineageField("child", "lineage")
	if err != nil || !pyjson.Equal(lineage, []any{"owned", "child"}) {
		t.Fatalf("lineage %v %v", lineage, err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "child"), "system_prompt"), "prefix") {
		t.Fatal("child prompt")
	}
}

func TestInPlaceArchiveWithoutALeasePersistsFromAWorker(t *testing.T) {
	db, store := workerFixture(t)
	defer db.Close()
	if _, err := store.appendMessagesBatch("owned", textMessages("user", "old", "assistant", "long")); err != nil {
		t.Fatal(err)
	}
	archived, err := store.archiveAndCompact("owned", textMessages("assistant", "pruned"), archiveOptions{})
	if err != nil || !pyjson.Equal(archived, 1) {
		t.Fatalf("archive %v %v", archived, err)
	}
	if store.failure != "" || len(store.pending) != 0 {
		t.Fatal("journal stuck")
	}
	if got := conversationContents(t, db, "owned"); !pyjson.Equal(anyStrings(got), []any{"pruned"}) {
		t.Fatalf("contents %v", got)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM messages WHERE session_id='owned' AND compacted=1"), 2) {
		t.Fatal("compacted count")
	}
	if _, err := store.appendMessagesBatch("owned", textMessages("user", "after")); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.getMessagesAsConversation("owned", ConversationOptions{})
	contents := []any{}
	for _, r := range rows {
		contents = append(contents, rowValue(r.(*pyjson.Object), "content"))
	}
	if !pyjson.Equal(contents, []any{"pruned", "after"}) {
		t.Fatalf("contents %v", contents)
	}
}

func TestHistoryAndLineageKeepCacheScopeDistinctFromAttribution(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	for _, n := range []NewSession{
		{ID: "root", Source: "cli", SystemPrompt: "root prefix"},
		{ID: "child", Source: "cli", ParentSessionID: "root", ModelConfig: map[string]any{"_delegate_from": "root"}, SystemPrompt: "child prefix"},
	} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EndSession("child", "compression"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []NewSession{
		{ID: "tip", Source: "cli", ParentSessionID: "child", ModelConfig: map[string]any{"_delegate_from": "root"}, SystemPrompt: "child prefix"},
		{ID: "foreign", Source: "cli", SystemPrompt: "secret"},
	} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AppendMessagesBatch(ctx, "tip", []map[string]any{
		{"role": "user", "content": "canonical", "api_content": " EXACT\nWIRE ", "platform_message_id": "delivery"},
		{"role": "assistant", "content": "answer", "reasoning": "reason", "codex_message_items": []any{map[string]any{"type": "message"}}}}, AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "fixture")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "tip", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "tip", Generation: 0, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	store := newWorkerStore(t, db, scope)
	viaStore, err := store.getMessagesAsConversation("tip", ConversationOptions{IncludeRowIDs: true})
	direct, derr := db.GetMessagesAsConversation(ctx, "tip", ConversationOptions{IncludeRowIDs: true})
	if err != nil || derr != nil || !pyjson.Equal(viaStore, jsonRoundTrip(anyMessages(direct))) {
		t.Fatalf("history %v %v %v %v", viaStore, err, direct, derr)
	}
	lineage, _ := store.lineageField("tip", "lineage")
	directLineage, _ := db.GetCompressionLineage(ctx, "tip")
	if !pyjson.Equal(lineage, anyStrings(directLineage)) || !pyjson.Equal(lineage, []any{"child", "tip"}) {
		t.Fatalf("lineage %v %v", lineage, directLineage)
	}
	if root, _ := store.lineageField("tip", "root"); root != "root" {
		t.Fatalf("root %v", root)
	}
	rootRow, err := store.getSession("root")
	if err != nil || !pyjson.Equal(rowValue(rootRow, "system_prompt"), "root prefix") {
		t.Fatalf("root read %v %v", rootRow, err)
	}
	tip, _ := store.lineageField("child", "tip")
	directTip, _ := db.GetCompressionTip(ctx, "child")
	if tip != directTip {
		t.Fatalf("tip %v %v", tip, directTip)
	}
	resume, _ := store.lineageField("child", "resume")
	directResume, _ := db.ResolveResumeSessionID("child")
	if resume != directResume {
		t.Fatalf("resume %v %v", resume, directResume)
	}
	identity, _ := store.lineageField("child", "identity")
	if !pyjson.Equal(identity, []any{true, "cli"}) {
		t.Fatalf("identity %v", identity)
	}
	seq := store.nextSequence
	if r := receiptRefusal(t, db, store.rawReceipt("compression.context", map[string]any{"target": "foreign"})); r != "permission_denied" {
		t.Fatal(r)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT last_sequence FROM worker_executions"), seq-1) {
		t.Fatal("refused read advanced the sequence")
	}
}

func TestDeclaredCacheBoundaryMatchesLocalDatabase(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "owned", Source: "cli", SessionKey: "chat"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("owned", "session_reset"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(NewSession{ID: "next", Source: "cli", SessionKey: "chat"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "fixture")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "next", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "next", Generation: 0, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	store := newWorkerStore(t, db, scope)
	boundary, err := store.latestConversationBoundary("chat", "cli")
	if err != nil {
		t.Fatal(err)
	}
	generation, ok, err := db.LatestConversationBoundary(ctx, "chat", "cli")
	if err != nil {
		t.Fatal(err)
	}
	var direct any
	if ok {
		direct = generation
	}
	if !pyjson.Equal(boundary, direct) {
		t.Fatalf("boundary %v != %v", boundary, direct)
	}
	if _, err := store.latestConversationBoundary("foreign-chat", "cli"); refusalReason(t, err) != "permission_denied" {
		t.Fatal(err)
	}
}

func TestTerminalWorkerKeepsNoFullHistoryReceipt(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", strings.Repeat("x", 2000)), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "fixture")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "s", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "s", Generation: 0, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	read := func(includeRowIDs bool) *pyjson.Object {
		return pyjson.ObjectOf("target", "s", "include_ancestors", false, "include_inactive", false, "repair_alternation", false,
			"include_row_ids", includeRowIDs, "include_compacted", false)
	}
	history := scope
	history.Sequence, history.Operation, history.Payload = 1, "compression.history", read(false)
	result := mustReceipt(t, db, history)
	first := resultValue(result, "messages").([]any)[0].(*pyjson.Object)
	if !pyjson.Equal(rowValue(first, "content"), strings.Repeat("x", 2000)) {
		t.Fatalf("history %v", first)
	}
	mustReceipt(t, db, finishReceipt(scope, 2))
	stored := rowsOf(t, db, "SELECT sequence, LENGTH(result_json) AS size FROM worker_receipts ORDER BY sequence")
	if size, _ := exactInt(rowValue(stored[0], "size")); size >= 200 {
		t.Fatalf("a terminal execution kept a full-history receipt (%d bytes)", size)
	}
	other := history
	other.Payload = read(true)
	if r := receiptRefusal(t, db, other); r != "admission_conflict" {
		t.Fatal(r)
	}
	if r := receiptRefusal(t, db, history); r != "stale_generation" {
		t.Fatal(r)
	}
	if again := mustReceipt(t, db, finishReceipt(scope, 2)); !pyjson.Equal(again, pyjson.ObjectOf("status", "terminal")) {
		t.Fatalf("finish replay %v", again)
	}
}

func TestPublicationMovesAutomationOnceAndPreservesClearedControls(t *testing.T) {
	ctx := context.Background()
	db, store := workerFixture(t)
	defer db.Close()
	values := map[string]*pyjson.Object{
		"goal":      pyjson.ObjectOf("goal", "retain objective", "status", "paused"),
		"loop":      pyjson.ObjectOf("prompt", "retain loop", "status", "active", "interval_seconds", int64(60)),
		"heartbeat": pyjson.ObjectOf("status", "active", "interval_seconds", int64(120)),
	}
	for family, value := range values {
		text, _ := pyjson.Dumps(value)
		if err := db.SetMeta(ctx, family+":owned", text); err != nil {
			t.Fatal(err)
		}
	}
	if !mustBool(t)(store.tryAcquireCompressionLock("owned", "compressor")) {
		t.Fatal("lock")
	}
	if err := store.publishCompressionChild("owned", "child", "cli", textMessages("assistant", "summary"), publishOptions{compressionLockHolder: "compressor"}); err != nil {
		t.Fatal(err)
	}
	meta := func(key string) *pyjson.Object {
		raw, ok, err := db.GetMeta(ctx, key)
		if err != nil || !ok {
			t.Fatalf("%s missing %v", key, err)
		}
		decoded, err := pyjson.Loads(raw)
		if err != nil {
			t.Fatal(err)
		}
		return decoded.(*pyjson.Object)
	}
	for family, value := range values {
		want, _ := value.Get("status")
		if !pyjson.Equal(rowValue(meta(family+":child"), "status"), want) {
			t.Fatalf("%s child status", family)
		}
		if !pyjson.Equal(rowValue(meta(family+":owned"), "status"), "cleared") {
			t.Fatalf("%s parent not cleared", family)
		}
	}
	if !pyjson.Equal(rowValue(meta("goal:child"), "goal"), "retain objective") || !pyjson.Equal(rowValue(meta("loop:child"), "prompt"), "retain loop") {
		t.Fatal("moved payloads")
	}
}
