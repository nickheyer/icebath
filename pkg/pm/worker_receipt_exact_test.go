package pm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestWorkerReceiptIsExactEvenIfLatestIsReplaced(t *testing.T) {
	tmp := ieClientEnv(t)
	pubCurrentEnvironment(t, tmp, []string{})
	ctx := WithReceiptScope(context.Background())
	id := "my-update"
	restore := WorkerContext(ctx, &id)
	defer restore()
	var received []any
	real := acceptWorkerReceipt
	peSwap(t, &acceptWorkerReceipt, func(ctx context.Context, data any, updateID *string) error {
		peWrite(t, filepath.Join(receiptDir(ctx), "latest.json"), `{"update_id": "unrelated", "outcome": "failed"}`)
		received = append(received, data)
		return real(ctx, data, updateID)
	})
	if err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{}, Explicit: true, Plugins: &Members{Dirs: []MemberDir{}}}); err != nil {
		t.Fatal(err)
	}
	result := LastForUpdate(ctx, id, true)
	if len(received) == 0 || result == nil {
		t.Fatalf("received %v result %v", received, result)
	}
	got, _ := json.Marshal(result)
	want, _ := json.Marshal(pyjson.Plain(received[0]))
	if string(got) != string(want) || result["update_id"] != "my-update" || result["outcome"] != "ok" {
		t.Fatalf("result %s received %s", got, want)
	}
}
