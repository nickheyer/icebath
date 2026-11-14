package state

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store with session s and a boot epoch
func mutationStore(t *testing.T) (*SessionDB, string, int64) {
	t.Helper()
	path := t.TempDir() + "/state.db"
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(context.Background(), "boot")
	if err != nil {
		t.Fatal(err)
	}
	return db, path, epoch
}

func mustCreate(t *testing.T, db *SessionDB, n NewSession) {
	t.Helper()
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
}

func TestMutationReceiptFencesReplayRevisionAndEpoch(t *testing.T) {
	for _, c := range []struct {
		operation string
		payload   *pyjson.Object
	}{
		{"rename", pyjson.ObjectOf("title", " New  name ")},
		{"rename", pyjson.ObjectOf("title", "a\xed\xa0\x80b")},
		{"archive", pyjson.ObjectOf("archived", true)},
	} {
		ctx := context.Background()
		db, path, epoch := mutationStore(t)
		args := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "edit", ExpectedRevision: 0, Operation: c.operation, Payload: c.payload}
		expectedTitle := ""
		if c.operation == "rename" {
			mustCreate(t, db, NewSession{ID: "direct", Source: "test"})
			title := pyStr(rowValue(c.payload, "title"))
			if _, err := db.SetSessionTitle(ctx, "direct", title); err != nil {
				t.Fatal(err)
			}
			expectedTitle, _, _ = db.GetSessionTitle(ctx, "direct")
			if _, err := db.SetSessionTitle(ctx, "direct", ""); err != nil {
				t.Fatal(err)
			}
		}
		result := mustMutate(t, db, args)
		if c.operation == "rename" {
			if !pyjson.Equal(rowValue(result, "title"), expectedTitle) {
				t.Fatalf("title %v != %q", rowValue(result, "title"), expectedTitle)
			}
			if pyStr(rowValue(c.payload, "title")) != expectedTitle {
				changed := args
				changed.Payload = pyjson.ObjectOf("title", expectedTitle)
				if r := mutateRefusal(t, db, changed); r != "admission_conflict" {
					t.Fatal(r)
				}
			}
		}
		if !pyjson.Equal(rowValue(result, "revision"), 1) {
			t.Fatalf("revision %v", result)
		}
		if again := mustMutate(t, db, args); !pyjson.Equal(again, result) {
			t.Fatalf("replay %v", again)
		}
		if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "runtime_revision"), 1) {
			t.Fatal("revision column")
		}
		moved := args
		moved.ExpectedRevision = 1
		if r := mutateRefusal(t, db, moved); r != "admission_conflict" {
			t.Fatal(r)
		}
		other := args
		other.RequestID = "other"
		if r := mutateRefusal(t, db, other); r != "revision_conflict" {
			t.Fatal(r)
		}
		peer := mustSessionDB(t, path, false)
		peerArgs := args
		peerArgs.RequestID = "peer"
		if r := mutateRefusal(t, peer, peerArgs); r != "revision_conflict" {
			t.Fatal(r)
		}
		peerArgs.ExpectedRevision = 1
		if later := mustMutate(t, peer, peerArgs); !pyjson.Equal(rowValue(later, "revision"), 2) {
			t.Fatalf("peer revision %v", later)
		}
		if again := mustMutate(t, db, args); !pyjson.Equal(again, result) {
			t.Fatalf("replay after peer %v", again)
		}
		if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "runtime_revision"), 2) {
			t.Fatal("revision column after peer")
		}
		peer.Close()
		newEpoch, _ := db.BeginRuntimeEpoch(ctx, "replacement")
		if r := mutateRefusal(t, db, args); r != "stale_epoch" {
			t.Fatal(r)
		}
		fresh := args
		fresh.Epoch = newEpoch
		if again := mustMutate(t, db, fresh); !pyjson.Equal(again, result) {
			t.Fatalf("replay under the new epoch %v", again)
		}
		db.Close()
		db = mustSessionDB(t, path, false)
		if again := mustMutate(t, db, fresh); !pyjson.Equal(again, result) {
			t.Fatalf("replay after reopen %v", again)
		}
		db.Close()
	}
}

// True for a ValueError whose text carries the fragment
func valueErrorContains(err error, fragment string) bool {
	return err != nil && isPyValueError(err) && strings.Contains(err.Error(), fragment)
}

