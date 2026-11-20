package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Cached channel directory with aliases applied on read, the loader of the send command listing
func LoadChannelDirectory(ctx context.Context) *pyjson.Object { return loadChannelDirectory(ctx) }
