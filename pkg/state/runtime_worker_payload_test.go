package state

import (
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestWorkerReceiptRefusesPayloadThatIsNotADict(t *testing.T) {
	db, _, _, scope := setupWorker(t)
	payloads := []any{[]any{}, []any{"messages"}, "messages", int64(7), nil, (*jsonutil.Object)(nil), []any{int64(1)}, []any{jsonutil.NewObject()}, int64(0), false}
	for _, operation := range []string{"transcript.append", "execution.finish", "session.prompt", "session.context", "usage.main",
		"usage.auxiliary", "turn.acquire", "compression.watermark", "session.user_content", "session.create", "session.end"} {
		for _, payload := range payloads {
			in := scope
			in.Sequence, in.Operation, in.Payload = 1, operation, payload
			if reason := receiptRefusal(t, db, in); reason != "invalid_params" {
				t.Fatalf("%s %#v refused %q", operation, payload, reason)
			}
		}
	}
	if out := mustReceipt(t, db, appendReceipt(scope, 1, "after refusals")); !anyutil.Equal(out.ValueOr("count", nil), int64(1)) {
		t.Fatalf("first accepted receipt after refusals %v", out)
	}
}