func TestMutationRulesAndReceiptFailureAreAtomic(t *testing.T) {
	for _, c := range []struct {
		operation string
		payload   *pyjson.Object
	}{
		{"rename", pyjson.ObjectOf("title", " New  name ")},
		{"archive", pyjson.ObjectOf("archived", true)},
	} {
		ctx := context.Background()
		db, _, epoch := mutationStore(t)
		if _, err := db.SetSessionTitle(ctx, "s", "Original"); err != nil {
			t.Fatal(err)
		}
		args := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "edit", ExpectedRevision: 0, Operation: c.operation, Payload: c.payload}
		before := sessionOf(t, db, "s")
		mustExecConn(t, db.conn, "CREATE TRIGGER refuse_receipt BEFORE INSERT ON state_meta WHEN NEW.key LIKE 'gateway.mutation.%' BEGIN SELECT RAISE(ABORT, 'receipt refused'); END")
		if _, err := db.MutateRuntimeSession(ctx, args); !isIntegrityError(err) || !strings.Contains(err.Error(), "receipt refused") {
			t.Fatalf("refused receipt %v", err)
		}
		if !pyjson.Equal(sessionOf(t, db, "s"), before) {
			t.Fatal("refused receipt changed the row")
		}
		mustExecConn(t, db.conn, "DROP TRIGGER refuse_receipt")
		if c.operation == "rename" {
			mustCreate(t, db, NewSession{ID: "bot", Source: "test"})
			if _, err := db.SetSessionTitle(ctx, "bot", CanonicalBotChatTitle); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SetSessionHidden(ctx, "bot", true); err != nil {
				t.Fatal(err)
			}
			bot := args
			bot.SessionID = "bot"
			if _, err := db.MutateRuntimeSession(ctx, bot); !valueErrorContains(err, "canonical Bot Chat") {
				t.Fatalf("bot rename %v", err)
			}
			mustCreate(t, db, NewSession{ID: "collision", Source: "test"})
			if _, err := db.SetSessionTitle(ctx, "collision", "New name"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.MutateRuntimeSession(ctx, args); !valueErrorContains(err, "already in use") {
				t.Fatalf("collision %v", err)
			}
			if _, err := db.SetSessionTitle(ctx, "collision", ""); err != nil {
				t.Fatal(err)
			}
			mustMutate(t, db, args)
			if title, _, _ := db.GetSessionTitle(ctx, "s"); title != "New name" {
				t.Fatalf("title %q", title)
			}
			if source, _, _ := db.GetSessionTitleSource(ctx, "s"); source != TitleSourceUser {
				t.Fatalf("source %q", source)
			}
			if err := db.EndSession("s", "compression"); err != nil {
				t.Fatal(err)
			}
			mustCreate(t, db, NewSession{ID: "tip", Source: "test", ParentSessionID: "s"})
			transfer := args
			transfer.SessionID, transfer.RequestID = "tip", "transfer"
			mustMutate(t, db, transfer)
			if _, has, _ := db.GetSessionTitle(ctx, "s"); has {
				t.Fatal("title stayed on the parent")
			}
			if title, _, _ := db.GetSessionTitle(ctx, "tip"); title != "New name" {
				t.Fatalf("tip title %q", title)
			}
			if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "runtime_revision"), 2) || !pyjson.Equal(rowValue(sessionOf(t, db, "tip"), "runtime_revision"), 1) {
				t.Fatal("revisions")
			}
		} else {
			mustCreate(t, db, NewSession{ID: "child", Source: "test", ParentSessionID: "s"})
			if err := db.EndSession("s", "compression"); err != nil {
				t.Fatal(err)
			}
			mustMutate(t, db, args)
			if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "archived"), 1) || !pyjson.Equal(rowValue(sessionOf(t, db, "child"), "archived"), 1) {
				t.Fatal("archive flags")
			}
			if !pyjson.Equal(rowValue(sessionOf(t, db, "child"), "runtime_revision"), 1) {
				t.Fatal("child revision")
			}
		}
		db.Close()
	}
}

func TestSidebarEditIsAtomicAndRevisionFenced(t *testing.T) {
	ctx := context.Background()
	db, path, epoch := mutationStore(t)
	defer db.Close()
	args := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "edit", ExpectedRevision: 0, Operation: "sidebar",
		Payload: pyjson.ObjectOf("title", "Title", "pinned", true, "hidden", true, "unread", true)}
	mustExecConn(t, db.conn, "CREATE TRIGGER fail_receipt BEFORE INSERT ON state_meta WHEN NEW.key LIKE 'gateway.mutation.%' BEGIN SELECT RAISE(ABORT, 'receipt failed'); END")
	if _, err := db.MutateRuntimeSession(ctx, args); !isIntegrityError(err) || !strings.Contains(err.Error(), "receipt failed") {
		t.Fatalf("failed receipt %v", err)
	}
	row := sessionOf(t, db, "s")
	if rowValue(row, "title") != nil || pyTruthy(rowValue(row, "pinned")) {
		t.Fatal("failed receipt changed the row")
	}
	mustExecConn(t, db.conn, "DROP TRIGGER fail_receipt")
	receipt := mustMutate(t, db, args)
	if !pyjson.Equal(rowValue(receipt, "revision"), 1) || !pyTruthy(rowValue(receipt, "pinned")) || !pyTruthy(rowValue(receipt, "hidden")) || !pyTruthy(rowValue(receipt, "unread")) {
		t.Fatalf("receipt %v", receipt)
	}
	peer := mustSessionDB(t, path, false)
	defer peer.Close()
	other := args
	other.PrincipalID = "peer"
	if r := mutateRefusal(t, peer, other); r != "revision_conflict" {
		t.Fatal(r)
	}
	if again := mustMutate(t, peer, args); !pyjson.Equal(again, receipt) {
		t.Fatalf("peer replay %v", again)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "runtime_revision"), 1) {
		t.Fatal("revision column")
	}
}

func TestImportCannotReuseADeletedReceiptIdentity(t *testing.T) {
	installRetirementHooks(t)
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(context.Background(), "owner")
	mustCreate(t, db, NewSession{ID: "s", Source: "cli"})
	del := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "delete", ExpectedRevision: 0, ExpectedGeneration: int64Ptr(0), Operation: "delete", Payload: pyjson.NewObject()}
	deleted := mustMutate(t, db, del)
	imp := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "new", RequestID: "import", ExpectedRevision: 0, Operation: "import",
		Payload: pyjson.ObjectOf("sessions", []any{pyjson.ObjectOf("id", "new", "source", "cli"), pyjson.ObjectOf("id", "s", "source", "cli")})}
	if r := mutateRefusal(t, db, imp); r != "admission_conflict" {
		t.Fatal(r)
	}
	if sessionOf(t, db, "s") != nil || sessionOf(t, db, "new") != nil {
		t.Fatal("import wrote rows")
	}
	if again := mustMutate(t, db, del); !pyjson.Equal(again, deleted) {
		t.Fatalf("delete replay %v", again)
	}
}

