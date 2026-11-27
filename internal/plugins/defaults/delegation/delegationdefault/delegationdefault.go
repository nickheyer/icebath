package delegationdefault

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/delegation/delegationdefault/delegation"
)

func Register(registries *plugins.Registries) {
	delegation.Register(registries.Delegation)
}
