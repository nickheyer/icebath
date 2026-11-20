package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// End reason of the test session, empty while it is still open
func webhookSessionEndReason(t *testing.T, a *SessionAuthority) string {
	t.Helper()
	rows, err := a.DB.ReadAll(t.Context(), "SELECT COALESCE(end_reason, '') AS reason FROM sessions WHERE id=? AND ended_at IS NOT NULL", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return ""
	}
	return rowString(rows[0], "reason")
}

func TestWebhookFinalizeDeliveryClosesOnlyTheSettledAdmittedSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	holdDrains(t)
	ctx, db, a := newTestAuthority(t, PlatformWebhook)
	adapter, err := NewWebhookAdapter(&PlatformConfig{Extra: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := submitText(ctx, a, "delivery-1")
	if err != nil {
		t.Fatal(err)
	}

	unbound := NewMessageEvent("no admitted delivery")
	if err := adapter.endWebhookSession(ctx, unbound); err != nil || webhookSessionEndReason(t, a) != "" {
		t.Fatalf("an event without a receipt closed the session, err %v reason %q", err, webhookSessionEndReason(t, a))
	}

	settle := func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=1 WHERE id='s'"); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='terminal', generation=1 WHERE admission_id=?", receipt.AdmissionID)
		return err
	}
	if err := db.ExecuteWrite(ctx, settle); err != nil {
		t.Fatal(err)
	}
	event := NewMessageEvent("delivery-1")
	adapter.finalizeDelivery(ctx, event, a, receipt)
	if event.webhookReceipt == nil || event.webhookReceipt.authority != a || event.webhookReceipt.receipt.Status != "terminal" ||
		event.webhookReceipt.receipt.AdmissionID != receipt.AdmissionID {
		t.Fatalf("event carries %#v", event.webhookReceipt)
	}
	if reason := webhookSessionEndReason(t, a); reason != "webhook_complete" {
		t.Fatalf("settled delivery left the session with end reason %q", reason)
	}
}
