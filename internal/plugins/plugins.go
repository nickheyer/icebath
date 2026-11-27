package plugins

import (
	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/delegation"
	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/launch"
	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/packages"
	pluginsapp "github.com/nickheyer/icebath/internal/plugins/app"
	pluginsapprovals "github.com/nickheyer/icebath/internal/plugins/approvals"
	pluginsauxiliary "github.com/nickheyer/icebath/internal/plugins/auxiliary"
	pluginsbotdesktop "github.com/nickheyer/icebath/internal/plugins/botdesktop"
	pluginsbrowser "github.com/nickheyer/icebath/internal/plugins/browser"
	pluginscli "github.com/nickheyer/icebath/internal/plugins/cli"
	pluginsconnectors "github.com/nickheyer/icebath/internal/plugins/connectors"
	pluginscontexts "github.com/nickheyer/icebath/internal/plugins/contexts"
	pluginscredentials "github.com/nickheyer/icebath/internal/plugins/credentials"
	pluginsdelegation "github.com/nickheyer/icebath/internal/plugins/delegation"
	pluginsgeneration "github.com/nickheyer/icebath/internal/plugins/generation"
	pluginshooks "github.com/nickheyer/icebath/internal/plugins/hooks"
	pluginslaunch "github.com/nickheyer/icebath/internal/plugins/launch"
	pluginsmcp "github.com/nickheyer/icebath/internal/plugins/mcp"
	pluginsmodels "github.com/nickheyer/icebath/internal/plugins/models"
	pluginspackages "github.com/nickheyer/icebath/internal/plugins/packages"
	pluginsprofile "github.com/nickheyer/icebath/internal/plugins/profile"
	pluginsprompt "github.com/nickheyer/icebath/internal/plugins/prompt"
	pluginsprovider "github.com/nickheyer/icebath/internal/plugins/provider"
	pluginssecretcommands "github.com/nickheyer/icebath/internal/plugins/secretcommands"
	pluginssecrets "github.com/nickheyer/icebath/internal/plugins/secrets"
	pluginssecretstores "github.com/nickheyer/icebath/internal/plugins/secretstores"
	pluginsserver "github.com/nickheyer/icebath/internal/plugins/server"
	pluginssessions "github.com/nickheyer/icebath/internal/plugins/sessions"
	pluginssignins "github.com/nickheyer/icebath/internal/plugins/signins"
	pluginsskills "github.com/nickheyer/icebath/internal/plugins/skills"
	pluginsslash "github.com/nickheyer/icebath/internal/plugins/slash"
	pluginsspeech "github.com/nickheyer/icebath/internal/plugins/speech"
	pluginsterminal "github.com/nickheyer/icebath/internal/plugins/terminal"
	pluginstools "github.com/nickheyer/icebath/internal/plugins/tools"
	pluginstransport "github.com/nickheyer/icebath/internal/plugins/transport"
	pluginsvault "github.com/nickheyer/icebath/internal/plugins/vault"
	pluginsviews "github.com/nickheyer/icebath/internal/plugins/views"
	pluginsweb "github.com/nickheyer/icebath/internal/plugins/web"
	pluginsworktrees "github.com/nickheyer/icebath/internal/plugins/worktrees"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
	"github.com/nickheyer/icebath/internal/secrets"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	secretscli "github.com/nickheyer/icebath/internal/secrets/cli"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/internal/vault"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/internal/worktrees"
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
	// Ways to sign the accounts of a kind of provider in from the command line, which auth add runs
	SignIns *providercli.Registry
	// Clients for each provider API format
	Transport *transport.Registry
	// Credentials of the accounts of providers
	Credentials *credentials.Registry
	// Model lists of providers
	Models *models.Registry
	// Tools the agent can call, with how the app shows them and the risks they ask approval for
	Tools *tools.Registry
	// Channels approval prompts can go through, and through the host the approvals and command guards every tool shares
	Approvals *approvals.Registry
	// Session stores
	Sessions *sessions.Registry
	// Slash commands the server runs
	Slash *slash.Registry
	// Sections of the agent's system prompt
	Prompt *prompt.Registry
	// How the chat shows the calls of tools whose arguments and text are not the best way to read them
	Views *toolsapp.Registry
	// Services that answer web searches and read pages
	Web *web.Registry
	// Side jobs that run on helper models, each with its settings
	Auxiliary *auxiliary.Registry
	// Places the agent's commands run, and through the host the environments and background processes of sessions
	Terminal *terminal.Registry
	// Skills plugins bring, such as the bundled ones, beside the user's own
	Skills *skills.Registry
	// MCP servers plugins offer for install, or ship to connect without one
	MCP *mcp.Registry
	// Tools, runtimes and Python dependency sets features need on this machine, and through the host the package manager that installs them
	Packages *packages.Registry
	// Engines that keep long conversations within the model's limit, such as the built-in compressor
	Contexts *contexts.Registry
	// Flags of the launch of the app from the command line, and what features do around it as it runs
	Launch *launch.Registry
	// What features that work in git worktrees add to their upkeep, and through the host the trees sessions work in
	Worktrees *worktrees.Registry
	// Secret stores keys and other variables come from, such as Bitwarden and 1Password, and through the host the environment each profile reads
	Secrets *secrets.Registry
	// Commands of the secret stores, under icebath secrets
	SecretCommands *cli.Registry
	// How the secret stores screen shows each store and what it can do with it
	SecretStores *secretsapp.Registry
	// Password managers the autofill vault reads website logins from, and through the host the vault of each profile
	Vault *vault.Registry
	// Cloud browser services and browsers that hold pages of their own, and through the host the Chromium of this machine
	Browser *browser.Registry
	// Backends that route the tasks of delegate_task to where their subagents run, such as the default one following the delegation settings
	Delegation *delegation.Registry
	// What features do as a profile's Bot Desktop comes up, goes down or changes hands, and through the host the screens themselves
	Desktop *botdesktop.Registry
	// Services that draw pictures and make videos, such as FAL and OpenAI, and through the host the one each profile uses
	Generation *generation.Registry
	// Callbacks set on the agent's lifecycle hook points, such as the user's shell hooks, and through the host what fires them
	Hooks *hooks.Registry
	// Voices that read text aloud, recognizers that turn speech into text and engines that hear a wake word, and through the host the
	// pipelines text_to_speech, transcription and voice mode run on
	Speech *speech.Registry
}

