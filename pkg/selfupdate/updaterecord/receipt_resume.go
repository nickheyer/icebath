package updaterecord

import (
	"context"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Opens an existing run from its persisted data under its own identity, restoring the enclosing receipt when it finalizes
func ResumeReceipt(ctx context.Context, data *jsonutil.Object) {
	receipt := &UpdateReceipt{Data: data, CorrelationID: anyutil.String(data.ValueOr("update_id", nil))}
	st := stateOf(ctx)
	st.mu.Lock()
	receipt.begun, receipt.prev, receipt.owner = true, st.current, st
	st.current = receipt
	st.mu.Unlock()
}
