package dashboard

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
)

var testSkew Skew = &[2]string{"aaaaaaaaaa", "bbbbbbbbbb"}

// Retirement fence stand-in whose prepare answers idle and whose commit records the permit
type fakeRetireFence struct {
	idle      bool
	committed atomic.Bool
}

func (f *fakeRetireFence) Prepare() map[string]any {
	if f.idle {
		return map[string]any{"ok": true, "idle": true, "token": "t"}
	}
	return map[string]any{"ok": false, "idle": false}
}

func (f *fakeRetireFence) Commit(token string) map[string]any {
	f.committed.Store(token == "t")
	return map[string]any{"ok": f.committed.Load()}
}

func waitWatchdog(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func TestRetiresOnlyOnAProvenCodeChangeOutsideAnUpdate(t *testing.T) {
	if !ShouldRetireForSkew(testSkew, false) {
		t.Fatal("skew outside an update must retire")
	}
	if ShouldRetireForSkew(nil, false) {
		t.Fatal("no drift must stay up")
	}
	if ShouldRetireForSkew(testSkew, true) {
		t.Fatal("an update in flight must stay up")
	}
}

func runSkewWatchdog(t *testing.T, fence *fakeRetireFence, skews []Skew, update bool) bool {
	var exit atomic.Bool
	i := 0
	next := func() Skew {
		if i < len(skews) {
			i++
			return skews[i-1]
		}
		return skews[len(skews)-1]
	}
	waitWatchdog(t, StartCodeSkewWatchdog(&exit, next, func() bool { return update }, fence, 0.01, len(skews)+2))
	return exit.Load()
}

func TestSkewRetiresThroughTheFenceAfterTwoConsecutiveObservations(t *testing.T) {
	fence := &fakeRetireFence{idle: true}
	if !runSkewWatchdog(t, fence, []Skew{testSkew, testSkew}, false) {
		t.Fatal("two consecutive skews must retire")
	}
	if !fence.committed.Load() {
		t.Fatal("admission must close before exit")
	}
}

func TestASingleBlipDoesNotRetire(t *testing.T) {
	fence := &fakeRetireFence{idle: true}
	if runSkewWatchdog(t, fence, []Skew{testSkew, nil, testSkew, nil}, false) {
		t.Fatal("a single blip must not retire")
	}
	if fence.committed.Load() {
		t.Fatal("no commit without two consecutive skews")
	}
}

func TestABusyBackendWaitsInsteadOfCuttingOffItsClient(t *testing.T) {
	if runSkewWatchdog(t, &fakeRetireFence{idle: false}, []Skew{testSkew, testSkew, testSkew}, false) {
		t.Fatal("a busy backend must stay up")
	}
}

func TestNeverRetiresWhileAnUpdateIsInFlight(t *testing.T) {
	if runSkewWatchdog(t, &fakeRetireFence{idle: true}, []Skew{testSkew, testSkew, testSkew}, true) {
		t.Fatal("an update in flight must keep the backend up")
	}
}

// Writes a checkout whose HEAD names main at the given sha, read without spawning git
func writeFakeCheckout(t *testing.T, repo, sha string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", "main"), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultProbeRetiresOnceTheCheckoutMovesPastTheBootRevision(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	writeFakeCheckout(t, repo, "1111111111111111111111111111111111111111")
	t.Cleanup(pm.PatchRepoRoot(repo))
	gateway.RecordBootFingerprint()

	fence := &fakeRetireFence{idle: true}
	var unchanged atomic.Bool
	waitWatchdog(t, StartCodeSkewWatchdog(&unchanged, nil, func() bool { return false }, fence, 0.01, 4))
	if unchanged.Load() {
		t.Fatal("an unchanged checkout must stay up")
	}

	writeFakeCheckout(t, repo, "2222222222222222222222222222222222222222")
	var moved atomic.Bool
	waitWatchdog(t, StartCodeSkewWatchdog(&moved, nil, func() bool { return false }, fence, 0.01, 4))
	if !moved.Load() {
		t.Fatal("a checkout moved past the boot revision must retire")
	}
}
