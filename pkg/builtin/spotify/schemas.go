package spotify

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func stringProp() *jsonutil.Object { return jsonutil.ObjectOf("type", "string") }

func intProp() *jsonutil.Object { return jsonutil.ObjectOf("type", "integer") }

func boolProp() *jsonutil.Object { return jsonutil.ObjectOf("type", "boolean") }

func stringArrayProp() *jsonutil.Object {
	return jsonutil.ObjectOf("type", "array", "items", stringProp())
}

func enumProp(values ...string) *jsonutil.Object {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return jsonutil.ObjectOf("type", "string", "enum", enum)
}

func intDescProp(text string) *jsonutil.Object {
	return jsonutil.ObjectOf("type", "integer", "description", text)
}

// Tool schema with an object parameter block and its required names
func toolSchema(name, description string, properties *jsonutil.Object, required ...string) *jsonutil.Object {
	if len(required) == 0 {
		required = []string{"action"}
	}
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return jsonutil.ObjectOf("name", name, "description", description,
		"parameters", jsonutil.ObjectOf("type", "object", "properties", properties, "required", req))
}

func playbackSchema() *jsonutil.Object {
	return toolSchema("spotify_playback", "Control Spotify playback, inspect the active playback state, or fetch recently played tracks.", jsonutil.ObjectOf(
		"action", enumProp("get_state", "get_currently_playing", "play", "pause", "next", "previous", "seek", "set_repeat", "set_shuffle", "set_volume", "recently_played"),
		"device_id", stringProp(), "market", stringProp(), "context_uri", stringProp(),
		"uris", stringArrayProp(), "offset", jsonutil.ObjectOf("type", "object"), "position_ms", intProp(),
		"state", jsonutil.ObjectOf("description", "For set_repeat use track/context/off. For set_shuffle use boolean-like true/false.",
			"oneOf", []any{jsonutil.ObjectOf("type", "string"), jsonutil.ObjectOf("type", "boolean")}),
		"volume_percent", intProp(), "limit", intDescProp("For recently_played: number of tracks (max 50)"),
		"after", intDescProp("For recently_played: Unix ms cursor (after this timestamp)"),
		"before", intDescProp("For recently_played: Unix ms cursor (before this timestamp)"),
	))
}

func devicesSchema() *jsonutil.Object {
	return toolSchema("spotify_devices", "List Spotify Connect devices or transfer playback to a different device.",
		jsonutil.ObjectOf("action", enumProp("list", "transfer"), "device_id", stringProp(), "play", boolProp()))
}

func queueSchema() *jsonutil.Object {
	return toolSchema("spotify_queue", "Inspect the user's Spotify queue or add an item to it.",
		jsonutil.ObjectOf("action", enumProp("get", "add"), "uri", stringProp(), "device_id", stringProp()))
}

func searchSchema() *jsonutil.Object {
	return toolSchema("spotify_search", "Search the Spotify catalog for tracks, albums, artists, playlists, shows, or episodes.",
		jsonutil.ObjectOf("query", stringProp(), "types", stringArrayProp(), "type", stringProp(), "limit", intProp(), "offset", intProp(),
			"market", stringProp(), "include_external", stringProp()), "query")
}

func playlistsSchema() *jsonutil.Object {
	return toolSchema("spotify_playlists", "List, inspect, create, update, and modify Spotify playlists.", jsonutil.ObjectOf(
		"action", enumProp("list", "get", "items", "create", "add_items", "remove_items", "update_details"),
		"playlist_id", stringProp(), "market", stringProp(),
		"limit", intDescProp("For list and items: page size (max 50). get always returns the first 100 items"),
		"offset", intDescProp("For list and items: index of the first entry. Use items to page past 100 tracks"),
		"name", stringProp(), "description", stringProp(),
		"public", boolProp(), "collaborative", boolProp(), "uris", stringArrayProp(), "position", intProp(), "snapshot_id", stringProp(),
	))
}

func albumsSchema() *jsonutil.Object {
	return toolSchema("spotify_albums", "Fetch Spotify album metadata or album tracks.",
		jsonutil.ObjectOf("action", enumProp("get", "tracks"), "album_id", stringProp(), "id", stringProp(), "market", stringProp(),
			"limit", intProp(), "offset", intProp()))
}

func librarySchema() *jsonutil.Object {
	return toolSchema("spotify_library", "List, save, or remove the user's saved Spotify tracks or albums. Use `kind` to select which.", jsonutil.ObjectOf(
		"kind", jsonutil.ObjectOf("type", "string", "enum", []any{"tracks", "albums"}, "description", "Which library to operate on"),
		"action", enumProp("list", "save", "remove"),
		"limit", intProp(), "offset", intProp(), "market", stringProp(), "uris", stringArrayProp(), "ids", stringArrayProp(), "items", stringArrayProp(),
	), "kind", "action")
}
