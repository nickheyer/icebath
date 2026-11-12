package gateway

import (
	"context"
	"testing"
)

func TestAPolicyStoredBeforeTheFieldRestoresAsAskWithItsIdentityUnchanged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	policy, err := buildPolicy(context.Background(), map[string]any{"source": "acp", "cwd": t.TempDir(), "model": "fixture"}, map[string]any{}, buildPolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored := policy.asDict()
	delete(stored, "edit_approval_policy")
	old, err := restorePolicy(stored)
	if err != nil {
		t.Fatal(err)
	}
	if old.EditApprovalPolicy != "ask" {
		t.Fatalf("edit approval policy %q", old.EditApprovalPolicy)
	}
	if policyIdentity(old, nil) != policyIdentity(policy, nil) {
		t.Fatal("identity changed by a missing edit approval policy")
	}
	session := *policy
	session.EditApprovalPolicy = "session"
	if policyIdentity(&session, nil) == policyIdentity(policy, nil) {
		t.Fatal("edit approval policy left out of the identity")
	}
	if _, err := restorePolicy(storedWith(policy, map[string]any{"edit_approval_policy": "everything"})); err == nil {
		t.Fatal("unknown edit approval policy restored")
	}
}
