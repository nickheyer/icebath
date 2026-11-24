package homeassistant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	websocketutil "github.com/nickheyer/icebath/pkg/utils/websocket"
)

const (
	toolset        = "homeassistant"
	defaultHassURL = "http://homeassistant.local:8123"
	toolEmoji      = "\U0001F3E0"
)

// Tool error envelope with non ASCII text kept
func toolError(message string) string { return tools.JSON(tools.Dict("error", message)) }

// Why a URL cannot be the Home Assistant base URL, empty when it can
func urlSchemeProblem(u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return ""
	}
	return fmt.Sprintf("Home Assistant URL %s has no http:// or https:// scheme. Set HASS_URL (or "+
		"platforms.homeassistant.extra.url) to the full URL, e.g. https://ha.example.com or "+
		"http://homeassistant.local:8123", u)
}

// get_secret of a name with the default for an unset value, the scope error while multiplexing without one
func getSecret(ctx context.Context, name, def string) (string, error) {
	v, ok, err := provider.GetSecret(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return def, nil
	}
	return v, nil
}

// Active profile URL without trailing slashes and stripped token, the ValueError of a URL without a scheme
func getConfig(ctx context.Context) (string, string, error) {
	rawURL, err := getSecret(ctx, "HASS_URL", defaultHassURL)
	if err != nil {
		return "", "", err
	}
	hassURL := strings.TrimRight(rawURL, "/")
	if problem := urlSchemeProblem(hassURL); problem != "" {
		return "", "", &pyErr{"ValueError", problem}
	}
	token, err := getSecret(ctx, "HASS_TOKEN", "")
	if err != nil {
		return "", "", err
	}
	return hassURL, strings.TrimSpace(token), nil
}

// Allow and deny patterns of the active profile
func getEntityFilterConfig(ctx context.Context) ([]string, []string, error) {
	allow, err := getSecret(ctx, "HASS_ENTITY_ALLOWLIST", "")
	if err != nil {
		return nil, nil, err
	}
	deny, err := getSecret(ctx, "HASS_ENTITY_DENYLIST", "")
	if err != nil {
		return nil, nil, err
	}
	return parseEntityFilterPatterns(allow), parseEntityFilterPatterns(deny), nil
}

