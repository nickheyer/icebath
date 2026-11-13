package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// GetConnectionFor handles getConnectionFor for the calling window
func (b *ConnectionsService) GetConnectionFor(ctx context.Context, payload any) (backend.Connection, error) {
	windowID := ""
	if w := callerWindow(b.shell, ctx); w != nil {
		windowID = w.w.Name()
	}
	return b.svc.GetConnectionFor(windowID, payload)
}