func TestImportRollsBackRowsWithReceiptFailure(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	args := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "first", RequestID: "import", ExpectedRevision: 0, Operation: "import",
		Payload: pyjson.ObjectOf("sessions", []any{
			pyjson.ObjectOf("id", "first", "source", "cli", "messages", []any{pyjson.ObjectOf("role", "user", "content", "keep")}),
			pyjson.ObjectOf("id", "second", "source", "cli", "parent_session_id", "first", "messages", []any{})})}
	mustExecConn(t, db.conn, "CREATE TRIGGER fail_receipt BEFORE INSERT ON state_meta WHEN NEW.key LIKE 'gateway.mutation.%' BEGIN SELECT RAISE(ABORT, 'receipt failed'); END")
	if _, err := db.MutateRuntimeSession(ctx, args); !isIntegrityError(err) || !strings.Contains(err.Error(), "receipt failed") {
		t.Fatalf("failed receipt %v", err)
	}
	if sessionOf(t, db, "first") != nil || sessionOf(t, db, "second") != nil {
		t.Fatal("rolled back import left rows")
	}
	mustExecConn(t, db.conn, "DROP TRIGGER fail_receipt")
	result := mustMutate(t, db, args)
	if !pyjson.Equal(rowValue(result, "imported_ids"), []any{"first", "second"}) {
		t.Fatalf("imported %v", result)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "second"), "parent_session_id"), "first") {
		t.Fatal("parent link")
	}
	if got := contentsOf(t, db, "first", GetMessagesOptions{}); !pyjson.Equal(got, []any{"keep"}) {
		t.Fatalf("messages %v", got)
	}
	if again := mustMutate(t, db, args); !pyjson.Equal(again, result) {
		t.Fatalf("replay %v", again)
	}
	empty := args
	empty.Payload = pyjson.ObjectOf("sessions", []any{})
	if r := mutateRefusal(t, db, empty); r != "admission_conflict" {
		t.Fatal(r)
	}
}

func TestImportNeverOverwritesExistingLiveRows(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "live", Source: "test"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	admitInput(t, db, epoch, "human", "live", "queued", admissionText("keep"))
	before := sessionOf(t, db, "live")
	result := mustMutate(t, db, RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "new", RequestID: "import", ExpectedRevision: 0, Operation: "import",
		Payload: pyjson.ObjectOf("sessions", []any{
			pyjson.ObjectOf("id", "new", "source", "cli", "messages", []any{}),
			pyjson.ObjectOf("id", "live", "source", "cli", "title", "Overwrite", "messages", []any{})})})
	if !pyjson.Equal(rowValue(result, "skipped_ids"), []any{"live"}) {
		t.Fatalf("skipped %v", result)
	}
	if !pyjson.Equal(sessionOf(t, db, "live"), before) {
		t.Fatal("live row changed")
	}
	admissions, err := db.ListSessionAdmissions(ctx, "live", true)
	if err != nil || len(admissions) == 0 || objStr(admissions[0], "status") != "queued" {
		t.Fatalf("admissions %v %v", admissions, err)
	}
}

func TestDeleteRefusesOutstandingDescendantObligations(t *testing.T) {
	installRetirementHooks(t)
	for _, obligation := range []string{"queued", "started", "unknown", "worker"} {
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		mustCreate(t, db, NewSession{ID: "s", Source: "test"})
		mustCreate(t, db, NewSession{ID: "child", Source: "test", ParentSessionID: "s", ModelConfig: map[string]any{"_delegate_from": "s"}})
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		if obligation == "worker" {
			if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "w", SessionID: "child", Generation: 0, Kind: "child", AdoptionSecret: "private"}); err != nil {
				t.Fatal(err)
			}
		} else {
			admitInput(t, db, epoch, "human", "child", "input", admissionText("keep"))
			if obligation != "queued" {
				claimHead(t, db, epoch, "child")
			}
			if obligation == "unknown" {
				epoch, _ = db.BeginRuntimeEpoch(ctx, "replacement")
				if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
					t.Fatal(err)
				}
			}
		}
		args := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "delete", ExpectedRevision: 0, ExpectedGeneration: int64Ptr(0), Operation: "delete", Payload: pyjson.NewObject()}
		before := sessionOf(t, db, "child")
		if r := mutateRefusal(t, db, args); r != "session_busy" && r != "unknown_execution" {
			t.Fatalf("%s -> %s", obligation, r)
		}
		if !pyjson.Equal(sessionOf(t, db, "child"), before) || sessionOf(t, db, "s") == nil {
			t.Fatal("refused delete changed rows")
		}
		db.Close()
	}
}

