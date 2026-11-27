package terminals

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/daytona"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/local"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/modal"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/singularity"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/ssh"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/vercel"
)

func Register(registries *plugins.Registries) {
	local.Register(registries)
	docker.Register(registries)
	singularity.Register(registries)
	modal.Register(registries)
	daytona.Register(registries)
	vercel.Register(registries)
	ssh.Register(registries)
}
