package approvals

import "github.com/nickheyer/icebath/internal/approvals"

func Register(host *approvals.Approvals) *approvals.Registry {
	registry := new(approvals.Registry)
	host.Register(registry)
	return registry
}
