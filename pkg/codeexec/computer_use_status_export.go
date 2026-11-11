package codeexec

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Computer Use readiness payload of the configured backend for the desktop card
func ComputerUseStatus(env *tools.Env) (*pyjson.Object, error) {
	return computerUseStatus(env, nil)
}
