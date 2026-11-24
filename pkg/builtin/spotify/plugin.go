// Package spotify is the Spotify catalog plugin port, its seven Web API tools and the icebath spotify login command
package spotify

import (
	"context"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func init() { plugins.RegisterPort(Plugin{}) }

// Spotify catalog plugin port, active once icebath plugins install spotify lands it in a home
type Plugin struct{}

// Toolset every Spotify tool registers under
const Toolset = "spotify"

// Name, schema builder, handler and emoji of each tool in registration order
var spotifyTools = []struct {
	name    string
	schema  func() *jsonutil.Object
	handler tools.Handler
	emoji   string
}{
	{"spotify_playback", playbackSchema, handleSpotifyPlayback, "\U0001F3B5"},
	{"spotify_devices", devicesSchema, handleSpotifyDevices, "\U0001F508"},
	{"spotify_queue", queueSchema, handleSpotifyQueue, "\U0001F4FB"},
	{"spotify_search", searchSchema, handleSpotifySearch, "\U0001F50E"},
	{"spotify_playlists", playlistsSchema, handleSpotifyPlaylists, "\U0001F4DA"},
	{"spotify_albums", albumsSchema, handleSpotifyAlbums, "\U0001F4BF"},
	{"spotify_library", librarySchema, handleSpotifyLibrary, "❤️"},
}

// Manifest of the spotify catalog plugin at its compiled-in pin
func (Plugin) Manifest() plugins.Manifest { return plugins.CatalogPortManifest("spotify") }

// Registers the seven login gated tools in the spotify toolset and the icebath spotify command
func (Plugin) Register(ctx *plugins.Context) error {
	for _, t := range spotifyTools {
		if _, err := ctx.RegisterTool(context.Background(), plugins.ToolSpec{Name: t.name, Toolset: Toolset,
			Schema: t.schema(), Handler: t.handler, Check: checkSpotifyAvailable, Emoji: t.emoji}); err != nil {
			return err
		}
	}
	ctx.RegisterCLICommand("spotify", "Log in to Spotify and check the plugin's auth status", setupCLI, nil,
		"Spotify login for the spotify plugin (loopback PKCE; needs your own Spotify app).")
	return nil
}
