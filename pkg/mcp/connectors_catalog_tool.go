package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	CatalogActions   = []string{"search", "install"}
	CatalogItemKinds = []string{"plugin", "skill"}
	catalogTopKeys   = map[string]bool{"action": true, "kind": true, "query": true, "items": true, "reason": true}
	catalogItemKeys  = map[string]bool{"kind": true, "id": true}
)

const CatalogSearchLimit = 10

const CatalogNotHere = "manage_catalog is available only in the Icebath desktop app, where the approval card can be drawn. " +
	"Tell the user to install from a terminal instead: `icebath plugins install <id>` for a plugin, " +
	"`icebath skills install <id>` for a skill."

const CatalogNote = "Settled once; do not re-offer a row the user skipped. Each installed row names target_profile: " +
	"its tools and skills are live now in that profile's open chats and in every new chat of it; " +
	"they are callable in this chat only when this chat runs in that profile. A failed row carries " +
	"the reason in detail."

func catalogEnum(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

var ManageCatalogSchema = map[string]any{
	"name": "manage_catalog",
	"description": "Find and install Icebath catalog plugins and hub skills for the user. 'search' lists matches " +
		"(id, kind, display, tier, platforms, installed) and changes nothing. 'install' shows the user " +
		"one approval card with a row per item and blocks until every row is installed, skipped or " +
		"failed, or the card is closed; the host installs each approved row into this chat's profile at " +
		"the catalog's reviewed version, and turns on a catalog plugin that is installed but not " +
		"enabled. Pass only catalog ids exactly as 'search' returns them; the " +
		"host decides the source, version, profile and settings, and the user can change them on the " +
		"card. Offer an install only for something the user asked for or agreed to.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": catalogEnum(CatalogActions)},
			"kind": map[string]any{"type": "string", "enum": catalogEnum(CatalogItemKinds),
				"description": "search only: limit results to plugins or skills."},
			"query": map[string]any{"type": "string", "description": "search only: words to match; empty lists plugins."},
			"items": map[string]any{
				"type":        "array",
				"description": "install only: the catalog items to offer, one card row each.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "enum": catalogEnum(CatalogItemKinds)},
						"id":   map[string]any{"type": "string", "description": "Catalog id as 'search' returns it."},
					},
					"required":             []any{"kind", "id"},
					"additionalProperties": false,
				},
			},
			"reason": map[string]any{"type": "string", "description": "install only: one line on why these are offered."},
		},
		"required":             []any{"action"},
		"additionalProperties": false,
	},
}

func catalogKeys(v any) []string {
	switch d := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		return keys
	case *pyjson.Object:
		return append([]string(nil), d.Keys...)
	}
	return nil
}

func catalogIsDict(v any) bool {
	switch d := v.(type) {
	case map[string]any:
		return true
	case *pyjson.Object:
		return d != nil
	}
	return false
}

func catalogOneOf(v any, values []string) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, x := range values {
		if s == x {
			return true
		}
	}
	return false
}

