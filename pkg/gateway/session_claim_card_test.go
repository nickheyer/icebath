package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	claimCardDisplay   = "Background Process Finished: sleep 6; echo BGDONE"
	claimCardModelText = "[IMPORTANT: Background process proc_1 completed (exit 0).\nOutput:\nBGDONE]"
)

// Payload of one admitted local automation turn that carries the process completion card
func claimCardAutomation() *pyjson.Object {
	return pyjson.ObjectOf("text", claimCardModelText, "local_automation_v1", pyjson.ObjectOf(
		"identity", "i", "owner", "s", "route", "r", "target", "s",
		"display_kind", "process_complete", "display_text", claimCardDisplay))
}

func TestStampClaimPublishesTheCardAndAnOrdinaryClaimStaysEmpty(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "owned", "owner", db, epoch)
	live := NewLiveSession(nil, "route")
	a.Sessions["s"] = live
	var published []*pyjson.Object
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) {
		params, _ := frame.Get("params")
		if kind, _ := params.(*pyjson.Object).Get("type"); kind == "message.start" {
			payload, _ := params.(*pyjson.Object).Get("payload")
			published = append(published, payload.(*pyjson.Object))
		}
	}}] = true
	if err := a.stampClaim(ctx, testRef, live, pyjson.ObjectOf("generation", 9, "admission_id", "adm-9", "payload", claimCardAutomation())); err != nil {
		t.Fatal(err)
	}
	if err := a.stampClaim(ctx, testRef, live, pyjson.ObjectOf("generation", 10, "admission_id", "adm-10",
		"payload", pyjson.ObjectOf("text", "typed"))); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published %v", published)
	}
	want := pyjson.ObjectOf("display_kind", "process_complete", "display_text", claimCardDisplay, "text", claimCardModelText)
	if got := pyDumps(t, published[0]); got != pyDumps(t, want) {
		t.Fatalf("card %s", got)
	}
	if published[1].Len() != 0 {
		t.Fatalf("ordinary claim published %s", pyDumps(t, published[1]))
	}
}

func TestClaimStampCarriesTheAutomationCardOnMessageStart(t *testing.T) {
	want := pyjson.ObjectOf("display_kind", "process_complete", "display_text", claimCardDisplay, "text", claimCardModelText)
	if got := pyDumps(t, AutomationAdmissionDisplay(claimCardAutomation())); got != pyDumps(t, want) {
		t.Fatalf("local card %s", got)
	}
	diagnostic := pyjson.ObjectOf("text", "x", "native_text_v1", pyjson.ObjectOf("automation", pyjson.ObjectOf(
		"display_kind", "process_complete", "display_text", claimCardDisplay, "notification_category", "diagnostic")))
	if category, _ := AutomationAdmissionDisplay(diagnostic).Get("notification_category"); category != "diagnostic" {
		t.Fatalf("diagnostic card %s", pyDumps(t, AutomationAdmissionDisplay(diagnostic)))
	}
	if card := AutomationAdmissionDisplay(pyjson.ObjectOf("text", "typed by the user")); card.Len() != 0 {
		t.Fatalf("typed input card %s", pyDumps(t, card))
	}
	if card := AutomationAdmissionDisplay(pyjson.ObjectOf("text", "x", "local_automation_v1", pyjson.ObjectOf("identity", "i"))); card.Len() != 0 {
		t.Fatalf("bare descriptor card %s", pyDumps(t, card))
	}
}
