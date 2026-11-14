package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Model provider profile of a plugin, its data fields read by the host and its calls run here
type Profile struct {
	Name   string
	Type   string
	Fields map[string]any
	Calls  map[string]Func
}

// Dashboard API request the host forwards, Authenticated carrying the admission of this one request
type APIRequest struct {
	Method        string
	Path          string
	Query         string
	Headers       [][2]string
	Body          []byte
	Authenticated bool
}

// Answer of the plugin to a dashboard API request
type APIResponse struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

// Serves one dashboard API request of the plugin
type APIFunc func(ctx context.Context, req APIRequest) (APIResponse, error)

var (
	profiles     []Profile
	dashboardAPI APIFunc
	configSchema any
)

// Names a model provider profile the plugin offers, called from an init function
func RegisterProfile(p Profile) { profiles = append(profiles, p) }

// Names the function serving the dashboard API of the plugin, called from an init function
func RegisterDashboardAPI(fn APIFunc) { dashboardAPI = fn }

// Names the config schema of a memory provider plugin, called from an init function
func RegisterConfigSchema(schema any) { configSchema = schema }

// Registration a category load keeps here in place of sending it to the host
type capture struct {
	method   string
	forward  bool
	captured *Object
}

// Keeps the first provider handed to the captured registrar, true when the call ends here
func (c *capture) intercept(method string, args []any) bool {
	if method == c.method {
		if obj, ok := firstObject(args); ok && c.captured == nil {
			c.captured = obj
		}
		return true
	}
	return !c.forward && (strings.HasPrefix(method, "register_") || method == "subscribe")
}

// First positional argument when it is a provider object
func firstObject(args []any) (*Object, bool) {
	if len(args) == 0 {
		return nil, false
	}
	obj, ok := args[0].(*Object)
	return obj, ok && obj != nil
}

// Runs the register function of a category plugin and answers the one provider it registered, nil when none
func (p *peer) loadInstance(ctx context.Context, params map[string]any) (any, error) {
	if registered == nil {
		return nil, nil
	}
	manifest, _ := params["manifest"].(map[string]any)
	h := &host{peer: p, key: text(params["plugin_key"]), id: text(params["plugin_id"]), name: text(params["name"]),
		dir: text(params["path"]), profile: text(params["profile_name"]), manifest: manifest, loading: ctx,
		capture: &capture{method: text(params["capture"]), forward: truthy(params["forward"])}}
	p.mu.Lock()
	p.hosts[h.key] = h
	p.mu.Unlock()
	err := registered(h)
	h.mu.Lock()
	h.loading = nil
	h.mu.Unlock()
	if h.capture.captured == nil {
		if err != nil {
			p.unload(h.key)
		}
		return nil, nil
	}
	return p.objectRef(h.capture.captured), nil
}

// Python truthiness of a decoded wire value
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Call of a profile by attribute name, a callable field counting like a method
func (pr Profile) call(attr string) Func {
	if fn := pr.Calls[attr]; fn != nil {
		return fn
	}
	fn, _ := pr.Fields[attr].(Func)
	return fn
}

// Runs one method or callable field of the named model provider profile
func (p *peer) profileCall(ctx context.Context, params map[string]any) (any, error) {
	name := text(params["profile"])
	for _, pr := range profiles {
		if pr.Name != name {
			continue
		}
		fn := pr.call(text(params["attr"]))
		if fn == nil {
			return nil, &Error{Type: "AttributeError", Message: text(params["attr"])}
		}
		return fn(ctx, p.decodeList(params["args"]), p.decodeMap(params["kwargs"]))
	}
	return nil, &Error{Type: "KeyError", Message: fmt.Sprintf("'%s'", name)}
}

// Data fields of a profile with the methods and callable fields that must run in the plugin
func (p *peer) describeProfile(pr Profile) map[string]any {
	fields := map[string]any{}
	calls := map[string]any{}
	for name, value := range pr.Fields {
		if strings.HasPrefix(name, "_") {
			continue
		}
		if _, callable := value.(Func); callable {
			calls[name] = map[string]any{"async": false, "sig": openSignature, "field": true}
			continue
		}
		fields[name] = p.encode(value)
	}
	for name := range pr.Calls {
		if strings.HasPrefix(name, "_") {
			continue
		}
		if _, taken := fields[name]; taken {
			continue
		}
		if _, taken := calls[name]; taken {
			continue
		}
		calls[name] = map[string]any{"async": false, "sig": openSignature, "field": false}
	}
	return map[string]any{"name": pr.Name, "type": pr.Type, "fields": fields, "calls": calls}
}

// JSON document of every profile the plugin registered, the answer of the extract profiles run
func (p *peer) extractProfiles() []byte {
	entries := make([]any, 0, len(profiles))
	for _, pr := range profiles {
		entries = append(entries, p.describeProfile(pr))
	}
	out, err := json.Marshal(map[string]any{"profiles": entries})
	if err != nil {
		out, _ = json.Marshal(map[string]any{"error": "TypeError: " + err.Error()})
	}
	return out
}

// Serves one dashboard API request with the function the plugin registered
func (p *peer) asgi(ctx context.Context, params map[string]any) (any, error) {
	if dashboardAPI == nil {
		return nil, &Error{Type: "AttributeError", Message: fmt.Sprintf("dashboard api %s has no 'router'", path.Base(text(params["api_file"])))}
	}
	req := APIRequest{Method: text(params["method"]), Path: text(params["path"]), Query: text(params["query"]),
		Authenticated: params["authenticated"] == true}
	for _, raw := range asList(params["headers"]) {
		if pair := asList(raw); len(pair) == 2 {
			req.Headers = append(req.Headers, [2]string{text(pair[0]), text(pair[1])})
		}
	}
	req.Body, _ = p.decode(params["body"]).([]byte)
	resp, err := dashboardAPI(ctx, req)
	if err != nil {
		return nil, err
	}
	headers := make([]any, 0, len(resp.Headers))
	for _, h := range resp.Headers {
		headers = append(headers, []any{h[0], h[1]})
	}
	body := resp.Body
	if body == nil {
		body = []byte{}
	}
	return map[string]any{"status": json.Number(fmt.Sprint(resp.Status)), "headers": headers, "body": body}, nil
}

// Assigns a decoded value to a public attribute of a plugin object
func (p *peer) objSetattr(params map[string]any) (any, error) {
	obj := p.object(params["ref"])
	name := text(params["name"])
	if obj == nil || strings.HasPrefix(name, "_") {
		return nil, &Error{Type: "AttributeError", Message: name}
	}
	p.mu.Lock()
	if obj.Attrs == nil {
		obj.Attrs = map[string]any{}
	}
	obj.Attrs[name] = p.decode(params["value"])
	p.mu.Unlock()
	return nil, nil
}

// Argument that makes a program plugin print its profiles as JSON and exit without serving the wire
const ExtractProfilesFlag = "--extract-profiles"
