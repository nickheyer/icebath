package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	claimCardDisplay   = "Background Process Finished: sleep 6; echo BGDONE"
	claimCardModelText = "[IMPORTANT: Background process proc_1 completed (exit 0).\nOutput:\nBGDONE]"
)

// Payload of one admitted local automation turn that carries the process completion card
func claimCardAutomation() *jsonutil.Object {
	return jsonutil.ObjectOf("text", claimCardModelText, "local_automation_v1", jsonutil.ObjectOf(
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
	var published []*jsonutil.Object
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *jsonutil.Object) {
		params, _ := frame.Get("params")
		if kind, _ := params.(*jsonutil.Object).Get("type"); kind == "message.start" {
			payload, _ := params.(*jsonutil.Object).Get("payload")
			published = append(published, payload.(*jsonutil.Object))
		}
	}}] = true
	if err := a.StampClaim(ctx, testRef, live, jsonutil.ObjectOf("generation", 9, "admission_id", "adm-9", "payload", claimCardAutomation())); err != nil {
		t.Fatal(err)
	}
	if err := a.StampClaim(ctx, testRef, live, jsonutil.ObjectOf("generation", 10, "admission_id", "adm-10",
		"payload", jsonutil.ObjectOf("text", "typed"))); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published %v", published)
	}
	want := jsonutil.ObjectOf("display_kind", "process_complete", "display_text", claimCardDisplay, "text", claimCardModelText)
	if got := dumps(t, published[0]); got != dumps(t, want) {
		t.Fatalf("card %s", got)
	}
	if published[1].Len() != 0 {
		t.Fatalf("ordinary claim published %s", dumps(t, published[1]))
	}
}

func TestClaimStampCarriesTheAutomationCardOnMessageStart(t *testing.T) {
	want := jsonutil.ObjectOf("display_kind", "process_complete", "display_text", claimCardDisplay, "text", claimCardModelText)
	if got := dumps(t, AutomationAdmissionDisplay(claimCardAutomation())); got != dumps(t, want) {
		t.Fatalf("local card %s", got)
	}
	diagnostic := jsonutil.ObjectOf("text", "x", "native_text_v1", jsonutil.ObjectOf("automation", jsonutil.ObjectOf(
		"display_kind", "process_complete", "display_text", claimCardDisplay, "notification_category", "diagnostic")))
	if category, _ := AutomationAdmissionDisplay(diagnostic).Get("notification_category"); category != "diagnostic" {
		t.Fatalf("diagnostic card %s", dumps(t, AutomationAdmissionDisplay(diagnostic)))
	}
	if card := AutomationAdmissionDisplay(jsonutil.ObjectOf("text", "typed by the user")); card.Len() != 0 {
		t.Fatalf("typed input card %s", dumps(t, card))
	}
	if card := AutomationAdmissionDisplay(jsonutil.ObjectOf("text", "x", "local_automation_v1", jsonutil.ObjectOf("identity", "i"))); card.Len() != 0 {
		t.Fatalf("bare descriptor card %s", dumps(t, card))
	}
}
