package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// Transports a server may declare
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
	TransportWS    = "ws"
)

// MCP server as a plugin, a project or an agent declares it
type Server struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	Dir       string
	URL       string
	Headers   map[string]string
	// Command printing the headers as one JSON object
	HeadersHelper string
	// Plugin the server came with, nil for a loose one
	Plugin *Plugin
	// Settings the server needs from the person, by key
	UserConfig map[string]Option
}

// Server as the files spell it
type serverJSON struct {
	Type          string            `json:"type"`
	Transport     string            `json:"transport"`
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	Cwd           string            `json:"cwd"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	HeadersHelper string            `json:"headersHelper"`
	UserConfig    map[string]Option `json:"user_config"`
}

// Reads one server entry, its transport from the type or from the fields it sets
func parseServer(name string, raw json.RawMessage) (*Server, error) {
	var j serverJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("server %s: %w", name, err)
	}
	s := &Server{Name: name, Command: j.Command, Args: j.Args, Env: j.Env, Dir: j.Cwd, URL: j.URL, Headers: j.Headers, HeadersHelper: j.HeadersHelper, UserConfig: j.UserConfig}
	s.Transport = j.Type
	if s.Transport == "" {
		s.Transport = j.Transport
	}
	switch s.Transport {
	case "", TransportStdio, TransportHTTP, TransportSSE, TransportWS, "streamable-http", "streamable_http":
	default:
		return nil, fmt.Errorf("server %s: unknown transport %q", name, s.Transport)
	}
	switch s.Transport {
	case "streamable-http", "streamable_http":
		s.Transport = TransportHTTP
	case "":
		if s.Command != "" {
			s.Transport = TransportStdio
		} else {
			s.Transport = TransportHTTP
		}
	}
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return nil, fmt.Errorf("server %s: a stdio server needs a command", name)
		}
	case TransportWS:
		return nil, fmt.Errorf("server %s: websocket servers are not served, use stdio, http or sse", name)
	default:
		if s.URL == "" {
			return nil, fmt.Errorf("server %s: an %s server needs a url", name, s.Transport)
		}
		u, err := url.Parse(s.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("server %s: url %q is not absolute", name, s.URL)
		}
	}
	for k := range s.Env {
		if k == "PLUGIN_ROOT" || k == "PLUGIN_DATA" {
			return nil, fmt.Errorf("server %s: env may not set %s", name, k)
		}
	}
	return s, nil
}

// Reads a servers map, nested under mcpServers or at the top level, servers in name order
func parseServers(data []byte) ([]*Server, error) {
	var doc struct {
		Schema  string                     `json:"$schema"`
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	servers := doc.Servers
	if servers == nil {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, fmt.Errorf("mcp: %w", err)
		}
		servers = map[string]json.RawMessage{}
		for k, v := range top {
			if k == "$schema" {
				continue
			}
			servers[k] = v
		}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*Server
	var errs []error
	for _, name := range names {
		s, err := parseServer(name, servers[name])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, s)
	}
	return out, errors.Join(errs...)
}

// Reads a servers file, none when it does not exist
func readServersFile(path string) ([]*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out, err := parseServers(data)
	if err != nil {
		return out, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// Copy of the server with ${NAME} replaced in its command, arguments, environment, directory, url and headers
func (s *Server) Substituted(vars map[string]string) *Server {
	out := *s
	out.Command = substituteVars(s.Command, vars)
	out.Dir = substituteVars(s.Dir, vars)
	out.URL = substituteVars(s.URL, vars)
	if len(s.Args) > 0 {
		out.Args = make([]string, len(s.Args))
		for i, a := range s.Args {
			out.Args[i] = substituteVars(a, vars)
		}
	}
	if len(s.Env) > 0 {
		out.Env = make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			out.Env[k] = substituteVars(v, vars)
		}
	}
	if len(s.Headers) > 0 {
		out.Headers = make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			out.Headers[k] = substituteVars(v, vars)
		}
	}
	return &out
}

// Replaces ${NAME} with the value of every variable, longest names first
func SubstituteVars(text string, vars map[string]string) string { return substituteVars(text, vars) }

func substituteVars(text string, vars map[string]string) string {
	if text == "" || len(vars) == 0 {
		return text
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		text = strings.ReplaceAll(text, "${"+k+"}", vars[k])
	}
	return text
}
