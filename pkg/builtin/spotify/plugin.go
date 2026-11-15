// Package spotify is the built in Spotify plugin, its seven Web API tools and the icebath spotify login command
package spotify

import (
	"context"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled-in Spotify plugin
type Plugin struct{}

// Toolset every Spotify tool registers under
const Toolset = "spotify"

const pluginDescription = "Spotify for Icebath: seven tools (playback, devices, queue, search, playlists, albums, library) " +
	"over the Spotify Web API, plus `icebath spotify login|status|logout` (loopback PKCE with your own Spotify " +
	"developer app). Playback control needs Spotify Premium and an active Spotify Connect device."

const pluginHomepage = "https://github.com/NousResearch/hermes-official-plugins/tree/main/spotify"

// Name, schema builder, handler and emoji of each tool in registration order
var spotifyTools = []struct {
	name    string
	schema  func() map[string]any
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

func toolNames() []string {
	out := make([]string, len(spotifyTools))
	for i, t := range spotifyTools {
		out[i] = t.name
	}
	return out
}

// Manifest of the built in spotify plugin, loaded by default like a bundled backend
func (Plugin) Manifest() plugins.Manifest {
	names := toolNames()
	provided := make([]any, len(names))
	for i, n := range names {
		provided[i] = n
	}
	return plugins.Manifest{
		Name: "spotify", Version: "2.1.0", Description: pluginDescription, Author: "NousResearch", Kind: "backend",
		Homepage: pluginHomepage, ProvidesTools: names, ProvidesHooks: []string{},
		Raw: map[string]any{"name": "spotify", "label": "Spotify", "kind": "backend", "version": "2.1.0",
			"author": "NousResearch", "homepage": pluginHomepage, "description": pluginDescription,
			"provides_tools": provided, "provides_hooks": []any{},
			"optional_env": []any{
				map[string]any{"name": "ICEBATH_SPOTIFY_CLIENT_ID", "prompt": "Spotify Client ID", "password": false,
					"description": "Client ID of your Spotify developer app (`icebath spotify login` asks for it and saves it)"},
				map[string]any{"name": "ICEBATH_SPOTIFY_REDIRECT_URI", "prompt": "Spotify redirect URI", "password": false,
					"description": "Redirect URI allow-listed in your Spotify app (default: http://127.0.0.1:43827/spotify/callback; " +
						"set an https:// URI if your dashboard refuses the http form)"},
			}},
	}
}

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
