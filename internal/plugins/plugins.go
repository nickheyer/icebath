package plugins

import (
	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	pluginsapp "github.com/nickheyer/icebath/internal/plugins/app"
	pluginscli "github.com/nickheyer/icebath/internal/plugins/cli"
	pluginsconnectors "github.com/nickheyer/icebath/internal/plugins/connectors"
	pluginscredentials "github.com/nickheyer/icebath/internal/plugins/credentials"
	pluginsmodels "github.com/nickheyer/icebath/internal/plugins/models"
	pluginsprofile "github.com/nickheyer/icebath/internal/plugins/profile"
	pluginsprovider "github.com/nickheyer/icebath/internal/plugins/provider"
	pluginsserver "github.com/nickheyer/icebath/internal/plugins/server"
	pluginssessions "github.com/nickheyer/icebath/internal/plugins/sessions"
	pluginsslash "github.com/nickheyer/icebath/internal/plugins/slash"
	pluginstools "github.com/nickheyer/icebath/internal/plugins/tools"
	pluginstransport "github.com/nickheyer/icebath/internal/plugins/transport"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
)

// What a plugin can add to icebath, one registry per feature
type Registries struct {
	// Settings sections and their settings
	Config *config.Config
	// Connect services the server mounts
	Server *server.Registry
	// Screens of the terminal app: windows, dialogs, slash commands, keys and the settings they follow
	App *app.Registry
	// Commands of the command line
	CLI *cli.Registry
	// What each feature does when the profile in use changes, and when one is deleted
	Profile *profile.Registry
	// Kinds of model provider a profile can have
	Provider *provider.Registry
	// Ways to sign in the accounts of a kind of provider from the providers screen
	Connectors *providerapp.Registry
	// Clients for each provider API format
	Transport *transport.Registry
	// Credentials of the accounts of providers
	Credentials *credentials.Registry
	// Model lists of providers
	Models *models.Registry
	// Tools the agent can call, with how the app shows them and the risks they ask approval for
	Tools *tools.Registry
	// Session stores
	Sessions *sessions.Registry
	// Slash commands the server runs
	Slash *slash.Registry
}

func New(
	cfg *config.Config,
	srv *server.Server,
	a *app.App,
	c *cli.CLI,
	pr *profile.Profiles,
	p *provider.Provider,
	cn *providerapp.Connectors,
	t *transport.Transport,
	cr *credentials.Credentials,
	m *models.Models,
	tl *tools.Tools,
	s *sessions.Sessions,
	sl *slash.Slash,
) *Registries {
	return &Registries{
		Config:      cfg,
		Server:      pluginsserver.Register(srv),
		App:         pluginsapp.Register(a),
		CLI:         pluginscli.Register(c),
		Profile:     pluginsprofile.Register(pr),
		Provider:    pluginsprovider.Register(p),
		Connectors:  pluginsconnectors.Register(cn),
		Transport:   pluginstransport.Register(t),
		Credentials: pluginscredentials.Register(cr),
		Models:      pluginsmodels.Register(m),
		Tools:       pluginstools.Register(tl),
		Sessions:    pluginssessions.Register(s),
		Slash:       pluginsslash.Register(sl),
	}
}
