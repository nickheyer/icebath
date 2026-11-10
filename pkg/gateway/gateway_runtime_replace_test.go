package gateway

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

type pendingSpawn struct{}

func (pendingSpawn) Poll() (int, bool) { return 0, false }
func (pendingSpawn) Pid() int          { return 1 }

// Ready verdict of a current owner serving home
func readyVerdict(h string) ObservedEndpoint {
	return ObservedEndpoint{State: "ready", Endpoint: &GatewayEndpoint{ProfileID: h, InstanceID: "new", AuthorityEpoch: big.NewInt(1),
		RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Supervisor: "none", Capabilities: map[string]bool{"session-authority-v1": true}}}
}

// Resolved temp home with the update fence open
func replaceHome(t *testing.T) string {
	t.Helper()
	h, _ := pypath.Resolve(filepath.Join(t.TempDir(), ".icebath"))
	os.Mkdir(h, 0o700)
	swapSeam(t, &ensureUpdateFenced, func(context.Context, []string) (bool, error) { return false, nil })
	return h
}

// Discovery double answering the verdicts in order and repeating the last
func scriptVerdicts(t *testing.T, verdicts ...ObservedEndpoint) {
	t.Helper()
	swapSeam(t, &ensureDiscover, func(context.Context, string, time.Duration) (ObservedEndpoint, error) {
		next := verdicts[0]
		if len(verdicts) > 1 {
			verdicts = verdicts[1:]
		}
		return next, nil
	})
}

// Replacement double recording each home and answering the refusal
func recordReplacements(t *testing.T, refusal string) *[]string {
	t.Helper()
	calls := &[]string{}
	swapSeam(t, &replaceIncompatibleOwner, func(_ context.Context, h string, _ float64) string { *calls = append(*calls, h); return refusal })
	return calls
}

func TestEnsureReplacesAnIncompatibleOwnerOfThisHome(t *testing.T) {
	h := replaceHome(t)
	scriptVerdicts(t, ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol"}, ObservedEndpoint{State: "absent"}, readyVerdict(h))
	stopped := recordReplacements(t, "")
	swapSeam(t, &discoverExistingGatewayService, func(context.Context, string, float64) (*ExistingService, error) { return nil, nil })
	swapSeam(t, &spawnUnmanagedGateway, func(context.Context, string, float64, bool, bool) (SpawnedGateway, error) { return pendingSpawn{}, nil })
	result, err := EnsureGatewayRuntime(context.Background(), h, 5, false)
	if err != nil || result.State != "ready" || len(*stopped) != 1 || (*stopped)[0] != h {
		t.Fatalf("%+v %v %v", result, err, *stopped)
	}
}

func TestACurrentProtocolOwnerWithoutSessionAuthorityIsNotReplaced(t *testing.T) {
	h := replaceHome(t)
	scriptVerdicts(t, ObservedEndpoint{State: "incompatible", ReasonCode: "session_authority_unavailable"})
	calls := recordReplacements(t, "")
	result, err := EnsureGatewayRuntime(context.Background(), h, 5, false)
	if err != nil || result.ReasonCode != "session_authority_unavailable" || len(*calls) != 0 {
		t.Fatalf("%+v %v %v", result, err, *calls)
	}
}

func TestEnsureReplacesAtMostOnceThenReports(t *testing.T) {
	h := replaceHome(t)
	scriptVerdicts(t, ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol"})
	calls := recordReplacements(t, "")
	result, err := EnsureGatewayRuntime(context.Background(), h, 5, false)
	if err != nil || result.State != "incompatible" || result.ReasonCode != "runtime_protocol" || len(*calls) != 1 || (*calls)[0] != h {
		t.Fatalf("%+v %v %v", result, err, *calls)
	}
}

func TestARefusedReplacementIsReportedWithItsReason(t *testing.T) {
	h := replaceHome(t)
	scriptVerdicts(t, ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol"})
	recordReplacements(t, "foreign_install")
	result, err := EnsureGatewayRuntime(context.Background(), h, 5, false)
	if err != nil || result.State != "incompatible" || result.ReasonCode != "runtime_protocol" || !strings.Contains(result.Detail, "another Icebath install") {
		t.Fatalf("%+v %v", result, err)
	}
}

// Identify payload of an older owner without runtime_protocol
func ownerPayload(t *testing.T, h string, pid int, startTime any) *pyjson.Object {
	t.Helper()
	start, err := pyjson.Dumps(startTime)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(fmt.Sprintf(`{"protocol": 1, "kind": %q, "pid": %d, "start_time": %s, "hermes_home": %q, "supervisor": "manual"}`, gatewayKind, pid, start, h))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func TestReplacementNeverTargetsItself(t *testing.T) {
	h := replaceHome(t)
	swapSeam(t, &replaceInstallRootOf, func() string { return h })
	swapSeam(t, &replaceIdentifyOf, func(string, time.Duration) (string, *pyjson.Object, error) {
		return h, ownerPayload(t, h, os.Getpid(), 1), nil
	})
	if refusal := ReplaceIncompatibleOwner(context.Background(), h, pythread.Monotonic()+5); refusal != "owner_unverified" {
		t.Fatal(refusal)
	}
}
