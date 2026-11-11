package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// OpenSessionDBAtPath opens a session store writable through the registry or read only after its schema probes
func OpenSessionDBAtPath(ctx context.Context, dbPath string, readOnly bool) (*state.DB, error) {
	return openSessionDBAtPath(ctx, dbPath, readOnly)
}
