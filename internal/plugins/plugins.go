package plugins

import (
	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	pluginsapp "github.com/nickheyer/icebath/internal/plugins/app"
	pluginscli "github.com/nickheyer/icebath/internal/plugins/cli"
	pluginscredentials "github.com/nickheyer/icebath/internal/plugins/credentials"
	pluginsmodels "github.com/nickheyer/icebath/internal/plugins/models"
	pluginsprovider "github.com/nickheyer/icebath/internal/plugins/provider"
	pluginsserver "github.com/nickheyer/icebath/internal/plugins/server"
	pluginssessions "github.com/nickheyer/icebath/internal/plugins/sessions"
	pluginsslash "github.com/nickheyer/icebath/internal/plugins/slash"
	pluginstools "github.com/nickheyer/icebath/internal/plugins/tools"
	pluginstransport "github.com/nickheyer/icebath/internal/plugins/transport"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
)

type Registries struct {
	Config      *config.Config
	Server      *server.Registry
	App         *app.Registry
	CLI         *cli.Registry
	Provider    *provider.Registry
	Transport   *transport.Registry
	Credentials *credentials.Registry
	Models      *models.Registry
	Tools       *tools.Registry
	Sessions    *sessions.Registry
	Slash       *slash.Registry
}

func New(
	cfg *config.Config,
	srv *server.Server,
	a *app.App,
	c *cli.CLI,
	p *provider.Provider,
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
		Provider:    pluginsprovider.Register(p),
		Transport:   pluginstransport.Register(t),
		Credentials: pluginscredentials.Register(cr),
		Models:      pluginsmodels.Register(m),
		Tools:       pluginstools.Register(tl),
		Sessions:    pluginssessions.Register(s),
		Slash:       pluginsslash.Register(sl),
	}
}