func TestDeleteReceiptSurvivesRemovalAndFencesGeneration(t *testing.T) {
	installRetirementHooks(t)
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	mustCreate(t, db, NewSession{ID: "branch", Source: "test", ParentSessionID: "s"})
	epoch, _ := db.BeginRuntimeEpoch(context.Background(), "owner")
	args := RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "delete", ExpectedRevision: 0, ExpectedGeneration: int64Ptr(1), Operation: "delete", Payload: pyjson.NewObject()}
	if r := mutateRefusal(t, db, args); r != "stale_generation" {
		t.Fatal(r)
	}
	args.ExpectedGeneration = int64Ptr(0)
	receipt := mustMutate(t, db, args)
	if !pyjson.Equal(rowValue(receipt, "deleted_ids"), []any{"s"}) || sessionOf(t, db, "s") != nil {
		t.Fatalf("delete %v", receipt)
	}
	if rowValue(sessionOf(t, db, "branch"), "parent_session_id") != nil {
		t.Fatal("branch kept its parent")
	}
	if again := mustMutate(t, db, args); !pyjson.Equal(again, receipt) {
		t.Fatalf("replay %v", again)
	}
	args.ExpectedGeneration = int64Ptr(1)
	if r := mutateRefusal(t, db, args); r != "admission_conflict" {
		t.Fatal(r)
	}
}

// Publishes a compression child of parent under holder, the direct store rotation
func publishChild(t *testing.T, db *SessionDB, parent, child, source, holder string, modelConfig map[string]any, messages []map[string]any) {
	t.Helper()
	p := NewCompressionChildPublish(parent, child, source, messages)
	p.CompressionLockHolder = &holder
	if modelConfig != nil {
		cfg := pyjson.NewObject()
		for k, v := range modelConfig {
			cfg.Set(k, v)
		}
		p.ModelConfig = cfg
	}
	if err := db.PublishCompressionChild(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteOfCompressedLogicalRootRetiresEveryPhysicalContinuation(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "root", Source: "telegram", SessionKey: "agent:telegram:dm:1"})
	if _, err := db.AppendMessage(ctx, "root", map[string]any{"role": "user", "content": "before compression"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if !db.TryAcquireCompressionLock("root", "holder", 300) {
		t.Fatal("lock")
	}
	publishChild(t, db, "root", "child", "telegram", "holder", nil, textMessages("user", "summary"))
	if _, err := db.AppendMessage(ctx, "child", map[string]any{"role": "user", "content": "after compression"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	entry, _ := pyjson.Dumps(pyjson.ObjectOf("session_id", "child", "session_key", "agent:telegram:dm:1"))
	if err := db.SaveGatewayRoutingEntry(ctx, "agent:telegram:dm:1", entry, ""); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	receipt := mustMutate(t, db, RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "root", RequestID: "delete", ExpectedRevision: 0, ExpectedGeneration: int64Ptr(0), Operation: "delete", Payload: pyjson.NewObject()})
	if !deletedIDsAre(idSet(rowValue(receipt, "deleted_ids")), "root", "child") {
		t.Fatalf("deleted %v", receipt)
	}
	if sessionOf(t, db, "child") != nil {
		t.Fatal("child survived")
	}
	conversation, err := db.GetMessagesAsConversation(ctx, "child", ConversationOptions{})
	if err != nil || len(conversation) != 0 {
		t.Fatalf("conversation %v %v", conversation, err)
	}
	routes, err := db.LoadGatewayRoutingEntries(ctx, "")
	if err != nil || len(routes) != 0 {
		t.Fatalf("routes %v %v", routes, err)
	}
}

// Publishes child from parent under a lock released afterwards
func compressed(t *testing.T, db *SessionDB, parent, child string, modelConfig map[string]any) {
	t.Helper()
	if !db.TryAcquireCompressionLock(parent, "holder", 300) {
		t.Fatal("lock")
	}
	publishChild(t, db, parent, child, "cli", "holder", modelConfig, textMessages("user", "history of "+child))
	db.ReleaseCompressionLock(parent, "holder")
}

// Root, a delegate or inherited fork and its compression continuations, returning the tip and the chain
func delegateTopology(t *testing.T, db *SessionDB, shape string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	mustCreate(t, db, NewSession{ID: "root", Source: "telegram"})
	if shape == "inherited-branch" {
		mustCreate(t, db, NewSession{ID: "ancestor", Source: "telegram"})
		if err := db.writeSQL(ctx, "UPDATE sessions SET parent_session_id='ancestor', model_config=? WHERE id='root'", []any{`{"_branched_from": "ancestor"}`}, false, nil); err != nil {
			t.Fatal(err)
		}
		compressed(t, db, "root", "tip", map[string]any{"_branched_from": "ancestor"})
		return "tip", []string{"root", "tip"}
	}
	mustCreate(t, db, NewSession{ID: "delegate", Source: "cli", ParentSessionID: "root", ModelConfig: map[string]any{"_delegate_from": "root"}})
	var cfg map[string]any
	if shape == "inherited-delegate" {
		cfg = map[string]any{"_delegate_from": "root"}
	}
	compressed(t, db, "delegate", "tip", cfg)
	compressed(t, db, "tip", "tip2", nil)
	return "tip2", []string{"root", "delegate", "tip", "tip2"}
}

var topologyShapes = []string{"inherited-delegate", "unmarked-delegate", "inherited-branch"}

func TestQueuedWorkOnADelegateOrForkContinuationBlocksTheDelete(t *testing.T) {
	installRetirementHooks(t)
	for _, shape := range topologyShapes {
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		tip, _ := delegateTopology(t, db, shape)
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		admitInput(t, db, epoch, "human", tip, "q", admissionText("keep"))
		before := rowsOf(t, db, "SELECT id, parent_session_id FROM sessions ORDER BY id")
		if r := mutateRefusal(t, db, RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "root", RequestID: "delete", ExpectedRevision: 0, ExpectedGeneration: int64Ptr(0), Operation: "delete", Payload: pyjson.NewObject()}); r != "session_busy" {
			t.Fatalf("%s -> %s", shape, r)
		}
		after := rowsOf(t, db, "SELECT id, parent_session_id FROM sessions ORDER BY id")
		if !pyjson.Equal(anyObjects(after), anyObjects(before)) {
			t.Fatalf("%s rows changed", shape)
		}
		db.Close()
	}
}

func TestIdleDeleteTakesEveryDelegateAndForkContinuation(t *testing.T) {
	installRetirementHooks(t)
	for _, shape := range topologyShapes {
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		_, chain := delegateTopology(t, db, shape)
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		receipt := mustMutate(t, db, RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "root", RequestID: "delete", ExpectedRevision: 0, ExpectedGeneration: int64Ptr(0), Operation: "delete", Payload: pyjson.NewObject()})
		if !deletedIDsAre(idSet(rowValue(receipt, "deleted_ids")), chain...) {
			t.Fatalf("%s deleted %v", shape, receipt)
		}
		for _, sid := range chain {
			if sessionOf(t, db, sid) != nil {
				t.Fatalf("%s survived", sid)
			}
		}
		if shape == "inherited-branch" && sessionOf(t, db, "ancestor") == nil {
			t.Fatal("ancestor deleted")
		}
		db.Close()
	}
}

// Rewind mutation of s to a target row
func rewindTo(db *SessionDB, epoch int64, target any, requestID string) RuntimeMutation {
	return RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: requestID, ExpectedRevision: 0, ExpectedGeneration: int64Ptr(0),
		Operation: "rewind", Payload: pyjson.ObjectOf("target_message_id", target)}
}

