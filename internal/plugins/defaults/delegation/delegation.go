package delegation

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/delegation/delegationdefault"
)

func Register(registries *plugins.Registries) {
	delegationdefault.Register(registries)
}
