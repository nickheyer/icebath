package spotify

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool arguments as Python values keyed by name
type toolArgs = map[string]any

// One action of a tool, returning the result text or the error its boundary renders
type actionHandler func(c apiClient, a toolArgs, action string) (string, error)

// Profile home a tool call reads its Spotify login from
func callHome(ctx context.Context, env *tools.Env) string {
	if env != nil && strings.TrimSpace(env.Home) != "" {
		return env.Home
	}
	return home.Dir(ctx)
}

// Whether the profile holds a usable Spotify login, false on any failure
func checkSpotifyAvailable(env *tools.Env) bool {
	h := home.Process()
	if env != nil && strings.TrimSpace(env.Home) != "" {
		h = env.Home
	}
	status, err := provider.SpotifyAuthStatus(h)
	if err != nil {
		return false
	}
	return pyrepr.Truthy(status["logged_in"])
}

func toolError(message string) string {
	return tools.JSON(tools.Dict("error", tools.BoundErrorText(message)))
}

func toolResult(v any) string { return tools.ToolResult(v) }

// Tool error of a failure, Spotify errors by their own text and others with their class name
func spotifyToolError(err error) string {
	var se spotifyErrorKind
	if errors.As(err, &se) {
		return toolError(err.Error())
	}
	return toolError(fmt.Sprintf("Spotify tool failed: %s: %s", exceptionName(err), pyrepr.ExceptionMessage(err)))
}

// The value, or a SpotifyError with the message when it is None
func required(value any, message string) (any, error) {
	if value == nil {
		return nil, &SpotifyError{Msg: message}
	}
	return value, nil
}

// Stripped text of a value, a SpotifyError with the message when blank
func nonblank(raw any, message string) (string, error) {
	value := ""
	if pyrepr.Truthy(raw) {
		value = pystr.Strip(pyrepr.Str(raw))
	}
	if value == "" {
		return "", &SpotifyError{Msg: message}
	}
	return value, nil
}

