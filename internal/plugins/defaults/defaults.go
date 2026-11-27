package defaults

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers"
	"github.com/nickheyer/icebath/internal/plugins/defaults/contexts"
	"github.com/nickheyer/icebath/internal/plugins/defaults/delegation"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sessions"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends"
)

func Register(registries *plugins.Registries) {
	agents.Register(registries)
	tools.Register(registries)
	terminals.Register(registries)
	modelproviders.Register(registries)
	transports.Register(registries)
	webbackends.Register(registries)
	sessions.Register(registries)
	examples.Register(registries)
	skills.Register(registries)
	contexts.Register(registries)
	secrets.Register(registries)
	egress.Register(registries)
	delegation.Register(registries)
	browsers.Register(registries)
	generators.Register(registries)
	speech.Register(registries)
	hooks.Register(registries)
}
