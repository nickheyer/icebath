package state

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Every row of one read only statement, the reader entry of wrappers outside the package
func (d *SessionDB) ReadAll(ctx context.Context, query string, args ...any) ([]*jsonutil.Object, error) {
	return d.readAll(ctx, query, args...)
}