// Boolean of a bool or a yes or no word, the default for anything else
func coerceBool(raw any, def bool) bool {
	switch t := raw.(type) {
	case bool:
		return t
	case string:
		switch pystr.Lower(pystr.Strip(t)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

// Non blank stripped texts of a list or of a single value
func asList(raw any) []string {
	if raw == nil {
		return []string{}
	}
	items, ok := raw.([]any)
	if !ok {
		items = []any{raw}
	}
	out := []string{}
	for _, item := range items {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Python int of a value as the smallest Go integer holding it
func pyIntValue(v any) (any, error) {
	n, err := pyint.Of(v)
	if err != nil {
		return nil, err
	}
	return pyint.Value(n), nil
}

// Python int of the offset argument floored at zero
func offsetArg(a toolArgs) (any, error) {
	raw := a["offset"]
	if !pyrepr.Truthy(raw) {
		raw = int64(0)
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return nil, err
	}
	if n.Sign() < 0 {
		n = big.NewInt(0)
	}
	return pyint.Value(n), nil
}

// Limit argument clamped to Spotify's 1 to 50 window, the default when it is not a number
func limitArg(a toolArgs, def int64) int64 {
	value := def
	if n, err := pyint.Of(a["limit"]); err == nil {
		switch {
		case n.Cmp(big.NewInt(50)) > 0:
			value = 50
		case n.Cmp(big.NewInt(1)) < 0:
			value = 1
		default:
			value = n.Int64()
		}
		return value
	}
	return max(1, min(50, value))
}

// Successful action result with any extra keys ahead of the result
func okResult(action string, result any, extra ...any) string {
	pairs := append([]any{"success", true, "action", action}, extra...)
	pairs = append(pairs, "result", result)
	return toolResult(pyjson.ObjectOf(pairs...))
}

// Python subscript of a decoded JSON value by a string key with the KeyError or TypeError it raises
func subscript(v any, key string) (any, error) {
	switch t := v.(type) {
	case *pyjson.Object:
		if t != nil {
			if val, ok := t.Get(key); ok {
				return val, nil
			}
			return nil, &pyError{kind: "KeyError", msg: pyrepr.Repr(key)}
		}
	case map[string]any:
		if val, ok := t[key]; ok {
			return val, nil
		}
		return nil, &pyError{kind: "KeyError", msg: pyrepr.Repr(key)}
	case []any:
		return nil, &pyError{kind: "TypeError", msg: "list indices must be integers or slices, not str"}
	case string:
		return nil, &pyError{kind: "TypeError", msg: "string indices must be integers, not 'str'"}
	}
	return nil, &pyError{kind: "TypeError", msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(v))}
}

// Action text of the call, the default when blank
func actionOf(a toolArgs, def string) string {
	raw := a["action"]
	if !pyrepr.Truthy(raw) {
		raw = def
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
}

// Builds the client outside the error boundary so an auth failure propagates as the tool exception
func mustClient(ctx context.Context, env *tools.Env) apiClient {
	client, err := newSpotifyClient(ctx, callHome(ctx, env))
	if err != nil {
		panic(err)
	}
	return client
}

// Routes the action through the table inside the error boundary, prepare running before the lookup
func runDispatch(ctx context.Context, env *tools.Env, a toolArgs, toolName, def string, table map[string]actionHandler, prepare func(toolArgs) (toolArgs, error)) string {
	action := actionOf(a, def)
	client := mustClient(ctx, env)
	if prepare != nil {
		prepared, err := prepare(a)
		if err != nil {
			return spotifyToolError(err)
		}
		a = prepared
	}
	handler, ok := table[action]
	if !ok {
		return toolError(fmt.Sprintf("Unknown %s action: %s", toolName, action))
	}
	out, err := handler(client, a, action)
	if err != nil {
		return spotifyToolError(err)
	}
	return out
}

// Tool handler routing args through the action table
func dispatcher(toolName, def string, table map[string]actionHandler, prepare func(toolArgs) (toolArgs, error)) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		return runDispatch(ctx, call.Env, tools.PythonCallArgs(call), toolName, def, table, prepare)
	}
}

// Flag and fallback message a player read reports when Spotify answers 204
var emptyPlayback = map[string][2]string{
	"get_currently_playing": {"is_playing", "Spotify is not currently playing anything."},
	"get_state":             {"has_active_device", "No active Spotify playback session was found."},
}

// Player read result, an empty answer becoming an explanatory success payload
func pbRead(fetch func(market any) (any, error), a toolArgs, action string) (string, error) {
	payload, err := fetch(a["market"])
	if err != nil {
		return "", err
	}
	if obj, ok := payload.(*pyjson.Object); ok && pyrepr.Truthy(obj.GetOr("empty", nil)) {
		flagFallback := emptyPlayback[action]
		message := obj.GetOr("message", nil)
		if !pyrepr.Truthy(message) {
			message = flagFallback[1]
		}
		payload = pyjson.ObjectOf("success", true, "action", action, flagFallback[0], false,
			"status_code", obj.GetOr("status_code", int64(204)), "message", message)
	}
	return toolResult(payload), nil
}

// Context kind, URI prefix and URL fragment of the play contexts whose type is checked
var contextTypes = [][3]string{
	{"album", "spotify:album:", "/album/"}, {"playlist", "spotify:playlist:", "/playlist/"}, {"artist", "spotify:artist:", "/artist/"},
}

// Plays the first track inside its album and queues the rest behind it
func pbPlayTracks(c apiClient, a toolArgs, action string, uris []string) (string, error) {
	trackID, err := normalizeSpotifyID(uris[0], "track")
	if err != nil {
		return "", err
	}
	track, err := c.Request("GET", "/tracks/"+trackID, pyjson.ObjectOf("market", "from_token"), nil)
	if err != nil {
		return "", err
	}
	album, err := subscript(track, "album")
	if err != nil {
		return "", err
	}
	contextURI, err := subscript(album, "uri")
	if err != nil {
		return "", err
	}
	trackURI, err := subscript(track, "uri")
	if err != nil {
		return "", err
	}
	body := pyjson.ObjectOf("context_uri", contextURI, "offset", pyjson.ObjectOf("uri", trackURI), "position_ms", a["position_ms"])
	result, err := c.Request("PUT", "/me/player/play", pyjson.ObjectOf("device_id", a["device_id"]), body)
	if err != nil {
		return "", err
	}
	for _, uri := range uris[1:] {
		if _, err := c.Request("POST", "/me/player/queue", pyjson.ObjectOf("uri", uri, "device_id", a["device_id"]), nil); err != nil {
			return "", err
		}
	}
	return okResult(action, result, "started", trackURI, "context_uri", contextURI, "queued", stringsToAny(uris[1:])), nil
}

// Starts playback of a context, track list or offset on the chosen device
func pbPlay(c apiClient, a toolArgs, action string) (string, error) {
	var payloadOffset any
	if offset, ok := pyjson.AsObject(a["offset"]); ok {
		payloadOffset = stripNone(offset)
	}
	var uris []string
	if pyrepr.Truthy(a["uris"]) {
		normalized, err := normalizeSpotifyURIs(asList(a["uris"]), "track")
		if err != nil {
			return "", err
		}
		uris = normalized
	}
	if len(uris) > 0 && !pyrepr.Truthy(a["context_uri"]) {
		return pbPlayTracks(c, a, action, uris)
	}
	var contextURI any
	if pyrepr.Truthy(a["context_uri"]) {
		raw := pyrepr.Str(a["context_uri"])
		contextType := ""
		for _, ct := range contextTypes {
			if strings.HasPrefix(raw, ct[1]) || strings.Contains(raw, ct[2]) {
				contextType = ct[0]
				break
			}
		}
		normalized, err := normalizeSpotifyURI(raw, contextType)
		if err != nil {
			return "", err
		}
		contextURI = normalized
	}
	var urisValue any
	if uris != nil {
		urisValue = stringsToAny(uris)
	}
	body := pyjson.ObjectOf("context_uri", contextURI, "uris", urisValue, "offset", payloadOffset, "position_ms", a["position_ms"])
	result, err := c.Request("PUT", "/me/player/play", pyjson.ObjectOf("device_id", a["device_id"]), body)
	if err != nil {
		return "", err
	}
	return okResult(action, result), nil
}

// Player command whose only free argument is device_id, extra pairs being fixed params
func pbCmd(c apiClient, a toolArgs, action, method, path string, extra ...any) (string, error) {
	params := pyjson.ObjectOf(append(extra, "device_id", a["device_id"])...)
	result, err := c.Request(method, path, params, nil)
	if err != nil {
		return "", err
	}
	return okResult(action, result), nil
}

func pbDeviceCmd(method, path string) actionHandler {
	return func(c apiClient, a toolArgs, action string) (string, error) { return pbCmd(c, a, action, method, path) }
}

// PUT of the path with the required converted param plus device_id
func pbRequiredParam(path, param string, convert func(any) (any, error)) actionHandler {
	return func(c apiClient, a toolArgs, action string) (string, error) {
		raw, err := required(a[param], fmt.Sprintf("%s is required for action='%s'", param, action))
		if err != nil {
			return "", err
		}
		value, err := convert(raw)
		if err != nil {
			return "", err
		}
		return pbCmd(c, a, action, "PUT", path, param, value)
	}
}

// Repeat mode of the state argument, track context or off
func pbRepeatState(a toolArgs) (string, error) {
	state := ""
	if pyrepr.Truthy(a["state"]) {
		state = pystr.Lower(pystr.Strip(pyrepr.Str(a["state"])))
	}
	if state != "track" && state != "context" && state != "off" {
		return "", &SpotifyError{Msg: "state must be one of: track, context, off"}
	}
	return state, nil
}

// Recently played tracks paged by one of the after or before cursors
func pbRecentlyPlayed(c apiClient, a toolArgs, _ string) (string, error) {
	after, before := a["after"], a["before"]
	if pyrepr.Truthy(after) && pyrepr.Truthy(before) {
		return toolError("Provide only one of 'after' or 'before'"), nil
	}
	params := pyjson.ObjectOf("limit", limitArg(a, 20), "after", nil, "before", nil)
	if after != nil {
		v, err := pyIntValue(after)
		if err != nil {
			return "", err
		}
		params.Set("after", v)
	}
	if before != nil {
		v, err := pyIntValue(before)
		if err != nil {
			return "", err
		}
		params.Set("before", v)
	}
	result, err := c.Request("GET", "/me/player/recently-played", params, nil)
	if err != nil {
		return "", err
	}
	return toolResult(result), nil
}

var handleSpotifyPlayback = dispatcher("spotify_playback", "get_state", map[string]actionHandler{
	"get_state": func(c apiClient, a toolArgs, action string) (string, error) {
		return pbRead(c.GetPlaybackState, a, action)
	},
	"get_currently_playing": func(c apiClient, a toolArgs, action string) (string, error) {
		return pbRead(c.GetCurrentlyPlaying, a, action)
	},
	"play":     pbPlay,
	"pause":    pbDeviceCmd("PUT", "/me/player/pause"),
	"next":     pbDeviceCmd("POST", "/me/player/next"),
	"previous": pbDeviceCmd("POST", "/me/player/previous"),
	"seek":     pbRequiredParam("/me/player/seek", "position_ms", pyIntValue),
	"set_repeat": func(c apiClient, a toolArgs, action string) (string, error) {
		state, err := pbRepeatState(a)
		if err != nil {
			return "", err
		}
		return pbCmd(c, a, action, "PUT", "/me/player/repeat", "state", state)
	},
	"set_shuffle": func(c apiClient, a toolArgs, action string) (string, error) {
		state := "false"
		if coerceBool(a["state"], false) {
			state = "true"
		}
		return pbCmd(c, a, action, "PUT", "/me/player/shuffle", "state", state)
	},
	"set_volume": pbRequiredParam("/me/player/volume", "volume_percent", func(v any) (any, error) {
		n, err := pyint.Of(v)
		if err != nil {
			return nil, err
		}
		switch {
		case n.Sign() < 0:
			return int64(0), nil
		case n.Cmp(big.NewInt(100)) > 0:
			return int64(100), nil
		}
		return n.Int64(), nil
	}),
	"recently_played": pbRecentlyPlayed,
}, nil)

var handleSpotifyDevices = dispatcher("spotify_devices", "list", map[string]actionHandler{
	"list": func(c apiClient, _ toolArgs, _ string) (string, error) {
		result, err := c.Request("GET", "/me/player/devices", nil, nil)
		if err != nil {
			return "", err
		}
		return toolResult(result), nil
	},
	"transfer": func(c apiClient, a toolArgs, action string) (string, error) {
		deviceID, err := nonblank(a["device_id"], "device_id is required for action='transfer'")
		if err != nil {
			return "", err
		}
		result, err := c.Request("PUT", "/me/player", nil, pyjson.ObjectOf("device_ids", []any{deviceID}, "play", coerceBool(a["play"], false)))
		if err != nil {
			return "", err
		}
		return okResult(action, result), nil
	},
}, nil)

// Adds one item to the queue, the URI echoed back
func queueAdd(c apiClient, a toolArgs, action string) (string, error) {
	raw := ""
	if pyrepr.Truthy(a["uri"]) {
		raw = pyrepr.Str(a["uri"])
	}
	uri, err := normalizeSpotifyURI(raw, "")
	if err != nil {
		return "", err
	}
	result, err := c.Request("POST", "/me/player/queue", pyjson.ObjectOf("uri", uri, "device_id", a["device_id"]), nil)
	if err != nil {
		return "", err
	}
	return okResult(action, result, "uri", uri), nil
}

var handleSpotifyQueue = dispatcher("spotify_queue", "get", map[string]actionHandler{
	"get": func(c apiClient, _ toolArgs, _ string) (string, error) {
		result, err := c.Request("GET", "/me/player/queue", nil, nil)
		if err != nil {
			return "", err
		}
		return toolResult(result), nil
	},
	"add": queueAdd,
}, nil)

var searchTypes = map[string]bool{"album": true, "artist": true, "playlist": true, "track": true, "show": true, "episode": true, "audiobook": true}

// Searches the catalog, validation and paging read outside the request's error boundary
func handleSpotifySearch(ctx context.Context, call *tools.Call) string {
	a := tools.PythonCallArgs(call)
	client := mustClient(ctx, call.Env)
	query := ""
	if pyrepr.Truthy(a["query"]) {
		query = pystr.Strip(pyrepr.Str(a["query"]))
	}
	if query == "" {
		return toolError("query is required")
	}
	rawTypes := a["types"]
	if !pyrepr.Truthy(rawTypes) {
		rawTypes = a["type"]
	}
	if !pyrepr.Truthy(rawTypes) {
		rawTypes = []any{"track"}
	}
	var selected []string
	for _, value := range asList(rawTypes) {
		if lower := pystr.Lower(value); searchTypes[lower] {
			selected = append(selected, lower)
		}
	}
	if len(selected) == 0 {
		return toolError("types must contain one or more of: album, artist, playlist, track, show, episode, audiobook")
	}
	offset, err := offsetArg(a)
	if err != nil {
		panic(err)
	}
	params := pyjson.ObjectOf("q", query, "type", strings.Join(selected, ","), "limit", limitArg(a, 10), "offset", offset,
		"market", a["market"], "include_external", a["include_external"])
	result, err := client.Request("GET", "/search", params, nil)
	if err != nil {
		return spotifyToolError(err)
	}
	return toolResult(result)
}

// Limit, offset and market params of a paged read
func pageParams(a toolArgs) (*pyjson.Object, error) {
	limit := limitArg(a, 20)
	offset, err := offsetArg(a)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("limit", limit, "offset", offset, "market", a["market"]), nil
}

// API path of the playlist the playlist_id argument names, with a suffix
func playlistPath(a toolArgs, suffix string) (string, error) {
	raw := ""
	if pyrepr.Truthy(a["playlist_id"]) {
		raw = pyrepr.Str(a["playlist_id"])
	}
	id, err := normalizeSpotifyID(raw, "playlist")
	if err != nil {
		return "", err
	}
	return "/playlists/" + id + suffix, nil
}

// Runs a request and renders its result, the error passed on to the boundary
func resultOf(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return toolResult(v), nil
}

var handleSpotifyPlaylists = dispatcher("spotify_playlists", "list", map[string]actionHandler{
	"list": func(c apiClient, a toolArgs, _ string) (string, error) {
		limit := limitArg(a, 20)
		offset, err := offsetArg(a)
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("GET", "/me/playlists", pyjson.ObjectOf("limit", limit, "offset", offset), nil))
	},
	"get": func(c apiClient, a toolArgs, _ string) (string, error) {
		path, err := playlistPath(a, "")
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("GET", path, pyjson.ObjectOf("market", a["market"]), nil))
	},
	"items": func(c apiClient, a toolArgs, _ string) (string, error) {
		path, err := playlistPath(a, "/items")
		if err != nil {
			return "", err
		}
		params, err := pageParams(a)
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("GET", path, params, nil))
	},
	"create": func(c apiClient, a toolArgs, _ string) (string, error) {
		name, err := nonblank(a["name"], "name is required for action='create'")
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("POST", "/me/playlists", nil, pyjson.ObjectOf("name", name, "public", coerceBool(a["public"], false),
			"collaborative", coerceBool(a["collaborative"], false), "description", a["description"])))
	},
	"add_items": func(c apiClient, a toolArgs, _ string) (string, error) {
		path, err := playlistPath(a, "/items")
		if err != nil {
			return "", err
		}
		uris, err := normalizeSpotifyURIs(asList(a["uris"]), "")
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("POST", path, nil, pyjson.ObjectOf("uris", stringsToAny(uris), "position", a["position"])))
	},
	"remove_items": func(c apiClient, a toolArgs, _ string) (string, error) {
		path, err := playlistPath(a, "/items")
		if err != nil {
			return "", err
		}
		uris, err := normalizeSpotifyURIs(asList(a["uris"]), "")
		if err != nil {
			return "", err
		}
		items := make([]any, len(uris))
		for i, u := range uris {
			items[i] = pyjson.ObjectOf("uri", u)
		}
		return resultOf(c.Request("DELETE", path, nil, pyjson.ObjectOf("items", items, "snapshot_id", a["snapshot_id"])))
	},
	"update_details": func(c apiClient, a toolArgs, _ string) (string, error) {
		path, err := playlistPath(a, "")
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("PUT", path, nil, pyjson.ObjectOf("name", a["name"], "public", a["public"],
			"collaborative", a["collaborative"], "description", a["description"])))
	},
}, nil)

