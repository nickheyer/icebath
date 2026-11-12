package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Policy the reviewer profile advertises for a config with these approvals and turns
func roomTestPolicy(t *testing.T, approvalMode string, maxTurns int) *pyjson.Object {
	t.Helper()
	policy, err := ExecutionPolicyMapping(context.Background(), "reviewer", map[string]any{
		"agent":             map[string]any{"max_turns": maxTurns},
		"approvals":         map[string]any{"mode": approvalMode},
		"platform_toolsets": map[string]any{"api_server": []any{"icebath-api-server", "web"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// Recipient dispatch for the peer catalog advertising policy
func roomTestDispatch(t *testing.T, policy *pyjson.Object) *HostedMemberDispatch {
	t.Helper()
	mapping, err := CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer",
		PersistentProcess: true, TargetProfile: "reviewer", ExecutionPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := GatewayRoomCatalogFromMapping(mapping)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Review the user's patch."
	sum := sha256.Sum256([]byte(prompt))
	digest, _ := policy.Get("policy_digest")
	dispatch, err := HostedMemberDispatchFromMapping(map[string]any{
		"protocol_version": int64(2), "room_id": "room-1", "home_install_id": "install-home",
		"authority_gateway_id": "gateway-home", "authority_epoch": int64(1), "member_id": "member-reviewer",
		"target_install_id": "install-peer", "target_profile": "reviewer", "task_id": "task-1",
		"execution_generation": int64(1), "source_event_seq": int64(1), "cancellation_scope_id": "cancel-1",
		"prompt": prompt, "prompt_digest": hex.EncodeToString(sum[:]), "capability_digest": catalog.CatalogDigest,
		"execution_policy_digest": digest, "trace_id": "trace-1"})
	if err != nil {
		t.Fatal(err)
	}
	return dispatch
}

func TestExecutionPolicyDigestCoversToolsApprovalsAndIterationLimit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	value := roomTestPolicy(t, "manual", 12)
	checked, err := RoomExecutionPolicyFromMapping(value)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range checked.EnabledToolsets {
		found = found || name == "bot_room"
	}
	if !found {
		t.Fatalf("toolsets %v", checked.EnabledToolsets)
	}
	for _, change := range []struct {
		field       string
		replacement any
	}{{"enabled_toolsets", []any{"bot_room"}}, {"approval_mode", "off"}, {"max_iterations", int64(99)}} {
		changed := value.Copy()
		changed.Set(change.field, change.replacement)
		_, err := RoomExecutionPolicyFromMapping(changed)
		if _, isValue := err.(*RoomExecutionPolicyError); !isValue || !strings.Contains(err.Error(), "policy_digest") {
			t.Fatalf("%s: %v", change.field, err)
		}
	}
}

func TestUnlimitedPolicySurvivesTheCatalogJsonRoundTripExactly(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	policy := roomTestPolicy(t, "manual", 0)
	catalog, err := CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer",
		PersistentProcess: true, TargetProfile: "reviewer", ExecutionPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := pyjson.Loads(string(text))
	if err != nil {
		t.Fatal(err)
	}
	checked, err := GatewayRoomCatalogFromMapping(wire)
	if err != nil {
		t.Fatal(err)
	}
	maxIterations, _ := policy.Get("max_iterations")
	if !pyjson.Equal(maxIterations, int64(MaxPolicyIterations)) || float64(maxIterations.(int64)) != float64(MaxPolicyIterations) {
		t.Fatalf("max_iterations %v", maxIterations)
	}
	if checked.ExecutionPolicy.MaxIterations != MaxPolicyIterations || !pyjson.Equal(checked.ExecutionPolicy.AsMapping(), policy) {
		t.Fatalf("checked %+v", checked.ExecutionPolicy)
	}
}

func TestRoomPolicyOverridesBroaderLiveApprovalConfig(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	policy, err := RoomExecutionPolicyFromMapping(roomTestPolicy(t, "manual", 12))
	if err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: t.TempDir(), Config: config.Tree{"approvals": map[string]any{"mode": "off"}}, Values: &sync.Map{}}
	reset := BindRoomExecutionPolicyEnv(env, policy)
	if mode := tools.GetApprovalMode(env); mode != "manual" {
		reset()
		t.Fatalf("mode %q", mode)
	}
	reset()
	if mode := tools.GetApprovalMode(env); mode != "off" {
		t.Fatalf("reset mode %q", mode)
	}
}

func TestRoomCatalogFailsClosedWhenRemoteApprovalsAreOff(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, err := CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer",
		PersistentProcess: true, TargetProfile: "reviewer", ExecutionPolicy: roomTestPolicy(t, "off", 12)})
	if _, isPeer := err.(*HostedRoomPeerError); !isPeer || !strings.Contains(err.Error(), "requires manual or smart approvals") {
		t.Fatalf("err %v", err)
	}
}

// Alpha as the launch home and beta as a served sibling, each with its own turn limit
func twoProfileHomes(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	for name, turns := range map[string]string{"alpha": "12", "beta": "7"} {
		dir := filepath.Join(root, "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "agent:\n  max_turns: " + turns + "\napprovals:\n  mode: manual\nplatform_toolsets:\n  api_server: [icebath-api-server, web]\n"
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "alpha"))
	t.Setenv("ICEBATH_PROFILE", "alpha")
	os.Unsetenv("ICEBATH_YOLO_MODE")
}

func TestCatalogPolicyComesFromTheServedProfileNotTheLaunchEnv(t *testing.T) {
	twoProfileHomes(t)
	turns := func(profile string) any {
		catalog, err := CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer",
			PersistentProcess: true, TargetProfile: profile})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := catalog.Get("execution_policy")
		policy := raw.(*pyjson.Object)
		if target, _ := policy.Get("target_profile"); target != profile {
			t.Fatalf("target %v", target)
		}
		limit, _ := policy.Get("max_iterations")
		return limit
	}
	for _, step := range []struct {
		profile string
		want    int64
	}{{"alpha", 12}, {"beta", 7}, {"alpha", 12}} {
		if got := turns(step.profile); !pyjson.Equal(got, step.want) {
			t.Fatalf("%s turns %v", step.profile, got)
		}
	}
	_, err := ExecutionPolicyMapping(context.Background(), "ghost", nil)
	if _, isPolicy := err.(*RoomExecutionPolicyError); !isPolicy || !strings.Contains(err.Error(), "'ghost' is not a live profile") {
		t.Fatalf("ghost %v", err)
	}
}

func TestCatalogRequiresTheServedProfileAndNeverReadsHermesProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_PROFILE", "reviewer")
	if _, err := CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer", PersistentProcess: true}); err == nil || !strings.Contains(err.Error(), "target_profile") {
		t.Fatalf("missing %v", err)
	}
	_, err := CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer", PersistentProcess: true, TargetProfile: ""})
	if _, isPeer := err.(*HostedRoomPeerError); !isPeer || !strings.Contains(err.Error(), "target_profile") {
		t.Fatalf("empty %v", err)
	}
	_, err = CatalogMapping(context.Background(), CatalogOptions{InstallationID: "install-peer", PersistentProcess: true,
		TargetProfile: "other", ExecutionPolicy: roomTestPolicy(t, "manual", 12)})
	if _, isPeer := err.(*HostedRoomPeerError); !isPeer || !strings.Contains(err.Error(), "target_profile does not match") {
		t.Fatalf("other %v", err)
	}
}
