package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Every row of one read only statement, the reader entry of wrappers outside the package
func (d *SessionDB) ReadAll(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error) {
	return d.readAll(ctx, query, args...)
}