// Args with the album API path the album_id or id argument names
func prepareAlbum(a toolArgs) (toolArgs, error) {
	raw := a["album_id"]
	if !pyrepr.Truthy(raw) {
		raw = a["id"]
	}
	text := ""
	if pyrepr.Truthy(raw) {
		text = pyrepr.Str(raw)
	}
	id, err := normalizeSpotifyID(text, "album")
	if err != nil {
		return nil, err
	}
	out := make(toolArgs, len(a)+1)
	for k, v := range a {
		out[k] = v
	}
	out["_path"] = "/albums/" + id
	return out, nil
}

var handleSpotifyAlbums = dispatcher("spotify_albums", "get", map[string]actionHandler{
	"get": func(c apiClient, a toolArgs, _ string) (string, error) {
		return resultOf(c.Request("GET", pyrepr.Str(a["_path"]), pyjson.ObjectOf("market", a["market"]), nil))
	},
	"tracks": func(c apiClient, a toolArgs, _ string) (string, error) {
		params, err := pageParams(a)
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("GET", pyrepr.Str(a["_path"])+"/tracks", params, nil))
	},
}, prepareAlbum)

// Comma joined URIs of the ids or items arguments for a library remove
func libRemoveURIs(a toolArgs) (string, error) {
	itemType := pyrepr.Str(a["_item_type"])
	raw := a["ids"]
	if !pyrepr.Truthy(raw) {
		raw = a["items"]
	}
	var ids []string
	for _, item := range asList(raw) {
		id, err := normalizeSpotifyID(item, itemType)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", &SpotifyError{Msg: "ids/items is required for action='remove'"}
	}
	uris := make([]string, len(ids))
	for i, id := range ids {
		uris[i] = "spotify:" + itemType + ":" + id
	}
	return strings.Join(uris, ","), nil
}

