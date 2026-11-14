package gateway

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDeliveryID = "0123456789abcdef0123456789abcdef"

// Writes one ticket into a fresh profile mailbox and answers the profile home
func seedTicket(t *testing.T, record map[string]any) string {
	t.Helper()
	h := deliveryResolveHome(t.TempDir())
	root := deliveryRoot(h)
	if _, err := prepareMailbox(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if record != nil {
		if err := writeTicket(context.Background(), filepath.Join(root, record["id"].(string)+".json"), record); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func ticket(h, status string, extra map[string]any) map[string]any {
	if h == "" {
		h = "/profile"
	}
	r := map[string]any{"id": testDeliveryID, "delivery_id": testDeliveryID, "status": status, "created_at": int64(5),
		"owner": map[string]any{"profile_home": h, "session_id": "s", "lease_id": "l", "live_session_id": "s"}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func TestDeliveryIDAndOwnerPinRefusals(t *testing.T) {
	if _, err := checkDeliveryID("ABC"); err == nil || err.Error() != "delivery id must be 32 to 64 lowercase hex characters" {
		t.Fatal(err)
	}
	h := deliveryResolveHome(t.TempDir())
	if _, err := liveOwnerPin(h, map[string]any{"profile_home": h, "session_id": "", "lease_id": "", "live_session_id": ""}); err == nil {
		t.Fatal("an empty lease must be refused")
	}
	if _, err := liveOwnerPin(h, map[string]any{"profile_home": "/elsewhere", "session_id": "", "lease_id": "l", "live_session_id": ""}); err == nil || err.Error() != "owner belongs to a different profile home" {
		t.Fatal(err)
	}
	pin, err := liveOwnerPin(h, map[string]any{"profile_home": h, "session_id": "", "lease_id": "l", "live_session_id": ""})
	if err != nil || pin["lease_id"] != "l" {
		t.Fatal(pin, err)
	}
}

func TestDoorBuildsTheEnvelopeAndEchoesTheDiagnosticCategory(t *testing.T) {
	root := deliveryResolveHome(t.TempDir())
	h := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	call := func(_ context.Context, home string, params map[string]any) (map[string]any, error) {
		got = params
		return map[string]any{"admitted": true, "home": home}, nil
	}
	owner := map[string]any{"profile_home": h, "session_id": "", "lease_id": "l", "live_session_id": ""}
	out, err := deliveryDoor(context.Background(), call, h, owner, "hi", "", map[string]any{"name": "cron"}, "diagnostic")
	if err != nil || out["notification_category"] != "diagnostic" || out["admitted"] != true || out["home"] != h {
		t.Fatal(out, err)
	}
	if got["profile"] != "work" || got["message"] != "hi" || got["notification_category"] != "diagnostic" || len(got["id"].(string)) != 32 {
		t.Fatal(got)
	}
	if _, has := got["session_id"]; has || got["author"].(map[string]any)["name"] != "cron" {
		t.Fatal(got)
	}
	owner["session_id"] = "tip"
	out, err = deliveryDoor(context.Background(), call, h, owner, "hi", testDeliveryID, nil, "")
	if err != nil || got["session_id"] != "tip" || got["id"] != testDeliveryID || out["notification_category"] != nil {
		t.Fatal(out, got, err)
	}
	if _, err := deliveryDoor(context.Background(), call, h, owner, "hi", "", nil, "loud"); err == nil || err.Error() != "invalid notification category" {
		t.Fatal(err)
	}
}

func TestAuthorityCallsBeforeTheDialArePreDialErrors(t *testing.T) {
	h := deliveryResolveHome(t.TempDir())
	_, err := AuthorityDelivery(context.Background(), h, map[string]any{"id": testDeliveryID})
	var pre *PreDialError
	if !errors.As(err, &pre) || pre.Msg != "profile authority is not ready" {
		t.Fatal(err)
	}
	if _, err := FindCanonicalLiveOwner(context.Background(), h); err == nil || err.Error() != "profile authority is not ready" {
		t.Fatal(err)
	}
}

func TestCompleteDeliveryIsImmutableOnceTerminal(t *testing.T) {
	h := seedTicket(t, nil)
	if _, err := CompleteDelivery(context.Background(), h, testDeliveryID, "settled", "r", "", ""); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := CompleteDelivery(context.Background(), h, testDeliveryID, "done", "", "", ""); err == nil || err.Error() != "invalid terminal delivery status" {
		t.Fatal(err)
	}
	h = seedTicket(t, ticket("", "queued", nil))
	if _, err := CompleteDelivery(context.Background(), h, testDeliveryID, "settled", "r", "", ""); err == nil || err.Error() != "delivery must be claimed before completion" {
		t.Fatal(err)
	}
	h = seedTicket(t, ticket("", "claimed", nil))
	rec, err := CompleteDelivery(context.Background(), h, testDeliveryID, "settled", "reply", "", "")
	if err != nil || rec["status"] != "settled" || rec["completed_at"] == nil {
		t.Fatal(rec, err)
	}
	if again, err := CompleteDelivery(context.Background(), h, testDeliveryID, "settled", "reply", "", ""); err != nil || again["reply"] != "reply" {
		t.Fatal(again, err)
	}
	if _, err := CompleteDelivery(context.Background(), h, testDeliveryID, "failed", "", "x", ""); err == nil || err.Error() != "delivery already has a different terminal receipt" {
		t.Fatal(err)
	}
}

func TestCancelQueuedDeliveryLeavesAdmissionsAndClaims(t *testing.T) {
	h := seedTicket(t, ticket("", "queued", nil))
	rec, err := CancelQueuedDelivery(context.Background(), h, testDeliveryID, "no owner", "ownerless")
	if err != nil || rec["status"] != "cancelled" || rec["error"] != "no owner" || rec["reason"] != "ownerless" {
		t.Fatal(rec, err)
	}
	h = seedTicket(t, ticket("", "queued", map[string]any{"admission_id": "a1"}))
	if rec, err := CancelQueuedDelivery(context.Background(), h, testDeliveryID, "e", "r"); err != nil || rec["status"] != "queued" {
		t.Fatal(rec, err)
	}
	h = seedTicket(t, nil)
	if rec, err := CancelQueuedDelivery(context.Background(), h, testDeliveryID, "e", "r"); err != nil || rec != nil {
		t.Fatal(rec, err)
	}
}

func TestReadDeliveryResultMapsTheTransitionalCanonicalStatus(t *testing.T) {
	if rec, err := ReadDeliveryResult(context.Background(), t.TempDir(), testDeliveryID); err != nil || rec != nil {
		t.Fatal(rec, err)
	}
	h := seedTicket(t, ticket("", "canonical", map[string]any{"admission_id": "a1"}))
	rec, err := ReadDeliveryResult(context.Background(), h, testDeliveryID)
	if err != nil || rec["status"] != "queued" {
		t.Fatal(rec, err)
	}
	if err := os.WriteFile(filepath.Join(deliveryRoot(h), testDeliveryID+".json"), []byte("[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeliveryResult(context.Background(), h, testDeliveryID); err == nil || err.Error() != "ticket "+testDeliveryID+".json is not a JSON object (list)" {
		t.Fatal(err)
	}
}

func TestNextSequenceSurvivesAnUnreadableTicket(t *testing.T) {
	h := seedTicket(t, ticket("", "queued", map[string]any{"sequence": int64(9)}))
	root := deliveryRoot(h)
	if err := os.WriteFile(filepath.Join(root, strings.Repeat("e", 32)+".json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	seq, err := nextDeliverySequence(context.Background(), root)
	if err != nil || seq.Int64() != 10 {
		t.Fatal(seq, err)
	}
	if err := os.Remove(filepath.Join(root, testDeliveryID+".json")); err != nil {
		t.Fatal(err)
	}
	if seq, err := nextDeliverySequence(context.Background(), root); err != nil || seq.Int64() != 11 {
		t.Fatal(seq, err)
	}
	if problem := ticketShapeError(filepath.Join(root, "x.json"), ticket("", "queued", nil)); problem != "id does not match filename" {
		t.Fatal(problem)
	}
	bad := ticket("", "lost", nil)
	if problem := ticketShapeError(filepath.Join(root, testDeliveryID+".json"), bad); problem != "unknown status 'lost'" {
		t.Fatal(problem)
	}
	bad = ticket("", "queued", map[string]any{"created_at": true})
	if problem := ticketShapeError(filepath.Join(root, testDeliveryID+".json"), bad); problem != "created_at/sequence are not integers" {
		t.Fatal(problem)
	}
}

func TestAwaitDeliveryReturnsTheSettledReceiptOrThePendingOneAtTheDeadline(t *testing.T) {
	saved := deliveryPollInterval
	t.Cleanup(func() { deliveryPollInterval = saved })
	deliveryPollInterval = 10 * time.Millisecond
	h := seedTicket(t, ticket("", "queued", nil))
	short := 30 * time.Millisecond
	rec, err := AwaitDelivery(context.Background(), h, testDeliveryID, &short, nil)
	if err != nil || rec["status"] != "queued" {
		t.Fatal(rec, err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = writeTicket(context.Background(), filepath.Join(deliveryRoot(h), testDeliveryID+".json"), ticket("", "settled", map[string]any{"reply": "ok"}))
	}()
	rec, err = AwaitDelivery(context.Background(), h, testDeliveryID, nil, nil)
	if err != nil || rec["status"] != "settled" || rec["reply"] != "ok" {
		t.Fatal(rec, err)
	}
	stop := func() bool { return true }
	h = seedTicket(t, ticket("", "claimed", nil))
	if rec, err := AwaitDelivery(context.Background(), h, testDeliveryID, nil, stop); err != nil || rec["status"] != "claimed" {
		t.Fatal(rec, err)
	}
}

func TestUnusableMailboxIsAPermissionError(t *testing.T) {
	h := deliveryResolveHome(t.TempDir())
	runtimeDir := filepath.Join(h, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runtimeDir, 0o700) })
	_, err := CompleteDelivery(context.Background(), h, testDeliveryID, "settled", "", "", "")
	var unusable *MailboxUnusable
	if !errors.As(err, &unusable) || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "bot mailbox directory is not usable: [Errno 13] Permission denied") {
		t.Fatal(err)
	}
	if HasMailbox(h) {
		t.Fatal("no mailbox was created")
	}
}
