package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Catalog card app state with the reason sentence, empty when present or unknown
type Presence struct {
	State    string `json:"state"`
	Sentence string `json:"sentence"`
}

// ServerLiveness is a parsed server liveness declaration, field for field the mcp Liveness
type ServerLiveness struct {
	Kind       string
	Path       string
	URLField   string
	TokenField string
	PIDField   string
}

// OnboardingEntry is one onboarding card row with its app state
type OnboardingEntry struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tier        string   `json:"tier"`
	Platforms   []string `json:"platforms"`
	AppState    string   `json:"app_state"`
	Sentence    string   `json:"sentence"`
}

// NamedPresence is one questionnaire row with the app state and the description disclosure
type NamedPresence struct {
	Name       string `json:"name"`
	Title      string `json:"title"`
	State      string `json:"state"`
	Sentence   string `json:"sentence"`
	Disclosure string `json:"disclosure"`
}

var UnknownPresence = Presence{State: "unknown"}

const (
	presenceTimeout  = 5 * time.Second
	presenceMaxBytes = 256 * 1024
)

var (
	presenceGitHub     = regexp2.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`, regexp2.None)
	presenceDisclosure = regexp2.MustCompile(`\bDisclosure\s*[:\u2014]\s*(.+)`, regexp2.None)
	presenceRawBase    = "https://raw.githubusercontent.com"
	presenceClient     = &http.Client{Timeout: presenceTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 20 {
			return errors.New("too many redirects")
		}
		return nil
	}}
	presenceCatalog = LoadCatalogLive
	presenceCache   = struct {
		sync.Mutex
		manifests map[[3]string]*pyjson.Object
	}{manifests: map[[3]string]*pyjson.Object{}}
)

// Raw github URL of plugin.json at the pinned commit, empty for a repo off github
func rawManifestURL(repo, sha, subdir string) string {
	m, _ := presenceGitHub.FindStringMatch(pystr.Strip(repo))
	if m == nil {
		return ""
	}
	var parts []string
	for _, p := range []string{strings.Trim(subdir, "/"), "plugin.json"} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s", presenceRawBase, m.GroupByNumber(1).String(), m.GroupByNumber(2).String(), sha, strings.Join(parts, "/"))
}

// Manifest of one pin, remembered for good when read or absent and forgotten after a transient failure
func pinnedManifest(ctx context.Context, repo, sha, subdir string) *pyjson.Object {
	key := [3]string{repo, sha, subdir}
	presenceCache.Lock()
	cached, ok := presenceCache.manifests[key]
	presenceCache.Unlock()
	if ok {
		return cached
	}
	var manifest *pyjson.Object
	if url := rawManifestURL(repo, sha, subdir); url != "" {
		body, status, err := fetchPresenceManifest(ctx, url)
		if err != nil {
			logDebug(fmt.Sprintf("plugin catalog presence: %s unreadable: %s", url, err))
			return nil
		}
		if status == http.StatusOK && len(body) <= presenceMaxBytes {
			data, err := pyjson.LoadsBytes(body)
			if err != nil {
				logDebug(fmt.Sprintf("plugin catalog presence: %s unreadable: %s", url, err))
				return nil
			}
			manifest, _ = data.(*pyjson.Object)
		} else if status != http.StatusNotFound {
			return nil
		}
	}
	presenceCache.Lock()
	presenceCache.manifests[key] = manifest
	presenceCache.Unlock()
	return manifest
}

// Status and body of one manifest GET, the body read one byte past the size cap
func fetchPresenceManifest(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := presenceClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// TypeError the liveness parser raises for a fields value that is not a mapping, nil otherwise
func livenessFieldsTypeError(raw map[string]any) error {
	if raw["kind"] != "server_json" {
		return nil
	}
	for k := range raw {
		if k != "kind" && k != "path" && k != "fields" {
			return nil
		}
	}
	if f := raw["fields"]; pyrepr.Truthy(f) {
		if _, ok := f.(map[string]any); !ok {
			return fmt.Errorf("'%s' object is not a mapping", pyTypeName(f))
		}
	}
	return nil
}

// App state of one declared server judged by availability, then liveness, then a local probe
func serverPresence(ctx context.Context, decl *tools.PlatformDeclaration, livenessRaw map[string]any, title string) (Presence, error) {
	available := tools.PlatformAvailabilityOf(decl, "")
	switch available.State {
	case "no_requirements":
		return UnknownPresence, nil
	case "missing_app", "unsupported_os":
		return Presence{"missing_app", "needs " + title}, nil
	case "unsupported_gpu":
		gpu := ""
		if g := decl.RequiredGPU(); g != nil {
			gpu = *g
		}
		return Presence{"missing_app", "needs " + tools.PlatformGPULabel(gpu)}, nil
	case "version_too_old":
		found := ""
		if available.Version != nil && *available.Version != "" {
			found = fmt.Sprintf(" (found %s)", *available.Version)
		}
		minVersion := "None"
		if available.MinVersion != nil {
			minVersion = *available.MinVersion
		}
		return Presence{"missing_app", fmt.Sprintf("needs %s %s or newer%s", title, minVersion, found)}, nil
	}
	var live *ServerLiveness
	if livenessRaw != nil {
		if err := livenessFieldsTypeError(livenessRaw); err != nil {
			return Presence{}, err
		}
		parse, err := needService("ParseServerLiveness", func(s *Services) func(any) (ServerLiveness, error) { return s.ParseServerLiveness })
		if err != nil {
			return Presence{}, err
		}
		if parsed, err := parse(livenessRaw); err == nil {
			live = &parsed
		}
	}
	if live == nil || live.Kind == "static" {
		return Presence{State: "present"}, nil
	}
	if live.Kind == "interactive_session" {
		if hostinfo.InteractiveSession() {
			return Presence{State: "present"}, nil
		}
		return Presence{"app_not_running", title + " needs an interactive desktop session"}, nil
	}
	definition, ok := decl.AppFor(hostinfo.OSFamily())
	if !ok {
		return Presence{"missing_app", "needs " + title}, nil
	}
	if live.Kind == "server_json" {
		definition.LivenessKind, definition.LivenessPath = "server_json", live.Path
		definition.LivenessPIDKey, definition.LivenessURLKey, definition.LivenessTokenKey = live.PIDField, live.URLField, live.TokenField
	}
	resolver := tools.PlatformAppResolver{Definition: definition}
	probe, err := resolver.Probe(resolver.Locate(nil), tools.EffortLocal, 3.0)
	if err != nil {
		return Presence{}, err
	}
	if probe.Running.Value != nil && *probe.Running.Value {
		return Presence{State: "present"}, nil
	}
	return Presence{"app_not_running", title + " is not running"}, nil
}

// Ordered server names under the vendor extension of a manifest object
func presenceServerOrder(raw *pyjson.Object) []string {
	cur := raw
	for _, key := range []string{"extensions", portableExtension, "servers"} {
		next, _ := cur.Vals[key].(*pyjson.Object)
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur.Keys
}

// App state of one catalog entry on this host, the worst server winning in manifest order
func CatalogPresence(ctx context.Context, entry *PluginCatalogEntry) (Presence, error) {
	raw := pinnedManifest(ctx, entry.Repo, entry.Sha, entry.Subdir)
	if raw == nil || len(raw.Keys) == 0 {
		return UnknownPresence, nil
	}
	manifest, _ := pyjson.Plain(raw).(map[string]any)
	ext := manifest["extensions"]
	if !pyrepr.Truthy(ext) {
		ext = map[string]any{}
	}
	extMap, ok := ext.(map[string]any)
	if !ok {
		return Presence{}, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(ext))
	}
	vendor, present := extMap[portableExtension]
	if !present {
		vendor = map[string]any{}
	}
	vendorMap, ok := vendor.(map[string]any)
	if !ok {
		return Presence{}, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(vendor))
	}
	rawServers := vendorMap["servers"]
	if !pyrepr.Truthy(rawServers) {
		return UnknownPresence, nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return UnknownPresence, nil
	}
	names := map[string]map[string]any{}
	for name := range servers {
		names[name] = map[string]any{}
	}
	decls, err := portableServerDeclarations(manifest, names, nil)
	type parsedDecl struct {
		decl     *tools.PlatformDeclaration
		liveness map[string]any
	}
	var ordered []parsedDecl
	for _, name := range presenceServerOrder(raw) {
		if err != nil {
			break
		}
		d, ok := decls[name]
		if !ok {
			continue
		}
		var rawApp, rawReq any
		if d.App != nil {
			rawApp = d.App
		}
		if d.Requires != nil {
			rawReq = d.Requires
		}
		var pd *tools.PlatformDeclaration
		pd, err = tools.ParsePlatformDeclaration(name, rawApp, rawReq, "plugin.json extension server "+pyrepr.Repr(name))
		ordered = append(ordered, parsedDecl{pd, d.Liveness})
	}
	if err != nil {
		logDebug(fmt.Sprintf("plugin catalog presence: %s declaration invalid: %s", entry.Name, err))
		return UnknownPresence, nil
	}
	title := entry.Title
	if title == "" {
		title = entry.Name
	}
	rank := map[string]int{"missing_app": 0, "app_not_running": 1, "present": 2, "unknown": 3}
	worst, have := UnknownPresence, false
	for _, d := range ordered {
		p, err := serverPresence(ctx, d.decl, d.liveness, title)
		if err != nil {
			return Presence{}, err
		}
		if !have || rank[p.State] < rank[worst.State] {
			worst, have = p, true
		}
	}
	return worst, nil
}

// Whether the catalog entry names no platform or names this host os family
func runsHere(entry *PluginCatalogEntry, here string) bool {
	return len(entry.Platforms) == 0 || normalizedPlatforms(entry.Platforms)[here]
}

// Onboarding curated catalog entries this os family runs
func onboardingCatalog(ctx context.Context) []*PluginCatalogEntry {
	here := hostinfo.OSFamily()
	var out []*PluginCatalogEntry
	for _, entry := range presenceCatalog(ctx) {
		if entry.Onboarding && runsHere(entry, here) {
			out = append(out, entry)
		}
	}
	return out
}

// Exported onboarding names for the connectors catalog above pkg/plugins
func OnboardingNames(ctx context.Context) []string { return onboardingNames(ctx) }

// Catalog names of the onboarding plugins this os family runs, with no presence probe
func onboardingNames(ctx context.Context) []string {
	var out []string
	for _, entry := range onboardingCatalog(ctx) {
		out = append(out, entry.Name)
	}
	return out
}

// Onboarding card rows this os family runs, each with its app state
func OnboardingEntries(ctx context.Context) ([]OnboardingEntry, error) {
	var rows []OnboardingEntry
	for _, entry := range onboardingCatalog(ctx) {
		found, err := CatalogPresence(ctx, entry)
		if err != nil {
			return nil, err
		}
		title := entry.Title
		if title == "" {
			title = entry.Name
		}
		rows = append(rows, OnboardingEntry{Name: entry.Name, Title: title, Description: firstSentence(entry.Description),
			Tier: entry.Tier, Platforms: append([]string{}, entry.Platforms...), AppState: found.State, Sentence: found.Sentence})
	}
	return rows, nil
}

// Named catalog plugins this os family runs in the order asked, manifests read in parallel, unknown names dropped
func PresenceFor(ctx context.Context, names []string) ([]NamedPresence, error) {
	here := hostinfo.OSFamily()
	byName := map[string]*PluginCatalogEntry{}
	for _, entry := range presenceCatalog(ctx) {
		byName[entry.Name] = entry
	}
	seen := map[string]bool{}
	var entries []*PluginCatalogEntry
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if entry, ok := byName[name]; ok && runsHere(entry, here) {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	found := make([]Presence, len(entries))
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Go(func() { found[i], errs[i] = CatalogPresence(ctx, entry) })
	}
	wg.Wait()
	rows := make([]NamedPresence, 0, len(entries))
	for i, entry := range entries {
		if errs[i] != nil {
			return nil, errs[i]
		}
		title := entry.Title
		if title == "" {
			title = entry.Name
		}
		rows = append(rows, NamedPresence{Name: entry.Name, Title: title, State: found[i].State,
			Sentence: found[i].Sentence, Disclosure: disclosureOf(entry.Description)})
	}
	return rows, nil
}

// Whitespace runs collapsed to single spaces like a split and join
func collapseSpace(text string) string {
	return strings.Join(pystr.SplitWhitespace(text, -1), " ")
}

// First sentence after the description Disclosure marker, empty when absent
func disclosureOf(text string) string {
	m, _ := presenceDisclosure.FindStringMatch(collapseSpace(text))
	if m == nil {
		return ""
	}
	return firstSentence(m.GroupByNumber(1).String())
}

// Text up to and including the first period followed by a space
func firstSentence(text string) string {
	text = collapseSpace(text)
	if head, _, ok := strings.Cut(text, ". "); ok {
		return head + "."
	}
	return text
}
