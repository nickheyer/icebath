package updaterecord

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Opens an existing run from its persisted data under its own identity, restoring the enclosing receipt when it finalizes
func ResumeReceipt(ctx context.Context, data *pyjson.Object) {
	receipt := &UpdateReceipt{Data: data, CorrelationID: pyrepr.Str(data.GetOr("update_id", nil))}
	st := stateOf(ctx)
	st.mu.Lock()
	receipt.begun, receipt.prev, receipt.owner = true, st.current, st
	st.current = receipt
	st.mu.Unlock()
}