func TestRewindHasExistingSemanticsWithAtomicRetry(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "first", "assistant", "reply", "user", "again"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(ctx, "s", GetMessagesOptions{})
	target := rowValue(rows[len(rows)-1], "id")
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	args := rewindTo(db, epoch, target, "rewind")
	receipt := mustMutate(t, db, args)
	message, _ := rowValue(receipt, "target_message").(*pyjson.Object)
	if !pyjson.Equal(rowValue(receipt, "rewound_count"), 1) || message == nil || !pyjson.Equal(rowValue(message, "content"), "again") {
		t.Fatalf("receipt %v", receipt)
	}
	if got := contentsOf(t, db, "s", GetMessagesOptions{}); !pyjson.Equal(got, []any{"first", "reply"}) {
		t.Fatalf("contents %v", got)
	}
	if count, _ := db.MessageCount(ctx, "s"); count != 3 {
		t.Fatalf("message count %d", count)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "runtime_generation"), 1) {
		t.Fatal("generation")
	}
	if again := mustMutate(t, db, args); !pyjson.Equal(again, receipt) {
		t.Fatalf("replay %v", again)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "rewind_count"), 1) {
		t.Fatal("rewind_count")
	}
}

func TestRewindRefusesQueuedWork(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "user", "content": "keep"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(ctx, "s", GetMessagesOptions{})
	target := rowValue(rows[0], "id")
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	admitInput(t, db, epoch, "human", "s", "input", admissionText("queued"))
	if r := mutateRefusal(t, db, rewindTo(db, epoch, target, "rewind")); r != "session_busy" {
		t.Fatal(r)
	}
	if got := contentsOf(t, db, "s", GetMessagesOptions{}); !pyjson.Equal(got, []any{"keep"}) {
		t.Fatalf("contents %v", got)
	}
}

