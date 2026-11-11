package selfupdate

import (
	"io"
	"testing"
)

func outcomeOf(t *testing.T, plan *UpdatePlan, evidence runtimeOutcomeEvidence) string {
	t.Helper()
	outcomes := matchRuntimeOutcomes(plan, evidence)
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d rows", len(outcomes))
	}
	outcome, _ := rowValue(outcomes[0], "outcome").(string)
	return outcome
}

func TestExternalRequiresVerifiedPidEvidence(t *testing.T) {
	plan := NewUpdatePlan()
	plan.Runtimes = []*RuntimeRecord{gatewayRuntime("work", 4242)}
	common := runtimeOutcomeEvidence{RestartedServices: []string{}, RelaunchedProfiles: []string{}, ExternallySupervisedProfiles: []string{},
		KilledPIDs: map[int]bool{}, FailedUnits: []string{}}
	if got := outcomeOf(t, plan, common); got != "unaccounted" {
		t.Fatalf("no evidence gave %q", got)
	}
	other := common
	other.ExternalGatewayPIDs = map[int]bool{9999: true}
	if got := outcomeOf(t, plan, other); got != "unaccounted" {
		t.Fatalf("evidence for another pid gave %q", got)
	}
	verified := common
	verified.ExternalGatewayPIDs = map[int]bool{4242: true}
	outcomes := matchRuntimeOutcomes(plan, verified)
	if rowValue(outcomes[0], "outcome") != "external" {
		t.Fatalf("verified evidence gave %v", rowValue(outcomes[0], "outcome"))
	}
	if reportUnaccountedRuntimes(io.Discard, outcomes) {
		t.Fatal("an external gateway was reported unaccounted")
	}
}

func TestExternalEvidenceNeverCoversAServeRuntime(t *testing.T) {
	pid := 4242
	plan := NewUpdatePlan()
	plan.Runtimes = []*RuntimeRecord{{Kind: "serve", Profile: "work", PID: &pid, Supervisor: "manual-serve"}}
	got := outcomeOf(t, plan, runtimeOutcomeEvidence{KilledPIDs: map[int]bool{}, ExternalGatewayPIDs: map[int]bool{4242: true}})
	if got != "unaccounted" {
		t.Fatalf("outcome = %q", got)
	}
}
