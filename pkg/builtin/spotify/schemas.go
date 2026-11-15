package spotify

func stringProp() map[string]any { return map[string]any{"type": "string"} }

func intProp() map[string]any { return map[string]any{"type": "integer"} }

func boolProp() map[string]any { return map[string]any{"type": "boolean"} }

func stringArrayProp() map[string]any {
	return map[string]any{"type": "array", "items": stringProp()}
}

func enumProp(values ...string) map[string]any {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return map[string]any{"type": "string", "enum": enum}
}

func intDescProp(text string) map[string]any {
	return map[string]any{"type": "integer", "description": text}
}

// Tool schema with an object parameter block and its required names
func toolSchema(name, description string, properties map[string]any, required ...string) map[string]any {
	if len(required) == 0 {
		required = []string{"action"}
	}
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": req}}
}

func playbackSchema() map[string]any {
	return toolSchema("spotify_playback", "Control Spotify playback, inspect the active playback state, or fetch recently played tracks.", map[string]any{
		"action":    enumProp("get_state", "get_currently_playing", "play", "pause", "next", "previous", "seek", "set_repeat", "set_shuffle", "set_volume", "recently_played"),
		"device_id": stringProp(), "market": stringProp(), "context_uri": stringProp(),
		"uris": stringArrayProp(), "offset": map[string]any{"type": "object"}, "position_ms": intProp(),
		"state": map[string]any{"description": "For set_repeat use track/context/off. For set_shuffle use boolean-like true/false.",
			"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "boolean"}}},
		"volume_percent": intProp(), "limit": intDescProp("For recently_played: number of tracks (max 50)"),
		"after":  intDescProp("For recently_played: Unix ms cursor (after this timestamp)"),
		"before": intDescProp("For recently_played: Unix ms cursor (before this timestamp)"),
	})
}

func devicesSchema() map[string]any {
	return toolSchema("spotify_devices", "List Spotify Connect devices or transfer playback to a different device.",
		map[string]any{"action": enumProp("list", "transfer"), "device_id": stringProp(), "play": boolProp()})
}

func queueSchema() map[string]any {
	return toolSchema("spotify_queue", "Inspect the user's Spotify queue or add an item to it.",
		map[string]any{"action": enumProp("get", "add"), "uri": stringProp(), "device_id": stringProp()})
}

func searchSchema() map[string]any {
	return toolSchema("spotify_search", "Search the Spotify catalog for tracks, albums, artists, playlists, shows, or episodes.",
		map[string]any{"query": stringProp(), "types": stringArrayProp(), "type": stringProp(), "limit": intProp(), "offset": intProp(),
			"market": stringProp(), "include_external": stringProp()}, "query")
}

func playlistsSchema() map[string]any {
	return toolSchema("spotify_playlists", "List, inspect, create, update, and modify Spotify playlists.", map[string]any{
		"action":      enumProp("list", "get", "items", "create", "add_items", "remove_items", "update_details"),
		"playlist_id": stringProp(), "market": stringProp(),
		"limit":  intDescProp("For list and items: page size (max 50). get always returns the first 100 items"),
		"offset": intDescProp("For list and items: index of the first entry. Use items to page past 100 tracks"),
		"name":   stringProp(), "description": stringProp(),
		"public": boolProp(), "collaborative": boolProp(), "uris": stringArrayProp(), "position": intProp(), "snapshot_id": stringProp(),
	})
}

func albumsSchema() map[string]any {
	return toolSchema("spotify_albums", "Fetch Spotify album metadata or album tracks.",
		map[string]any{"action": enumProp("get", "tracks"), "album_id": stringProp(), "id": stringProp(), "market": stringProp(),
			"limit": intProp(), "offset": intProp()})
}

func librarySchema() map[string]any {
	return toolSchema("spotify_library", "List, save, or remove the user's saved Spotify tracks or albums. Use `kind` to select which.", map[string]any{
		"kind":   map[string]any{"type": "string", "enum": []any{"tracks", "albums"}, "description": "Which library to operate on"},
		"action": enumProp("list", "save", "remove"),
		"limit":  intProp(), "offset": intProp(), "market": stringProp(), "uris": stringArrayProp(), "ids": stringArrayProp(), "items": stringArrayProp(),
	}, "kind", "action")
}