func New(
	cfg *config.Config,
	srv *server.Server,
	a *app.App,
	c *cli.CLI,
	pr *profile.Profiles,
	p *provider.Provider,
	cn *providerapp.Connectors,
	si *providercli.SignIns,
	t *transport.Transport,
	cr *credentials.Credentials,
	m *models.Models,
	tl *tools.Tools,
	s *sessions.Sessions,
	sl *slash.Slash,
	pm *prompt.Prompt,
	tv *toolsapp.Views,
	w *web.Web,
	ax *auxiliary.Auxiliary,
	tm *terminal.Terminal,
	sk *skills.Skills,
	mc *mcp.MCP,
	ap *approvals.Approvals,
	pk *packages.Packages,
	cx *contexts.Contexts,
	ln *launch.Launch,
	wt *worktrees.Worktrees,
	sc *secrets.Secrets,
	scc *secretscli.Commands,
	ss *secretsapp.Stores,
	va *vault.Vault,
	br *browser.Browser,
	bd *botdesktop.Desktop,
	dl *delegation.Delegation,
	gn *generation.Generation,
	hk *hooks.Hooks,
	sp *speech.Speech,
) *Registries {
	return &Registries{
		Config:      cfg,
		Server:      pluginsserver.Register(srv),
		App:         pluginsapp.Register(a),
		CLI:         pluginscli.Register(c),
		Profile:     pluginsprofile.Register(pr),
		Provider:    pluginsprovider.Register(p),
		Connectors:  pluginsconnectors.Register(cn),
		SignIns:     pluginssignins.Register(si),
		Transport:   pluginstransport.Register(t),
		Credentials: pluginscredentials.Register(cr),
		Models:      pluginsmodels.Register(m),
		Tools:       pluginstools.Register(tl),
		Sessions:    pluginssessions.Register(s),
		Slash:       pluginsslash.Register(sl),
		Prompt:      pluginsprompt.Register(pm),
		Views:       pluginsviews.Register(tv),
		Web:         pluginsweb.Register(w),
		Auxiliary:   pluginsauxiliary.Register(ax),
		Terminal:    pluginsterminal.Register(tm),
		Skills:      pluginsskills.Register(sk),
		MCP:         pluginsmcp.Register(mc),
		Approvals:   pluginsapprovals.Register(ap),
		Packages:    pluginspackages.Register(pk),
		Contexts:    pluginscontexts.Register(cx),
		Launch:      pluginslaunch.Register(ln),
		Worktrees:   pluginsworktrees.Register(wt),

		Secrets:        pluginssecrets.Register(sc),
		SecretCommands: pluginssecretcommands.Register(scc),
		SecretStores:   pluginssecretstores.Register(ss),
		Vault:          pluginsvault.Register(va),
		Browser:        pluginsbrowser.Register(br),
		Desktop:        pluginsbotdesktop.Register(bd),
		Delegation:     pluginsdelegation.Register(dl),
		Generation:     pluginsgeneration.Register(gn),
		Hooks:          pluginshooks.Register(hk),
		Speech:         pluginsspeech.Register(sp),
	}
}