// Positive HASS_MAX_ENTITIES cap of the active profile, 0 when unset, not an integer or not positive
func getMaxEntitiesConfig(ctx context.Context) (int64, error) {
	raw, err := getSecret(ctx, "HASS_MAX_ENTITIES", "")
	if err != nil {
		return 0, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, ok := parseIntStr(raw)
	if !ok {
		pkgLog().Debug(fmt.Sprintf("Ignoring non-integer HASS_MAX_ENTITIES value %s", strconv.Quote(raw)))
		return 0, nil
	}
	return max(value, 0), nil
}

// Trimmed non empty patterns of a comma separated value
func parseEntityFilterPatterns(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(value, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Whether an entity id matches a pattern exactly, as a glob, or as a prefix with dot and underscore alike
func entityMatches(entityID any, patterns []string) (bool, error) {
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		id, isStr := entityID.(string)
		if isStr && id == pat {
			return true, nil
		}
		if !isStr {
			return false, &pyErr{"TypeError", fmt.Sprintf("an entity id must be text, got %s", errchain.Kind(entityID))}
		}
		if fnmatch.Fnmatch(id, pat) {
			return true, nil
		}
		patRunes := []rune(pat)
		if strings.ContainsRune("*._", patRunes[len(patRunes)-1]) {
			base := strings.TrimRight(pat, "*._")
			idRunes, baseRunes := []rune(id), []rune(base)
			if base != "" && strings.HasPrefix(id, base) &&
				(len(idRunes) == len(baseRunes) || strings.ContainsRune("._", idRunes[len(baseRunes)])) {
				return true, nil
			}
		}
	}
	return false, nil
}

// States the allowlist keeps and the denylist does not veto, the input untouched when neither is set
func applyOperatorFilters(states any, allow, deny []string) (any, error) {
	if len(allow) == 0 && len(deny) == 0 {
		return states, nil
	}
	items, err := pyIter(states)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, s := range items {
		entityID, err := pyGet(s, "entity_id", "")
		if err != nil {
			return nil, err
		}
		if len(allow) > 0 {
			ok, err := entityMatches(entityID, allow)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if len(deny) > 0 {
			hit, err := entityMatches(entityID, deny)
			if err != nil {
				return nil, err
			}
			if hit {
				continue
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Reason operator config excludes one entity from reads, empty when it may be read
func operatorFilterViolation(ctx context.Context, entityID string) (string, error) {
	allow, deny, err := getEntityFilterConfig(ctx)
	if err != nil {
		return "", err
	}
	if len(deny) > 0 {
		if hit, _ := entityMatches(entityID, deny); hit {
			return "excluded by HASS_ENTITY_DENYLIST", nil
		}
	}
	if len(allow) > 0 {
		if ok, _ := entityMatches(entityID, allow); !ok {
			return "not in HASS_ENTITY_ALLOWLIST", nil
		}
	}
	return "", nil
}

// Target keys that select entities on the server where the operator lists cannot be checked
var indirectTargetKeys = []string{"area_id", "device_id", "floor_id", "label_id"}

// Domains whose services act on no entity
var nonEntityDomains = map[string]bool{"notify": true, "persistent_notification": true}

// Built in script services, every other script service running the script entity of its name
var scriptBuiltinServices = map[string]bool{"turn_on": true, "turn_off": true, "toggle": true, "reload": true}

// Whether a payload key names entities
func isEntityKey(key string) bool {
	return key == "entity_id" || key == "entities" || key == "snapshot_entities" ||
		strings.HasSuffix(key, "_entity_id") || strings.HasSuffix(key, "_entity_ids")
}

// Whether a value equals one of None, the empty str, the empty list or the empty dict
func emptyTargetValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case *jsonutil.Object:
		return t.Len() == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// Lowercased entity ids and indirect target keys a payload names, nested dicts searched
func collectServiceTargets(payload any, entityIDs, indirect map[string]bool) {
	if !isDict(payload) {
		return
	}
	for _, key := range dictKeys(payload) {
		value, _ := pyGet(payload, key, nil)
		if emptyTargetValue(value) {
			continue
		}
		switch {
		case containsString(indirectTargetKeys, key):
			indirect[key] = true
		case isEntityKey(key):
			ids := value
			if isDict(value) {
				keys := dictKeys(value)
				list := make([]any, len(keys))
				for i, k := range keys {
					list[i] = k
				}
				ids = list
			}
			for e := range normalizeIDList(ids) {
				entityIDs[strings.ToLower(e)] = true
			}
		case isDict(value):
			collectServiceTargets(value, entityIDs, indirect)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Sorted members of a string set
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Why the operator lists refuse a service call, empty when they allow it or are unset
func serviceCallViolation(ctx context.Context, domain, service string, payload any) (string, error) {
	allow, deny, err := getEntityFilterConfig(ctx)
	if err != nil {
		return "", err
	}
	if len(allow) == 0 && len(deny) == 0 {
		return "", nil
	}
	hint := "Call ha_call_service with explicit entity_id values that the operator " +
		"allow/deny lists permit, or ask the operator to adjust the lists."
	entityIDs := map[string]bool{}
	indirect := map[string]bool{}
	collectServiceTargets(payload, entityIDs, indirect)
	if len(indirect) > 0 {
		return fmt.Sprintf("%s.%s targets by %s, which cannot be checked against HASS_ENTITY_ALLOWLIST/HASS_ENTITY_DENYLIST. %s",
			domain, service, strings.Join(sortedSet(indirect), ", "), hint), nil
	}
	if entityIDs["all"] {
		return fmt.Sprintf("%s.%s with entity_id 'all' would act on every entity, which "+
			"HASS_ENTITY_ALLOWLIST/HASS_ENTITY_DENYLIST do not permit. %s", domain, service, hint), nil
	}
	delete(entityIDs, "none")
	if len(entityIDs) == 0 {
		if nonEntityDomains[domain] {
			return "", nil
		}
		if domain == "script" && !scriptBuiltinServices[service] {
			entityIDs = map[string]bool{"script." + service: true}
		} else {
			return fmt.Sprintf("%s.%s names no entity, so it could act on every %s "+
				"entity while HASS_ENTITY_ALLOWLIST/HASS_ENTITY_DENYLIST are set. %s", domain, service, domain, hint), nil
		}
	}
	for _, eid := range sortedSet(entityIDs) {
		if len(deny) > 0 {
			for _, p := range deny {
				if hit, _ := entityMatches(eid, []string{p}); hit {
					return fmt.Sprintf("%s is excluded by HASS_ENTITY_DENYLIST (pattern %s); %s.%s refused. %s",
						eid, p, domain, service, hint), nil
				}
			}
		}
		if len(allow) > 0 {
			if ok, _ := entityMatches(eid, allow); !ok {
				return fmt.Sprintf("%s is not in HASS_ENTITY_ALLOWLIST; %s.%s refused. %s", eid, domain, service, hint), nil
			}
		}
	}
	return "", nil
}

// Deduplicated non empty ids of a list or a comma and whitespace delimited str
func normalizeIDList(value any) map[string]bool {
	out := map[string]bool{}
	var parts []string
	switch t := value.(type) {
	case nil:
		return out
	case string:
		parts = strings.FieldsFunc(strings.TrimSpace(t), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	case []any:
		for _, p := range t {
			parts = append(parts, strings.TrimSpace(anyutil.String(p)))
		}
	default:
		parts = []string{strings.TrimSpace(anyutil.String(value))}
	}
	for _, p := range parts {
		if p != "" {
			out[p] = true
		}
	}
	return out
}

var (
	// Valid entity id such as light.living_room
	entityIDRE = regexutil.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z0-9_]+$`)
	// Domain and service names interpolated into the services path, so only lowercase word characters
	serviceNameRE = regexutil.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Domains that run code or reach the network from the Home Assistant host
var blockedDomains = []string{"command_line", "hassio", "pyscript", "python_script", "rest_command", "shell_command"}

// Whether a pattern matches a tool argument, panicking with a TypeError for an argument that is not text
func reMatch(re interface{ MatchString(string) (bool, error) }, v any) bool {
	s, ok := v.(string)
	if !ok {
		panic(&pyErr{"TypeError", fmt.Sprintf("expected text, got %s", errchain.Kind(v))})
	}
	matched, err := re.MatchString(s)
	if err != nil {
		panic(err)
	}
	return matched
}

// Authorization and JSON content headers, the configured token when none is given
func getHeaders(ctx context.Context, token string) (map[string]string, error) {
	if token == "" {
		_, t, err := getConfig(ctx)
		if err != nil {
			return nil, err
		}
		token = t
	}
	return map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}, nil
}

// One REST call returning the JSON body, the ClientResponseError of an HTTP error carrying the body text
func apiJSON(ctx context.Context, method, path string, timeout time.Duration, payload any) (any, error) {
	hassURL, hassToken, err := getConfig(ctx)
	if err != nil {
		return nil, err
	}
	headers, err := getHeaders(ctx, hassToken)
	if err != nil {
		return nil, err
	}
	client := newSession(false)
	defer client.CloseIdleConnections()
	var resp *httpResponse
	if method == http.MethodPost {
		resp, err = postJSON(ctx, client, hassURL+path, headers, payload, timeout)
	} else {
		resp, err = doRequest(ctx, client, method, hassURL+path, headers, nil, timeout)
	}
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		message := textutil.First(resp.text(), 500)
		if message == "" {
			message = resp.reason
		}
		return nil, &responseError{status: resp.status, message: message, url: resp.url}
	}
	return resp.json()
}

// Lower case of a text argument, an AttributeError for any other value
func pyLower(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", &pyErr{"AttributeError", fmt.Sprintf("expected text, got %s", errchain.Kind(v))}
	}
	return strings.ToLower(s), nil
}

// Lower case of a value, empty when the value is empty
func lowerOrEmpty(v any) (string, error) {
	if !anyutil.IsTrue(v) {
		return "", nil
	}
	return pyLower(v)
}

// Friendly name of a state's attributes, empty when absent
func friendlyName(s any) (any, error) {
	attrs, err := pyGet(s, "attributes", jsonutil.NewObject())
	if err != nil {
		return nil, err
	}
	return pyGet(attrs, "friendly_name", "")
}

// Whether a state belongs to the area by its registry area, else by friendly name or area attribute
func inArea(s any, areaLower string, entityArea keyedMap) (bool, error) {
	eid, err := pyGet(s, "entity_id", "")
	if err != nil {
		return false, err
	}
	registered, found, err := entityArea.get(eid)
	if err != nil {
		return false, err
	}
	if found && registered != nil {
		lowered, err := pyLower(registered)
		if err != nil {
			return false, err
		}
		return strings.Contains(lowered, areaLower), nil
	}
	attrs, err := pyGet(s, "attributes", jsonutil.NewObject())
	if err != nil {
		return false, err
	}
	name, err := pyGet(attrs, "friendly_name", "")
	if err != nil {
		return false, err
	}
	nameLower, err := lowerOrEmpty(name)
	if err != nil {
		return false, err
	}
	if strings.Contains(nameLower, areaLower) {
		return true, nil
	}
	area, err := pyGet(attrs, "area", "")
	if err != nil {
		return false, err
	}
	areaAttr, err := lowerOrEmpty(area)
	if err != nil {
		return false, err
	}
	return strings.Contains(areaAttr, areaLower), nil
}

// States kept by a predicate in order, the first predicate error raised
func filterStates(states []any, keep func(s any) (bool, error)) ([]any, error) {
	out := []any{}
	for _, s := range states {
		ok, err := keep(s)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// Operator lists, caller filters and the entity cap applied to raw states, then the compact payload
func filterAndSummarize(ctx context.Context, states any, domain, area, entityIDs, name, maxEntities any,
	entityArea, entityDevice keyedMap) (*jsonutil.Object, error) {
	if entityArea == nil {
		entityArea = keyedMap{}
	}
	if entityDevice == nil {
		entityDevice = keyedMap{}
	}
	allow, deny, err := getEntityFilterConfig(ctx)
	if err != nil {
		return nil, err
	}
	filtered, err := applyOperatorFilters(states, allow, deny)
	if err != nil {
		return nil, err
	}
	idFilter := normalizeIDList(entityIDs)
	var list []any
	listed := false
	// Comprehension over the current states, turning the raw value into a list on first use
	comprehend := func(keep func(s any) (bool, error)) error {
		if !listed {
			items, err := pyIter(filtered)
			if err != nil {
				return err
			}
			list, listed = items, true
		}
		kept, err := filterStates(list, keep)
		if err != nil {
			return err
		}
		list = kept
		return nil
	}
	if len(idFilter) > 0 {
		if err := comprehend(func(s any) (bool, error) {
			eid, err := pyGet(s, "entity_id", "")
			if err != nil {
				return false, err
			}
			if _, err := hashKeyAs(eid, "set member"); err != nil {
				return false, err
			}
			id, ok := eid.(string)
			return ok && idFilter[id], nil
		}); err != nil {
			return nil, err
		}
	} else {
		if anyutil.IsTrue(domain) {
			prefix := anyutil.String(domain) + "."
			if err := comprehend(func(s any) (bool, error) {
				eid, err := pyGet(s, "entity_id", "")
				if err != nil {
					return false, err
				}
				id, ok := eid.(string)
				if !ok {
					return false, attrErr(eid, "an entity id", "text")
				}
				return strings.HasPrefix(id, prefix), nil
			}); err != nil {
				return nil, err
			}
		}
		if anyutil.IsTrue(area) {
			areaLower, err := pyLower(area)
			if err != nil {
				return nil, err
			}
			if err := comprehend(func(s any) (bool, error) { return inArea(s, areaLower, entityArea) }); err != nil {
				return nil, err
			}
		}
		if anyutil.IsTrue(name) {
			nameLower, err := pyLower(name)
			if err != nil {
				return nil, err
			}
			if err := comprehend(func(s any) (bool, error) {
				fn, err := friendlyName(s)
				if err != nil {
					return false, err
				}
				fnLower, err := lowerOrEmpty(fn)
				if err != nil {
					return false, err
				}
				if strings.Contains(fnLower, nameLower) {
					return true, nil
				}
				eid, err := pyGet(s, "entity_id", "")
				if err != nil {
					return false, err
				}
				eidLower, err := lowerOrEmpty(eid)
				if err != nil {
					return false, err
				}
				return strings.Contains(eidLower, nameLower), nil
			}); err != nil {
				return nil, err
			}
		}
	}
	var limit int64
	if n, ok := anyutil.Int(maxEntities); ok && n > 0 {
		limit = n
	} else if limit, err = getMaxEntitiesConfig(ctx); err != nil {
		return nil, err
	}
	if !listed {
		items, err := pyIter(filtered)
		if err != nil {
			return nil, &pyErr{"TypeError", fmt.Sprintf("the entity filter must give a list, got %s", errchain.Kind(filtered))}
		}
		list = items
	}
	total := len(list)
	truncated := limit > 0 && int64(total) > limit
	if truncated {
		if !listed && isDict(filtered) {
			return nil, &pyErr{"KeyError", fmt.Sprintf("the entity filter gave a mapping, which cannot be cut to its first %d entities", limit)}
		}
		list = list[:limit]
	}
	entities := []any{}
	for _, s := range list {
		eid, err := pyIndex(s, "entity_id")
		if err != nil {
			return nil, err
		}
		state, err := pyIndex(s, "state")
		if err != nil {
			return nil, err
		}
		fn, err := friendlyName(s)
		if err != nil {
			return nil, err
		}
		item := jsonutil.ObjectOf("entity_id", eid, "state", state, "friendly_name", fn)
		if v, ok, err := entityArea.get(eid); err != nil {
			return nil, err
		} else if ok {
			item.Set("area", v)
		}
		if v, ok, err := entityDevice.get(eid); err != nil {
			return nil, err
		} else if ok {
			item.Set("device", v)
		}
		entities = append(entities, item)
	}
	result := jsonutil.ObjectOf("count", len(entities), "entities", entities)
	if truncated {
		result.Set("truncated", true)
		result.Set("truncated_note", fmt.Sprintf("Showing first %d of %d matching entities. "+
			"Refine with domain/area/name filters or an explicit entity_ids "+
			"list, or pass a larger max.", limit, total))
	}
	return result, nil
}

const (
	registryTTL        = 300.0
	registryFailureTTL = 60.0
	registryTimeout    = 20 * time.Second
	registryMaxMsg     = 64 * 1024 * 1024
)

// Area and device maps of the last registry pull keyed by the instance URL it came from
var registryCache = struct {
	sync.Mutex
	hasURL       bool
	url          string
	ts           float64
	failedAt     float64
	entityArea   keyedMap
	entityDevice keyedMap
}{entityArea: keyedMap{}, entityDevice: keyedMap{}}

// Seconds since the epoch with the fraction kept
func epochSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// WebSocket URL of the instance base URL
func wsBaseURL(hassURL string) string {
	return strings.ReplaceAll(strings.ReplaceAll(hassURL, "https://", "wss://"), "http://", "ws://")
}

// Message received where a text message was expected
type wsMessageTypeError struct{ msg websocketutil.Message }

func (e *wsMessageTypeError) Error() string {
	return fmt.Sprintf("received a %s message of %d bytes where text was expected", e.msg.Type, len(e.msg.Data))
}

// Next message of a websocket decoded as JSON, a wsMessageTypeError for a binary message and the decode error of bad JSON
func receiveJSON(ctx context.Context, conn *websocketutil.Conn) (any, error) {
	msg, err := conn.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if msg.Type != websocketutil.Text {
		return nil, &wsMessageTypeError{msg: msg}
	}
	return jsonutil.Decode(msg.Data)
}

// Sends a value as a JSON text message
func sendJSON(conn *websocketutil.Conn, v any) error {
	text, err := jsonutil.Encode(v, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return err
	}
	return conn.SendText(text)
}

// Result list of one registry request, the RuntimeError of a refused request
func registryFetch(ctx context.Context, conn *websocketutil.Conn, mid int64, mtype string) (any, error) {
	if err := sendJSON(conn, jsonutil.ObjectOf("id", mid, "type", mtype)); err != nil {
		return nil, err
	}
	for {
		m, err := receiveJSON(ctx, conn)
		if err != nil {
			return nil, err
		}
		id, err := pyGet(m, "id", nil)
		if err != nil {
			return nil, err
		}
		if !anyutil.Equal(id, mid) {
			continue
		}
		kind, err := pyGet(m, "type", nil)
		if err != nil {
			return nil, err
		}
		if kind != "result" {
			continue
		}
		success, _ := pyGet(m, "success", true)
		if !anyutil.IsTrue(success) {
			detail, _ := pyGet(m, "error", nil)
			return nil, &pyErr{"RuntimeError", fmt.Sprintf("%s refused: %s", mtype, anyutil.String(detail))}
		}
		result, _ := pyGet(m, "result", nil)
		if !anyutil.IsTrue(result) {
			return []any{}, nil
		}
		return result, nil
	}
}

// Authenticates and reads the area, device and entity registries, refused true when the token was rejected
func pullRegistry(ctx context.Context, wsURL, token string) (entArea, entDev keyedMap, refused bool, err error) {
	conn, err := websocketutil.Connect(ctx, wsURL, websocketutil.Options{PingInterval: -1, MaxMessageSize: registryMaxMsg})
	if err != nil {
		return nil, nil, false, err
	}
	defer func() {
		conn.CloseWith(ctx, websocketutil.CloseNormal, "")
		if err == nil && ctx.Err() != nil {
			entArea, entDev, refused, err = nil, nil, false, ctx.Err()
		}
	}()
	if _, err := receiveJSON(ctx, conn); err != nil {
		return nil, nil, false, err
	}
	if err := sendJSON(conn, jsonutil.ObjectOf("type", "auth", "access_token", token)); err != nil {
		return nil, nil, false, err
	}
	authReply, err := receiveJSON(ctx, conn)
	if err != nil {
		return nil, nil, false, err
	}
	authType, err := pyGet(authReply, "type", nil)
	if err != nil {
		return nil, nil, false, err
	}
	if authType != "auth_ok" {
		reason, _ := pyGet(authReply, "message", nil)
		if !anyutil.IsTrue(reason) {
			reason = authType
		}
		pkgLog().Warn(fmt.Sprintf("HA registry websocket auth failed (%s); area filter degraded", anyutil.String(reason)))
		return nil, nil, true, nil
	}
	areaReg, err := registryFetch(ctx, conn, 1, "config/area_registry/list")
	if err != nil {
		return nil, nil, false, err
	}
	areas := keyedMap{}
	areaItems, err := pyIter(areaReg)
	if err != nil {
		return nil, nil, false, err
	}
	for _, a := range areaItems {
		k, err := pyIndex(a, "area_id")
		if err != nil {
			return nil, nil, false, err
		}
		v, err := pyIndex(a, "name")
		if err != nil {
			return nil, nil, false, err
		}
		if err := areas.set(k, v); err != nil {
			return nil, nil, false, err
		}
	}
	deviceReg, err := registryFetch(ctx, conn, 2, "config/device_registry/list")
	if err != nil {
		return nil, nil, false, err
	}
	devices := keyedMap{}
	deviceItems, err := pyIter(deviceReg)
	if err != nil {
		return nil, nil, false, err
	}
	for _, d := range deviceItems {
		k, err := pyIndex(d, "id")
		if err != nil {
			return nil, nil, false, err
		}
		if err := devices.set(k, d); err != nil {
			return nil, nil, false, err
		}
	}
	entityReg, err := registryFetch(ctx, conn, 3, "config/entity_registry/list")
	if err != nil {
		return nil, nil, false, err
	}
	entityItems, err := pyIter(entityReg)
	if err != nil {
		return nil, nil, false, err
	}
	entArea, entDev = keyedMap{}, keyedMap{}
	for _, e := range entityItems {
		deviceID, err := pyGet(e, "device_id", nil)
		if err != nil {
			return nil, nil, false, err
		}
		dev, _, err := devices.get(deviceID)
		if err != nil {
			return nil, nil, false, err
		}
		if !anyutil.IsTrue(dev) {
			dev = jsonutil.NewObject()
		}
		aid, err := pyGet(e, "area_id", nil)
		if err != nil {
			return nil, nil, false, err
		}
		if !anyutil.IsTrue(aid) {
			if aid, err = pyGet(dev, "area_id", nil); err != nil {
				return nil, nil, false, err
			}
		}
		areaName, known, err := areas.get(aid)
		if err != nil {
			return nil, nil, false, err
		}
		if known {
			eid, err := pyIndex(e, "entity_id")
			if err != nil {
				return nil, nil, false, err
			}
			if err := entArea.set(eid, areaName); err != nil {
				return nil, nil, false, err
			}
		}
		dname, err := pyGet(dev, "name_by_user", nil)
		if err != nil {
			return nil, nil, false, err
		}
		if !anyutil.IsTrue(dname) {
			if dname, err = pyGet(dev, "name", nil); err != nil {
				return nil, nil, false, err
			}
		}
		if anyutil.IsTrue(dname) {
			eid, err := pyIndex(e, "entity_id")
			if err != nil {
				return nil, nil, false, err
			}
			if err := entDev.set(eid, dname); err != nil {
				return nil, nil, false, err
			}
		}
	}
	return entArea, entDev, false, nil
}

// Entity area and device maps from the WebSocket registries, cached per instance with a failure backoff
func registryAreaMap(ctx context.Context) (keyedMap, keyedMap, error) {
	hassURL, hassToken, err := getConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	registryCache.Lock()
	if registryCache.hasURL && registryCache.url == hassURL {
		now := epochSeconds()
		if now-registryCache.failedAt < registryFailureTTL {
			registryCache.Unlock()
			return keyedMap{}, keyedMap{}, nil
		}
		if registryCache.ts != 0 && now-registryCache.ts < registryTTL {
			area, device := registryCache.entityArea, registryCache.entityDevice
			registryCache.Unlock()
			return area, device, nil
		}
	}
	registryCache.Unlock()
	pullCtx, cancel := context.WithTimeout(ctx, registryTimeout)
	entArea, entDev, refused, err := pullRegistry(pullCtx, wsBaseURL(hassURL)+"/api/websocket", hassToken)
	cancel()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	timedOut := errors.Is(err, context.DeadlineExceeded)
	pulled := err == nil && !refused && !timedOut
	switch {
	case timedOut:
		pkgLog().Warn(fmt.Sprintf("HA registry lookup timed out after %s; area filter degraded", registryTimeout))
	case err != nil:
		pkgLog().Warn(fmt.Sprintf("HA registry lookup failed (%v); area filter degraded", err))
	}
	registryCache.Lock()
	defer registryCache.Unlock()
	registryCache.hasURL, registryCache.url = true, hassURL
	if !pulled {
		registryCache.ts, registryCache.failedAt = 0, epochSeconds()
		registryCache.entityArea, registryCache.entityDevice = keyedMap{}, keyedMap{}
		return keyedMap{}, keyedMap{}, nil
	}
	registryCache.ts, registryCache.failedAt = epochSeconds(), 0
	registryCache.entityArea, registryCache.entityDevice = entArea, entDev
	return entArea, entDev, nil
}

// Cached area and device maps of this instance while fresh, without a network call
func warmRegistryMaps(ctx context.Context) (keyedMap, keyedMap, error) {
	hassURL, _, err := getConfig(ctx)
	if err != nil {
		var pe *pyErr
		if errors.As(err, &pe) && pe.kind == "ValueError" {
			return keyedMap{}, keyedMap{}, nil
		}
		return nil, nil, err
	}
	registryCache.Lock()
	defer registryCache.Unlock()
	if registryCache.hasURL && registryCache.url == hassURL && registryCache.ts != 0 && epochSeconds()-registryCache.ts < registryTTL {
		return registryCache.entityArea, registryCache.entityDevice, nil
	}
	return keyedMap{}, keyedMap{}, nil
}

// Entity states with every filter applied before the payload is built, the registry read only for an area query
func listEntities(ctx context.Context, domain, area, entityIDs, name, maxEntities any) (any, error) {
	states, err := apiJSON(ctx, http.MethodGet, "/api/states", 15*time.Second, nil)
	if err != nil {
		return nil, err
	}
	var entityArea, entityDevice keyedMap
	if anyutil.IsTrue(area) {
		entityArea, entityDevice, err = registryAreaMap(ctx)
	} else {
		entityArea, entityDevice, err = warmRegistryMaps(ctx)
	}
	if err != nil {
		return nil, err
	}
	return filterAndSummarize(ctx, states, domain, area, entityIDs, name, maxEntities, entityArea, entityDevice)
}

// Full state of one entity with its attributes and change times
func getState(ctx context.Context, entityID string) (any, error) {
	data, err := apiJSON(ctx, http.MethodGet, "/api/states/"+entityID, 10*time.Second, nil)
	if err != nil {
		return nil, err
	}
	eid, err := pyIndex(data, "entity_id")
	if err != nil {
		return nil, err
	}
	state, err := pyIndex(data, "state")
	if err != nil {
		return nil, err
	}
	attrs, err := pyGet(data, "attributes", jsonutil.NewObject())
	if err != nil {
		return nil, err
	}
	lastChanged, _ := pyGet(data, "last_changed", nil)
	lastUpdated, _ := pyGet(data, "last_updated", nil)
	return jsonutil.ObjectOf("entity_id", eid, "state", state, "attributes", attrs, "last_changed", lastChanged,
		"last_updated", lastUpdated), nil
}

// Service call payload of the data with the entity id argument overriding data entity_id
func buildServicePayload(entityID, data any) (*jsonutil.Object, error) {
	payload, err := pyDict(data)
	if err != nil {
		return nil, err
	}
	if anyutil.IsTrue(entityID) {
		payload.Set("entity_id", entityID)
	}
	return payload, nil
}

// Compact service response of either the bare changed states list or the return_response shape
func parseServiceResponse(domain, service string, result, entityID any) (*jsonutil.Object, error) {
	affected := []any{}
	var serviceResponse any
	compact := func(s any) {
		eid, _ := pyGet(s, "entity_id", "")
		state, _ := pyGet(s, "state", "")
		affected = append(affected, jsonutil.ObjectOf("entity_id", eid, "state", state))
	}
	switch {
	case isDict(result):
		changed, _ := pyGet(result, "changed_states", nil)
		if !anyutil.IsTrue(changed) {
			changed = []any{}
		}
		items, err := pyIter(changed)
		if err != nil {
			return nil, err
		}
		for _, s := range items {
			if isDict(s) {
				compact(s)
			}
		}
		serviceResponse, _ = pyGet(result, "service_response", nil)
	default:
		if list, ok := result.([]any); ok {
			for _, s := range list {
				if isDict(s) {
					compact(s)
				}
			}
		}
	}
	out := jsonutil.ObjectOf("success", true, "service", domain+"."+service, "affected_entities", affected)
	if serviceResponse != nil {
		out.Set("service_response", serviceResponse)
	}
	if anyutil.IsTrue(entityID) && len(affected) == 0 && serviceResponse == nil {
		out.Set("success", false)
		out.Set("error", fmt.Sprintf("%s.%s affected no entities for %s", domain, service, anyutil.String(entityID)))
	}
	return out, nil
}

// Calls a service, retrying with return_response when required and explaining an empty result
func callService(ctx context.Context, domain, service string, entityID, data any) (any, error) {
	payload, err := buildServicePayload(entityID, data)
	if err != nil {
		return nil, err
	}
	path := "/api/services/" + domain + "/" + service
	result, err := apiJSON(ctx, http.MethodPost, path, 15*time.Second, payload)
	if err != nil {
		if !strings.Contains(err.Error(), "requires responses") {
			return nil, err
		}
		if result, err = apiJSON(ctx, http.MethodPost, path+"?return_response", 15*time.Second, payload); err != nil {
			return nil, err
		}
	}
	out, err := parseServiceResponse(domain, service, result, entityID)
	if err != nil {
		return nil, err
	}
	if success, _ := out.Get("success"); success != false || !anyutil.IsTrue(entityID) {
		return out, nil
	}
	eid := anyutil.String(entityID)
	errText, _ := out.Get("error")
	state, err := apiJSON(ctx, http.MethodGet, "/api/states/"+eid, 10*time.Second, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var re *responseError
		switch {
		case errors.As(err, &re) && re.status == 404:
			out.Set("error", fmt.Sprintf("%s.%s affected no entities: %s does not exist in "+
				"Home Assistant. Check the exact entity_id with ha_list_entities.", domain, service, eid))
		case errors.As(err, &re):
			out.Set("error", fmt.Sprintf("%s (could not verify %s: HTTP %d)", anyutil.String(errText), eid, re.status))
		default:
			out.Set("error", fmt.Sprintf("%s (could not verify %s: %s)", anyutil.String(errText), eid, err.Error()))
		}
		return out, nil
	}
	stateID, err := pyIndex(state, "entity_id")
	if err != nil {
		return nil, err
	}
	stateIDText, ok := stateID.(string)
	if !ok {
		return nil, attrErr(stateID, "the state id", "text")
	}
	actualDomain, _, _ := strings.Cut(stateIDText, ".")
	if actualDomain != domain && domain != "homeassistant" {
		out.Set("error", fmt.Sprintf("%s.%s affected no entities: %s exists but is in the "+
			"'%s' domain, not '%s'. Service calls only affect entities "+
			"of their own domain — call %s.%s instead.", domain, service, eid, actualDomain, domain, actualDomain, service))
		return out, nil
	}
	current, err := pyIndex(state, "state")
	if err != nil {
		return nil, err
	}
	out.Set("success", true)
	out.Delete("error")
	out.Set("note", fmt.Sprintf("%s was already in state '%s' — nothing to change.", eid, anyutil.String(current)))
	return out, nil
}

// Available services, optionally of one domain, compacted to descriptions and field descriptions
func listServices(ctx context.Context, domain any) (any, error) {
	services, err := apiJSON(ctx, http.MethodGet, "/api/services", 15*time.Second, nil)
	if err != nil {
		return nil, err
	}
	items, err := pyIter(services)
	if err != nil {
		return nil, err
	}
	if anyutil.IsTrue(domain) {
		if items, err = filterStates(items, func(s any) (bool, error) {
			d, err := pyGet(s, "domain", nil)
			return err == nil && anyutil.Equal(d, domain), err
		}); err != nil {
			return nil, err
		}
	}
	result := []any{}
	for _, svcDomain := range items {
		svcs, err := pyGet(svcDomain, "services", jsonutil.NewObject())
		if err != nil {
			return nil, err
		}
		if !isDict(svcs) {
			return nil, attrErr(svcs, "the services of a domain", "a mapping")
		}
		domainServices := jsonutil.NewObject()
		for _, svcName := range dictKeys(svcs) {
			svcInfo, _ := pyGet(svcs, svcName, nil)
			description, err := pyGet(svcInfo, "description", "")
			if err != nil {
				return nil, err
			}
			entry := jsonutil.ObjectOf("description", description)
			fields, _ := pyGet(svcInfo, "fields", jsonutil.NewObject())
			if anyutil.IsTrue(fields) {
				if !isDict(fields) {
					return nil, attrErr(fields, "the fields of a service", "a mapping")
				}
				described := jsonutil.NewObject()
				for _, k := range dictKeys(fields) {
					v, _ := pyGet(fields, k, nil)
					if isDict(v) {
						d, _ := pyGet(v, "description", "")
						described.Set(k, d)
					}
				}
				entry.Set("fields", described)
			}
			domainServices.Set(svcName, entry)
		}
		d, _ := pyGet(svcDomain, "domain", "")
		result = append(result, jsonutil.ObjectOf("domain", d, "services", domainServices))
	}
	return jsonutil.ObjectOf("count", len(result), "domains", result), nil
}

// Runs a tool body and wraps its value as result, logging an error and returning the tool error envelope
func dispatch(run func() (any, error), logName, failMsg string) string {
	value, err := run()
	if err != nil {
		pkgLog().Error(fmt.Sprintf("%s error: %s", logName, err.Error()))
		return toolError(failMsg + ": " + err.Error())
	}
	return tools.JSONASCII(tools.Dict("result", value))
}

// Lists entities, a text max converted to an integer and refused when it is not one
func handleListEntities(ctx context.Context, call *tools.Call) string {
	args := tools.PythonCallArgs(call)
	maxEntities := args["max"]
	if s, ok := maxEntities.(string); ok {
		n, parsed := parseIntStr(s)
		if !parsed {
			return toolError(fmt.Sprintf("Invalid max value: %s (expected a positive integer)", s))
		}
		maxEntities = n
	}
	return dispatch(func() (any, error) {
		return listEntities(ctx, args["domain"], args["area"], args["entity_ids"], args["name"], maxEntities)
	}, "ha_list_entities", "Failed to list entities")
}

// Value of an argument, the default for a missing key
func argOr(args map[string]any, key string, def any) any {
	if v, ok := args[key]; ok {
		return v
	}
	return def
}

// Reads one entity after the format check and the operator lists, refusing before any network call
func handleGetState(ctx context.Context, call *tools.Call) string {
	args := tools.PythonCallArgs(call)
	entityID := argOr(args, "entity_id", "")
	if !anyutil.IsTrue(entityID) {
		return toolError("Missing required parameter: entity_id")
	}
	if !reMatch(entityIDRE, entityID) {
		return toolError("Invalid entity_id format: " + anyutil.String(entityID))
	}
	id := entityID.(string)
	violation, err := operatorFilterViolation(ctx, id)
	if err != nil {
		panic(err)
	}
	if violation != "" {
		return toolError(fmt.Sprintf("%s is excluded by operator config (%s). "+
			"Use ha_list_entities with a filter to discover an alternative "+
			"entity, or ask the operator to adjust the allow/deny lists.", id, violation))
	}
	return dispatch(func() (any, error) { return getState(ctx, id) }, "ha_get_state", "Failed to get state for "+id)
}

// Calls a service after the format checks, the domain blocklist and the operator lists
func handleCallService(ctx context.Context, call *tools.Call) string {
	args := tools.PythonCallArgs(call)
	domainArg, serviceArg := argOr(args, "domain", ""), argOr(args, "service", "")
	if !anyutil.IsTrue(domainArg) || !anyutil.IsTrue(serviceArg) {
		return toolError("Missing required parameters: domain and service")
	}
	if !reMatch(serviceNameRE, domainArg) {
		return toolError("Invalid domain format: " + errchain.Show(domainArg))
	}
	if !reMatch(serviceNameRE, serviceArg) {
		return toolError("Invalid service format: " + errchain.Show(serviceArg))
	}
	domain, service := domainArg.(string), serviceArg.(string)
	if containsString(blockedDomains, domain) {
		return toolError(fmt.Sprintf("Service domain '%s' is blocked for security. Blocked domains: %s",
			domain, strings.Join(blockedDomains, ", ")))
	}
	entityID := args["entity_id"]
	if anyutil.IsTrue(entityID) && !reMatch(entityIDRE, entityID) {
		return toolError("Invalid entity_id format: " + anyutil.String(entityID))
	}
	data := args["data"]
	if s, ok := data.(string); ok {
		if strings.TrimSpace(s) == "" {
			data = nil
		} else {
			parsed, err := jsonutil.Decode(s)
			if err != nil {
				return toolError("Invalid JSON string in 'data' parameter: " + err.Error())
			}
			data = parsed
		}
	}
	allow, deny, err := getEntityFilterConfig(ctx)
	if err != nil {
		panic(err)
	}
	if len(allow) > 0 || len(deny) > 0 {
		if data != nil && !isDict(data) {
			return toolError("Invalid data parameter: expected a JSON object, got " + errchain.Kind(data))
		}
		payload, err := buildServicePayload(entityID, data)
		if err != nil {
			panic(err)
		}
		violation, err := serviceCallViolation(ctx, domain, service, payload)
		if err != nil {
			panic(err)
		}
		if violation != "" {
			return toolError("Refused by operator config: " + violation)
		}
	}
	return dispatch(func() (any, error) { return callService(ctx, domain, service, entityID, data) },
		"ha_call_service", fmt.Sprintf("Failed to call %s.%s", domain, service))
}

// Lists services of every domain or of the given one
func handleListServices(ctx context.Context, call *tools.Call) string {
	args := tools.PythonCallArgs(call)
	return dispatch(func() (any, error) { return listServices(ctx, args["domain"]) }, "ha_list_services", "Failed to list services")
}

// HASS_TOKEN through the profile scope without trimming, a multiplexed miss never borrowing the process env
func scopedSecretRaw(env *tools.Env, name string) string {
	if tools.MultiplexActiveFor(context.Background(), env) && !tools.IsGlobalEnv(name) {
		if env == nil || env.Secrets == nil {
			return ""
		}
		return env.Secrets.File()[name]
	}
	if env != nil && env.Secrets != nil {
		return env.Secrets.String(name)
	}
	return os.Getenv(name)
}

// Tools are available only when HASS_TOKEN is set
func checkHAAvailable(env *tools.Env) bool { return scopedSecretRaw(env, "HASS_TOKEN") != "" }

var haListEntitiesSchema = jsonutil.ObjectOf(
	"name", "ha_list_entities",
	"description", "List Home Assistant entities. On a large install a bare call can return "+
		"thousands of entities, so use the filters (domain, area, name, "+
		"entity_ids) to keep the response small and targeted. Each entity "+
		"includes its area and device when known (always after an area query).\n"+
		"For a request about a room (\"the kitchen lights\"), filter by `area`, "+
		"not `name`: a device belongs to a room by its Home Assistant area, and "+
		"its name often does not mention the room. Take everything the area "+
		"filter returns as part of that room, and judge what an entity is by "+
		"the domain prefix of its entity_id (every light.* is a light), not by "+
		"its name.\n"+
		"An operator may configure a global cap (HASS_MAX_ENTITIES); when that "+
		"or a 'max' parameter limits a result, the response is marked "+
		"'truncated' with a note on how to narrow the query or get more.",
	"parameters", jsonutil.ObjectOf(
		"type", "object",
		"properties", jsonutil.ObjectOf(
			"domain", jsonutil.ObjectOf(
				"type", "string",
				"description", "Entity domain to filter by (e.g. 'light', 'switch', 'climate', "+
					"'sensor', 'binary_sensor', 'cover', 'fan', 'media_player'). "+
					"The cheapest filter; combine with area or name for a "+
					"targeted query. Omit to list all entities.",
			),
			"area", jsonutil.ObjectOf(
				"type", "string",
				"description", "Area/room name to filter by (e.g. 'living room', 'kitchen'). "+
					"Use this, not name, for any request about a room. "+
					"Resolved against the Home Assistant area registry, so an "+
					"entity is matched even when its name does not mention the "+
					"room; falls back to matching friendly names when the "+
					"registry is unreachable. Omit to list all.",
			),
			"name", jsonutil.ObjectOf(
				"type", "string",
				"description", "Substring to match against entity friendly names and "+
					"entity_ids (e.g. 'thermostat', 'temperature'). "+
					"Useful when you don't know the domain. Not for rooms: "+
					"use area, because names rarely mention the room.",
			),
			"entity_ids", jsonutil.ObjectOf(
				"type", "array",
				"items", jsonutil.ObjectOf("type", "string"),
				"description", "Exact list of entity_ids to fetch (e.g. "+
					"['climate.office', 'sensor.office_temp']). Returns just "+
					"these entities, ignoring the domain/area/name filters.",
			),
			"max", jsonutil.ObjectOf(
				"type", "integer",
				"description", "Maximum number of entities to return for this call. "+
					"Overrides any operator-configured cap (HASS_MAX_ENTITIES) "+
					"for this one call; set higher if you genuinely need a "+
					"larger batch and the query is already well filtered.",
			),
		),
		"required", []any{},
	),
)

var haGetStateSchema = jsonutil.ObjectOf(
	"name", "ha_get_state",
	"description", "Get the detailed state of a single Home Assistant entity, including all "+
		"attributes (brightness, color, temperature setpoint, sensor readings, etc.).",
	"parameters", jsonutil.ObjectOf(
		"type", "object",
		"properties", jsonutil.ObjectOf(
			"entity_id", jsonutil.ObjectOf(
				"type", "string",
				"description", "The entity ID to query (e.g. 'light.living_room', "+
					"'climate.thermostat', 'sensor.temperature').",
			),
		),
		"required", []any{"entity_id"},
	),
)

var haListServicesSchema = jsonutil.ObjectOf(
	"name", "ha_list_services",
	"description", "List available Home Assistant services (actions) for device control. "+
		"Shows what actions can be performed on each device type and what "+
		"parameters they accept. Use this to discover how to control devices "+
		"found via ha_list_entities.",
	"parameters", jsonutil.ObjectOf(
		"type", "object",
		"properties", jsonutil.ObjectOf(
			"domain", jsonutil.ObjectOf(
				"type", "string",
				"description", "Filter by domain (e.g. 'light', 'climate', 'switch'). "+
					"Omit to list services for all domains.",
			),
		),
		"required", []any{},
	),
)

var haCallServiceSchema = jsonutil.ObjectOf(
	"name", "ha_call_service",
	"description", "Call a Home Assistant service to control a device. Use ha_list_services "+
		"to discover available services and their parameters for each domain. "+
		"Never guess an entity_id: call ha_list_entities first (optionally with a "+
		"domain filter) and use the exact ids it returns. The service domain MUST "+
		"match the entity's own domain: a light.* entity needs light.turn_on, a "+
		"switch.* entity needs switch.turn_on, an input_boolean.* toggle needs "+
		"input_boolean.turn_on — calling the wrong domain affects nothing. The "+
		"error field explains exactly what went wrong (missing entity, wrong "+
		"domain, or already in that state) — follow its suggestion and retry. "+
		"If the operator restricts entities, target explicit entity_ids: calls "+
		"on restricted entities, or by area/device/label or with no target, are refused.",
	"parameters", jsonutil.ObjectOf(
		"type", "object",
		"properties", jsonutil.ObjectOf(
			"domain", jsonutil.ObjectOf(
				"type", "string",
				"description", "Service domain (e.g. 'light', 'switch', 'climate', "+
					"'cover', 'media_player', 'fan', 'scene', 'script').",
			),
			"service", jsonutil.ObjectOf(
				"type", "string",
				"description", "Service name (e.g. 'turn_on', 'turn_off', 'toggle', "+
					"'set_temperature', 'set_hvac_mode', 'open_cover', "+
					"'close_cover', 'set_volume_level').",
			),
			"entity_id", jsonutil.ObjectOf(
				"type", "string",
				"description", "Target entity ID (e.g. 'light.living_room'). "+
					"Some services (like scene.turn_on) may not need this.",
			),
			"data", jsonutil.ObjectOf(
				"type", "string",
				"description", "Additional service data as a JSON string. Examples: "+
					`{"brightness": 255, "color_name": "blue"} for lights, `+
					`{"temperature": 22, "hvac_mode": "heat"} for climate, `+
					`{"volume_level": 0.5} for media players.`,
			),
		),
		"required", []any{"domain", "service"},
	),
)

// Schema and handler of each tool in registration order
var haTools = []struct {
	schema  *jsonutil.Object
	handler tools.Handler
}{
	{haListEntitiesSchema, handleListEntities},
	{haGetStateSchema, handleGetState},
	{haListServicesSchema, handleListServices},
	{haCallServiceSchema, handleCallService},
}

// Registers the four tools in the homeassistant toolset behind the HASS_TOKEN check
func registerTools(pctx *plugins.Context) error {
	ctx := pctx.HomeContext()
	for _, t := range haTools {
		if _, err := pctx.RegisterTool(ctx, plugins.ToolSpec{Name: t.schema.Value("name").(string), Toolset: toolset, Schema: t.schema,
			Handler: t.handler, Check: checkHAAvailable, Emoji: toolEmoji}); err != nil {
			return err
		}
	}
	return nil
}