// Q1 to A2 archived by in place compaction then Q3 and A3, the shown contents by id and the epoch
func compactedSession(t *testing.T, db *SessionDB) (map[string]any, int64) {
	t.Helper()
	ctx := context.Background()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "Q1", "assistant", "A1", "user", "Q2", "assistant", "A2"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArchiveAndCompact(ctx, "s", textMessages("user", "[CONTEXT SUMMARY] Q1..A2", "assistant", "ok, continuing"), ArchiveCompactOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "Q3", "assistant", "A3"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	shown := map[string]any{}
	rows, _ := db.GetMessages(ctx, "s", GetMessagesOptions{IncludeCompacted: true})
	for _, row := range rows {
		shown[pyStr(rowValue(row, "content"))] = rowValue(row, "id")
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	return shown, epoch
}

// Refusal field and detail of a rewind to a target that must be refused invalid_params
func rewindRefusalDetail(t *testing.T, db *SessionDB, epoch int64, target any, requestID string) string {
	t.Helper()
	_, err := db.MutateRuntimeSession(context.Background(), rewindTo(db, epoch, target, requestID))
	var refusal *RuntimeStoreError
	if r := refusalReason(t, err); r != "invalid_params" {
		t.Fatalf("%s -> %s", requestID, r)
	}
	refusal = err.(*RuntimeStoreError)
	if refusal.Field == nil || *refusal.Field != "target_message_id" {
		t.Fatalf("field %v", refusal.Field)
	}
	if refusal.Detail == nil {
		return ""
	}
	return *refusal.Detail
}

func TestRewindRefusesARowCompactionArchived(t *testing.T) {
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	shown, epoch := compactedSession(t, db)
	live := []any{"[CONTEXT SUMMARY] Q1..A2", "ok, continuing", "Q3", "A3"}
	if got := contentsOf(t, db, "s", GetMessagesOptions{}); !pyjson.Equal(got, live) {
		t.Fatalf("contents %v", got)
	}
	rewindRefusalDetail(t, db, epoch, shown["Q2"], "rewind")
	if got := contentsOf(t, db, "s", GetMessagesOptions{}); !pyjson.Equal(got, live) || !pyjson.Equal(rowValue(sessionOf(t, db, "s"), "runtime_generation"), 0) {
		t.Fatal("refused rewind changed state")
	}
	receipt := mustMutate(t, db, rewindTo(db, epoch, shown["Q3"], "live"))
	if !pyjson.Equal(rowValue(receipt, "rewound_count"), 2) {
		t.Fatalf("receipt %v", receipt)
	}
	if got := contentsOf(t, db, "s", GetMessagesOptions{}); !pyjson.Equal(got, live[:2]) {
		t.Fatalf("contents %v", got)
	}
}

func TestRewindRefusesATargetThatIsNotALiveUserRow(t *testing.T) {
	for _, target := range []string{"missing", "assistant"} {
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		shown, epoch := compactedSession(t, db)
		var id any = int64(99999)
		if target == "assistant" {
			id = shown["A3"]
		}
		if r := mutateRefusal(t, db, rewindTo(db, epoch, id, "rewind")); r != "invalid_params" {
			t.Fatal(r)
		}
		got := contentsOf(t, db, "s", GetMessagesOptions{})
		if !pyjson.Equal(got[len(got)-2:], []any{"Q3", "A3"}) {
			t.Fatalf("contents %v", got)
		}
		db.Close()
	}
}

func TestRewindRefusesARowACompressionArchived(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "old", "assistant", "reply"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(ctx, "s", GetMessagesOptions{})
	archived := rowValue(rows[0], "id")
	if _, err := db.ArchiveAndCompact(ctx, "s", textMessages("user", "[CONTEXT COMPACTION] earlier turns"), ArchiveCompactOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "live", "assistant", "answer"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	live := contentsOf(t, db, "s", GetMessagesOptions{})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	if r := mutateRefusal(t, db, rewindTo(db, epoch, archived, "rewind")); r != "invalid_params" {
		t.Fatal(r)
	}
	if got := contentsOf(t, db, "s", GetMessagesOptions{}); !pyjson.Equal(got, live) {
		t.Fatalf("contents %v", got)
	}
}

func TestOnlyACompactedTargetCarriesTheCompactionMarker(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	shown, epoch := compactedSession(t, db)
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "Q4", "assistant", "A4"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(ctx, "s", GetMessagesOptions{})
	q4, _ := exactInt(rowValue(rows[len(rows)-2], "id"))
	if _, err := db.RewindToMessage(ctx, "s", q4, RewindOptions{}); err != nil {
		t.Fatal(err)
	}
	compacted := rewindRefusalDetail(t, db, epoch, shown["Q2"], "compacted")
	if !strings.HasPrefix(compacted, "compacted:") {
		t.Fatalf("detail %q", compacted)
	}
	for name, target := range map[string]any{"assistant": shown["A3"], "undone": q4, "archived-assistant": shown["A1"], "missing": int64(99999)} {
		detail := rewindRefusalDetail(t, db, epoch, target, name)
		if detail == compacted || strings.HasPrefix(detail, "compacted:") {
			t.Fatalf("%s carried the compaction marker: %q", name, detail)
		}
	}
}

func TestATurnARotatingCompressionLeftOnTheParentCarriesTheMarker(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "p", Source: "test"})
	if _, err := db.AppendMessagesBatch(ctx, "p", textMessages("user", "old", "assistant", "reply"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(ctx, "p", GetMessagesOptions{})
	old := rowValue(rows[0], "id")
	if err := db.EndSession("p", "compression"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, NewSession{ID: "s", Source: "test", ParentSessionID: "p"})
	if _, err := db.AppendMessagesBatch(ctx, "s", textMessages("user", "[CONTEXT SUMMARY] old", "user", "live", "assistant", "ok"), AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, NewSession{ID: "elsewhere", Source: "test"})
	foreign, err := db.AppendMessage(ctx, "elsewhere", map[string]any{"role": "user", "content": "not this chat"}, AppendGuards{})
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	if !strings.HasPrefix(rewindRefusalDetail(t, db, epoch, old, "parent"), "compacted:") {
		t.Fatal("parent turn without the marker")
	}
	if strings.HasPrefix(rewindRefusalDetail(t, db, epoch, foreign, "foreign"), "compacted:") {
		t.Fatal("foreign turn carried the marker")
	}
}

// Legacy prompt admissions row as the pre runtime store wrote it
func legacyRow(seq int64, target, status string) []any {
	lineage, _ := pyjson.Dumps([]any{"s", target})
	payload, _ := pyjson.Dumps(pyjson.ObjectOf("text", "input-"+pyStr(seq), "intent", "queue"))
	var outcome, generation any
	if status == "terminal" {
		outcome = "completed"
	}
	if status == "started" {
		generation = seq
	}
	return []any{seq, "old-" + pyStr(seq), target, "s", lineage, payload, status, outcome, "dead", generation}
}

// Writes a frozen legacy admissions database at path with the rows
func legacyAdmissionsDB(t *testing.T, path string, rows [][]any) {
	t.Helper()
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE admissions(seq INTEGER PRIMARY KEY, admission_id TEXT,
            target_session_id TEXT, root TEXT, lineage TEXT, payload TEXT, status TEXT,
            outcome TEXT, owner TEXT, generation INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := conn.ExecContext(ctx, "INSERT INTO admissions VALUES(?,?,?,?,?,?,?,?,?,?)", row...); err != nil {
			t.Fatal(err)
		}
	}
}

// Runs one statement on a raw connection to the file
func rawExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func admissionIDs(rows []*pyjson.Object) []any {
	out := []any{}
	for _, row := range rows {
		out = append(out, rowValue(row, "admission_id"))
	}
	return out
}

func admissionStatuses(rows []*pyjson.Object) []any {
	out := []any{}
	for _, row := range rows {
		out = append(out, rowValue(row, "status"))
	}
	return out
}

func TestFrozenLegacyImportPreservesWorkWithoutBranchTheft(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := mustSessionDB(t, dir+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	if err := db.EndSession("s", "compression"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, NewSession{ID: "tip", Source: "test", ParentSessionID: "s"})
	mustCreate(t, db, NewSession{ID: "fork", Source: "test", ParentSessionID: "s", ModelConfig: map[string]any{"_branched_from": "s"}})
	source := dir + "/prompt-admissions.db"
	unbound := legacyRow(2, "s", "started")
	unbound[len(unbound)-1] = nil
	legacyAdmissionsDB(t, source, [][]any{legacyRow(1, "s", "queued"), unbound, legacyRow(3, "fork", "queued"), legacyRow(4, "s", "terminal")})
	rawExec(t, source, "CREATE TABLE executions(root TEXT PRIMARY KEY, generation INTEGER NOT NULL)")
	rawExec(t, source, "INSERT INTO executions VALUES('s', 40)")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "boot")
	imported, err := db.ImportLegacySessionAdmissions(ctx, epoch, source, "legacy-human", true)
	if err != nil || imported != 4 {
		t.Fatalf("import %d %v", imported, err)
	}
	if again, err := db.ImportLegacySessionAdmissions(ctx, epoch, source, "legacy-human", true); err != nil || again != 0 {
		t.Fatalf("replay %d %v", again, err)
	}
	after, _ := os.ReadFile(source)
	if string(after) != string(before) {
		t.Fatal("the frozen source changed")
	}
	tip, err := db.ListSessionAdmissions(ctx, "tip", false)
	if err != nil || !pyjson.Equal(admissionIDs(tip), []any{"old-1", "old-2", "old-4"}) || !pyjson.Equal(admissionStatuses(tip), []any{"queued", "unknown", "terminal"}) {
		t.Fatalf("tip admissions %v %v", tip, err)
	}
	fork, _ := db.ListSessionAdmissions(ctx, "fork", true)
	if !pyjson.Equal(admissionIDs(fork), []any{"old-3"}) {
		t.Fatalf("fork admissions %v", fork)
	}
	if _, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "tip"}); refusalReason(t, err) != "unknown_execution" {
		t.Fatal(err)
	}
	if _, err := db.ResolveUnknownSessionInput(ctx, epoch, "old-2", 99, nil, nil); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
	unknownGeneration, ok := exactInt(rowValue(tip[1], "generation"))
	if !ok || unknownGeneration <= 40 {
		t.Fatalf("unknown generation %v", rowValue(tip[1], "generation"))
	}
	if _, err := db.ResolveUnknownSessionInput(ctx, epoch, "old-2", unknownGeneration, nil, nil); err != nil {
		t.Fatal(err)
	}
	claimed := claimHead(t, db, epoch, "tip")
	if generation, _ := exactInt(rowValue(claimed, "generation")); objStr(claimed, "admission_id") != "old-1" || generation <= unknownGeneration {
		t.Fatalf("claimed %v", claimed)
	}
	rawExec(t, source, "UPDATE admissions SET payload='{}' WHERE seq=1")
	if _, err := db.ImportLegacySessionAdmissions(ctx, epoch, source, "legacy-human", true); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
}

func TestLegacyImportFailureRollsBackRowsAndMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := mustSessionDB(t, dir+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "test"})
	source := dir + "/prompt-admissions.db"
	legacyAdmissionsDB(t, source, [][]any{legacyRow(1, "s", "queued"), legacyRow(2, "missing", "queued")})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "boot")
	if _, err := db.ImportLegacySessionAdmissions(ctx, epoch, source, "human", false); refusalReason(t, err) != "invalid_params" {
		t.Fatal(err)
	}
	if _, err := db.ImportLegacySessionAdmissions(ctx, epoch, source, "human", true); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
	if rows, _ := db.ListSessionAdmissions(ctx, "s", true); len(rows) != 0 {
		t.Fatalf("rolled back rows survived %v", rows)
	}
	if scalarOf(t, db.conn, "SELECT value FROM state_meta WHERE key='gateway.prompt_admissions_import.v1'") != nil {
		t.Fatal("marker written")
	}
	mustCreate(t, db, NewSession{ID: "missing", Source: "test"})
	if imported, err := db.ImportLegacySessionAdmissions(ctx, epoch, source, "human", true); err != nil || imported != 2 {
		t.Fatalf("import %d %v", imported, err)
	}
}

