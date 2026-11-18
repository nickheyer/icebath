package codeexec

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Computer Use readiness payload of the configured backend for the desktop card
func ComputerUseStatus(ctx context.Context, env *tools.Env) (*pyjson.Object, error) {
	return computerUseStatus(ctx, env, nil)
}