func catalogTextOf(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Refusal text for keys or values the model may not send, empty when the call may run
func catalogCheckArgs(args map[string]any) string {
	var unknown []string
	for k := range args {
		if !catalogTopKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Sprintf("unknown parameter(s) %s: manage_catalog takes action, kind, query, "+
			"items and reason. The host decides source, version, profile and settings.", strings.Join(unknown, ", "))
	}
	if !catalogOneOf(args["action"], CatalogActions) {
		return fmt.Sprintf("action must be one of %s.", strings.Join(CatalogActions, ", "))
	}
	if kind, ok := args["kind"]; ok && kind != nil && !catalogOneOf(kind, CatalogItemKinds) {
		return fmt.Sprintf("kind must be one of %s.", strings.Join(CatalogItemKinds, ", "))
	}
	if args["action"] == "search" {
		return ""
	}
	items := catalogList(args["items"])
	if _, isList := args["items"].([]any); (!isList && items == nil) || len(items) == 0 {
		return "install needs 'items': [{\"kind\": \"plugin\", \"id\": \"<catalog id>\"}, ...]."
	}
	for _, item := range items {
		if !catalogIsDict(item) {
			return "every item is an object {kind, id}."
		}
		var extra []string
		for _, k := range catalogKeys(item) {
			if !catalogItemKeys[k] {
				extra = append(extra, k)
			}
		}
		if len(extra) > 0 {
			sort.Strings(extra)
			return fmt.Sprintf("unknown item field(s) %s: an item is {kind, id}. The host decides "+
				"source, version, profile and settings.", strings.Join(extra, ", "))
		}
		fields := catalogDict(item)
		if !catalogOneOf(fields["kind"], CatalogItemKinds) || pystr.Strip(catalogTextOf(fields["id"])) == "" {
			return "every item needs kind (plugin or skill) and a non-empty id."
		}
	}
	return ""
}

// CatalogOptions carries the session, the card callback and the installer of one manage_catalog call
type CatalogOptions struct {
	SessionID   string
	ToolCallID  string
	Callback    tools.ConnCallback
	CardSurface bool
	Installer   CatalogInstaller
}

// Desktop chat catalog search, or install through the approval card, refused anywhere else
func ManageCatalog(ctx context.Context, env *tools.Env, args map[string]any, opts CatalogOptions) (string, error) {
	if msg := catalogCheckArgs(args); msg != "" {
		return toolError(msg), nil
	}
	if opts.Callback == nil || !opts.CardSurface {
		return toolError(CatalogNotHere), nil
	}
	if args["action"] == "search" {
		kind, _ := args["kind"].(string)
		out, err := CatalogSearch(ctx, catalogTextOf(args["query"]), kind)
		if err != nil {
			return "", err
		}
		text, err := pyjson.Dumps(out, pyjson.EnsureASCII(false))
		return text, err
	}
	return CatalogInstall(ctx, env, catalogList(args["items"]), opts), nil
}

// Opens one approval card with a row per distinct item and blocks until it settles
func CatalogInstall(ctx context.Context, env *tools.Env, items []any, opts CatalogOptions) string {
	type key struct{ kind, id string }
	seen := map[key]bool{}
	var targets []*tools.ConnTarget
	for _, item := range items {
		fields := catalogDict(item)
		k := key{pyrepr.Str(fields["kind"]), pystr.Strip(pyrepr.Str(fields["id"]))}
		if !seen[k] {
			seen[k] = true
			targets = append(targets, tools.NewConnTarget(k.id, k.kind, "install"))
		}
	}
	runner := OpenCatalogRunner(ctx, env, opts.Installer)
	defer runner.Close()
	kind := tools.ConnKind{Prepare: func(op *tools.ConnectionOperation) { runner.Prepare(ctx, op) }, Observe: runner.Observe,
		Note: CatalogNote}
	return tools.RunOperation(ctx, env, targets, kind, tools.RunOptions{SessionKey: tools.OperationSessionKey(env, opts.SessionID),
		ToolCallID: opts.ToolCallID, Callback: opts.Callback})
}

// Catalog plugin and hub skill matches with whether this chat's profile has each installed
func CatalogSearch(ctx context.Context, query, kind string) (*pyjson.Object, error) {
	rows := []any{}
	if kind == "" || kind == "plugin" {
		plugin, err := catalogPluginRows(ctx, query)
		if err != nil {
			return nil, err
		}
		rows = append(rows, plugin...)
	}
	if (kind == "" || kind == "skill") && pystr.Strip(query) != "" {
		skill, err := catalogSkillRows(ctx, query)
		if err != nil {
			return nil, err
		}
		rows = append(rows, skill...)
	}
	var installedIn any
	if label := home.ProfileNameFor(home.Dir(ctx)); label != "" {
		installedIn = label
	}
	return pyjson.ObjectOf("results", rows, "installed_in", installedIn), nil
}

func catalogPluginRows(ctx context.Context, query string) ([]any, error) {
	installed := map[string]bool{}
	metadata, err := plugins.ReadInstallMetadata(home.Dir(ctx))
	var opErr *plugins.PluginOperationError
	switch {
	case errors.As(err, &opErr):
	case err != nil:
		return nil, err
	default:
		for _, rec := range metadata {
			catalog := catalogDict(catalogDict(rec)["catalog"])
			if !catalogIsDict(catalogDict(rec)["catalog"]) || !pyrepr.Truthy(catalog["name"]) {
				continue
			}
			installed[pyrepr.Str(catalog["name"])] = true
		}
	}
	entries := plugins.FilterEntries(plugins.LoadCatalogLive(ctx), query)
	if len(entries) > CatalogSearchLimit {
		entries = entries[:CatalogSearchLimit]
	}
	rows := make([]any, 0, len(entries))
	for _, e := range entries {
		platforms := make([]any, len(e.Platforms))
		for i, p := range e.Platforms {
			platforms[i] = p
		}
		rows = append(rows, pyjson.ObjectOf("id", e.Name, "kind", "plugin", "display", catalogDisplay(e.Name), "tier", e.Tier,
			"platforms", platforms, "installed", installed[e.Name]))
	}
	return rows, nil
}

// Hub source router and unified search seams the tests replace
var (
	catalogSkillSources  = func(ctx context.Context, x *skills.HubCLI) []skills.SkillSource { return x.ResolvedSources() }
	catalogUnifiedSearch = func(ctx context.Context, x *skills.HubCLI, query string, sources []skills.SkillSource, limit int) []skills.SkillMeta {
		return x.Hub.UnifiedSearch(ctx, query, sources, "all", limit)
	}
)

func catalogSkillRows(ctx context.Context, query string) ([]any, error) {
	x := skills.NewHubCLI(skills.NewScope(home.Dir(ctx), nil))
	installed, err := x.Hub.LockFile("").ByIdentifier()
	if err != nil {
		return nil, err
	}
	hits := catalogUnifiedSearch(ctx, x, query, catalogSkillSources(ctx, x), CatalogSearchLimit)
	rows := make([]any, 0, len(hits))
	for _, r := range hits {
		tier := "community"
		if r.Source == "official" {
			tier = "official"
		}
		rows = append(rows, pyjson.ObjectOf("id", r.Identifier, "kind", "skill", "display", r.Name, "tier", tier,
			"platforms", []any{}, "installed", skills.InstalledEntry(installed, r.Identifier, r.Source) != nil))
	}
	return rows, nil
}

func init() {
	tools.Register(tools.Def{Name: "manage_catalog", Toolset: "catalog", Schema: ManageCatalogSchema, Emoji: "🧩",
		Handler: func(ctx context.Context, call *tools.Call) string {
			out, err := ManageCatalog(ctx, call.Env, call.Args, CatalogOptions{SessionID: call.SessionID})
			if err != nil {
				panic(err)
			}
			return out
		}})
}