func TestTargetAdvanceAllowsQueuedFollowerAndRefusesStartedHead(t *testing.T) {
	installRetirementHooks(t)
	for _, operation := range []string{"reset", "compress"} {
		ctx := context.Background()
		dir := t.TempDir()
		db := mustSessionDB(t, dir+"/state.db", false)
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		sid := localSessionFixture(t, db, epoch, dir)
		admitInput(t, db, epoch, "human", sid, "head", admissionText("head"))
		admitInput(t, db, epoch, "human", sid, "follower", admissionText("follower"))
		mutate := func() (*pyjson.Object, error) {
			row := sessionOf(t, db, sid)
			in := mutationAt(db, sid, operation, operation+"-"+pyStr(rowValue(row, "runtime_revision")), pyjson.NewObject())
			in.Epoch = epoch
			in.PrepareOnly = operation == "compress"
			return db.MutateRuntimeSession(ctx, in)
		}
		receipt, err := mutate()
		if err != nil {
			t.Fatal(err)
		}
		if operation == "compress" {
			if _, has := receipt.Get("snapshot"); !has {
				t.Fatalf("receipt %v", receipt)
			}
		} else if pyjson.Equal(rowValue(receipt, "target_session_id"), sid) {
			t.Fatalf("receipt %v", receipt)
		}
		rows, _ := db.ListSessionAdmissions(ctx, sid, true)
		if !pyjson.Equal(admissionStatuses(rows), []any{"queued", "queued"}) {
			t.Fatalf("statuses %v", rows)
		}
		head := claimHead(t, db, epoch, sid)
		before := sessionOf(t, db, sid)
		if _, err := mutate(); refusalReason(t, err) != "session_busy" {
			t.Fatal(err)
		}
		after := sessionOf(t, db, sid)
		if !pyjson.Equal(rowValue(after, "runtime_generation"), rowValue(before, "runtime_generation")) || !pyjson.Equal(rowValue(after, "runtime_revision"), rowValue(before, "runtime_revision")) {
			t.Fatal("refused mutation moved the row")
		}
		generation, _ := exactInt(rowValue(head, "generation"))
		if _, err := db.SettleSessionInput(ctx, SettleInput{Epoch: epoch, AdmissionID: objStr(head, "admission_id"), Generation: generation, Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		rows, _ = db.ListSessionAdmissions(ctx, sid, true)
		requests := []any{}
		for _, row := range rows {
			requests = append(requests, rowValue(row, "request_id"))
		}
		if !pyjson.Equal(requests, []any{"follower"}) {
			t.Fatalf("pending %v", requests)
		}
		db.Close()
	}
}

func TestWorkerReadsKeepRevisionAndWritesStillAdvanceIt(t *testing.T) {
	db, store := setupStore(t)
	defer db.Close()
	if _, err := store.appendMessagesBatch("owned", textMessages("user", "hello")); err != nil {
		t.Fatal(err)
	}
	seen, _ := exactInt(rowValue(sessionOf(t, db, "owned"), "runtime_revision"))
	if _, err := store.getSession("owned"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sessionLifecycleStatuses([]string{"owned"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.getMessagesAsConversation("owned", ConversationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.lineageField("owned", "tip"); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "owned"), "runtime_revision"), seen) {
		t.Fatal("reads moved the revision")
	}
	result := mustMutate(t, db, RuntimeMutation{Epoch: store.scope.Epoch, PrincipalID: "client", SessionID: "owned", RequestID: "rename", ExpectedRevision: seen,
		Operation: "rename", Payload: pyjson.ObjectOf("title", "renamed")})
	if !pyjson.Equal(rowValue(result, "revision"), seen+1) {
		t.Fatalf("revision %v", result)
	}
	if err := store.touchSessionActivity("owned", nil, "working", nil); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "owned"), "runtime_revision"), seen+2) {
		t.Fatal("activity write")
	}
	if _, err := store.finish(); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "owned"), "runtime_revision"), seen+3) {
		t.Fatal("finish write")
	}
}

