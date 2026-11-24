package gateway

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Cached channel directory with aliases applied on read, the loader of the send command listing
func LoadChannelDirectory(ctx context.Context) *jsonutil.Object { return loadChannelDirectory(ctx) }
