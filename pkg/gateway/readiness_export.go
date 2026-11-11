package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// State database check of a home, its status and detail the way the readiness probe reports them
func ProbeStateDB(h string) *pyjson.Object { return readinessProbeStateDB(h) }