func TestPhysicalWorkerExcludesLogicalFifoAndSecondRegistration(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	logical := localResetFixture(t, db, epoch, "physical")
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "standalone-worker", SessionID: "physical", Generation: 0, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	admitInput(t, db, epoch, "human", logical, "later", admissionText("later"))
	if claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: logical}); err != nil || claim != nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	generation, _ := exactInt(rowValue(sessionOf(t, db, logical), "runtime_generation"))
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "second-worker", SessionID: logical, Generation: generation, Kind: "compute", AdoptionSecret: "secret"}); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
}

func TestResetRevokesOnlyOrphanUnknownWorkerAndUnblocksFifo(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	logical := localResetFixture(t, db, epoch, "physical")
	generation, _ := exactInt(rowValue(sessionOf(t, db, logical), "runtime_generation"))
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "standalone-worker", SessionID: logical, Generation: generation, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ = db.BeginRuntimeEpoch(ctx, "restarted")
	if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	admitInput(t, db, epoch, "human", logical, "later", admissionText("later"))
	if _, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: logical}); refusalReason(t, err) != "unknown_execution" {
		t.Fatal(err)
	}
	reset := mutationAt(db, logical, "reset", "explicit-reset", pyjson.NewObject())
	reset.Epoch = epoch
	receipt := mustMutate(t, db, reset)
	if pyjson.Equal(rowValue(receipt, "target_session_id"), "physical") {
		t.Fatalf("reset reused the physical target %v", receipt)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT status FROM worker_executions"), "terminal") {
		t.Fatal("orphan worker not revoked")
	}
	if claimed := claimHead(t, db, epoch, logical); objStr(claimed, "request_id") != "later" {
		t.Fatalf("claimed %v", claimed)
	}
	if _, err := db.AdoptWorkerExecution(ctx, epoch, "standalone-worker", logical, generation, "secret"); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
}
