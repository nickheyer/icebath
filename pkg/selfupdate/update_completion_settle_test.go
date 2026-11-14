package selfupdate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestUnwritableGatewayStatusNeverFailsASettledCommit(t *testing.T) {
	tmp := t.TempDir()
	vsSwap(t, &completionArm, func(context.Context, string) (string, error) { return "", nil })
	receipt := pyjson.ObjectOf("update_id", "u1", "outcome", "success", "finished_at", "now")
	vsSwap(t, &completionTerminalReceipt, func(context.Context, map[string]any) (*pyjson.Object, error) { return receipt, nil })
	h := filepath.Join(tmp, "home-is-a-file")
	if err := os.WriteFile(h, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	result := filepath.Join(tmp, "result.json")
	request := map[string]any{"source": tmp, "receipt": pyjson.ObjectOf("update_id", "u1"), "gateway_mode": true, "home": h, "pm_receipt": nil}
	code, err := settleAfterCommit(context.Background(), io.Discard, request, result, "dependencies", "pip failed")
	if err != nil || code != 0 {
		t.Fatalf("exit %d %v", code, err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	obj := answer.(*pyjson.Object)
	if !pyjson.Equal(obj.GetOr("exit_code", nil), 0) || !pyjson.Equal(obj.GetOr("receipt", nil), receipt) {
		t.Fatalf("answer %s", raw)
	}
}