var libraryTable = map[string]actionHandler{
	"list": func(c apiClient, a toolArgs, _ string) (string, error) {
		params, err := pageParams(a)
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("GET", "/me/"+pyrepr.Str(a["kind"]), params, nil))
	},
	"save": func(c apiClient, a toolArgs, _ string) (string, error) {
		raw := a["uris"]
		if !pyrepr.Truthy(raw) {
			raw = a["items"]
		}
		uris, err := normalizeSpotifyURIs(asList(raw), pyrepr.Str(a["_item_type"]))
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("PUT", "/me/library", pyjson.ObjectOf("uris", strings.Join(uris, ",")), nil))
	},
	"remove": func(c apiClient, a toolArgs, _ string) (string, error) {
		uris, err := libRemoveURIs(a)
		if err != nil {
			return "", err
		}
		return resultOf(c.Request("DELETE", "/me/library", pyjson.ObjectOf("uris", uris), nil))
	},
}

// Saved tracks or albums selected by kind, checked before any client is built
func handleSpotifyLibrary(ctx context.Context, call *tools.Call) string {
	a := tools.PythonCallArgs(call)
	kind := ""
	if pyrepr.Truthy(a["kind"]) {
		kind = pystr.Lower(pystr.Strip(pyrepr.Str(a["kind"])))
	}
	if kind != "tracks" && kind != "albums" {
		return toolError("kind must be one of: tracks, albums")
	}
	merged := make(toolArgs, len(a)+2)
	for k, v := range a {
		merged[k] = v
	}
	merged["kind"] = kind
	merged["_item_type"] = kind[:len(kind)-1]
	return runDispatch(ctx, call.Env, merged, "spotify_library", "list", libraryTable, nil)
}
