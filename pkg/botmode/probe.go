package botmode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// BotChatTitle is the only session title that receives the teammate protocol section
const BotChatTitle = "Bot Chat"

// botsMetaKey is the profile.yaml ui_meta block that marks a profile as Bot Mode managed
const botsMetaKey = "icebath-bots"

// defaultHandle is the handle the default profile answers to while no named profile holds it
const defaultHandle = "icebath"

var (
	sectionMu    sync.Mutex
	sectionCache = map[string]string{}
)

// Roster source the probe reads, replaced by tests that make the roster fail
var rosterFn = func(root string) ([]config.BotProfile, error) { return Roster(root), nil }

// Ambient process home as a string
func defaultHome() string {
	return home.Process()
}

// Given home, else the ambient process home
func resolveHome(h string) string {
	if h != "" {
		return pypath.PathStr(h)
	}
	return pypath.PathStr(defaultHome())
}

// InstallRoot is the install root shared by the default profile and every named profile
func InstallRoot(h string) string {
	parent := pypath.PathParent(h)
	if pypath.PathName(parent) == "profiles" {
		return pypath.PathParent(parent)
	}
	return pypath.PathStr(h)
}

// Profile folder id of a home, default for the install root
func profileName(h string) string {
	if pypath.PathName(pypath.PathParent(h)) == "profiles" {
		return pypath.PathName(h)
	}
	return "default"
}

// Folder id of the profile a home belongs to, default for the install root
func ProfileName(h string) string { return profileName(h) }

// Handle is the at handle a local profile answers to among the roster folder ids
func Handle(name string, names []string) string {
	if name == "default" && !containsString(names, defaultHandle) {
		return defaultHandle
	}
	return name
}

// ProfileForHandle is the folder id a bare handle names on this install
func ProfileForHandle(handle string, names []string) string {
	if handle == defaultHandle && !containsString(names, defaultHandle) {
		return "default"
	}
	return handle
}

// Reports whether names holds name
func containsString(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// Value of fn, def when it fails or panics, so the probe never crashes a prompt build
func swallow[T any](fn func() (T, error), def T) (out T) {
	defer func() {
		if recover() != nil {
			out = def
		}
	}()
	v, err := fn()
	if err != nil {
		return def
	}
	return v
}

// Roster lists the default profile and every live named profile, sorted by folder id
func Roster(root string) []config.BotProfile {
	named := swallow(func() ([]config.BotProfile, error) {
		profiles := pypath.PathJoin(root, "profiles")
		if st, err := os.Stat(profiles); err != nil || !st.IsDir() {
			return nil, nil
		}
		entries, err := os.ReadDir(profiles)
		if err != nil {
			return nil, err
		}
		var found []config.BotProfile
		for _, e := range entries {
			name := e.Name()
			dir := pypath.PathJoin(profiles, name)
			if name != "default" && home.ProfileIDRe.MatchString(strings.TrimSuffix(name, "\n")) && home.ProfileLive(dir) {
				found = append(found, config.BotProfile{Name: name, Dir: dir})
			}
		}
		return found, nil
	}, nil)
	return append([]config.BotProfile{{Name: "default", Dir: root}}, named...)
}

// ConfigRoster is the roster of the install that holds a home, the shape config migrations take
func ConfigRoster(h string) []config.BotProfile {
	return Roster(InstallRoot(h))
}

// YAML mapping at a path, nil when missing, unreadable, not a mapping, or lacking the needle
func readYAMLDict(path, needle string) map[string]any {
	return swallow(func() (map[string]any, error) {
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return nil, nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text := pystr.DecodeUTF8Replace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
		if needle != "" && !strings.Contains(text, needle) {
			return nil, nil
		}
		data, err := config.FastSafeLoad([]byte(text))
		if err != nil {
			return nil, err
		}
		m, _ := asDict(data)
		return m, nil
	}, nil)
}

// Mapping view of a parsed YAML value, false for anything that is not a mapping
func asDict(v any) (map[string]any, bool) {
	switch v.(type) {
	case map[string]any, map[any]any, config.Tree:
		return config.AsMap(v), true
	}
	return nil, false
}

// Bot Mode block of a parsed profile.yaml, false when absent or not a mapping
func botsMeta(data map[string]any) (map[string]any, bool) {
	if len(data) == 0 {
		return nil, false
	}
	ui, ok := asDict(data["ui_meta"])
	if !ok {
		return nil, false
	}
	return asDict(ui[botsMetaKey])
}

// Reports whether a profile carries the Bot Mode block
func isBotManaged(profileDir string) bool {
	_, ok := botsMeta(readYAMLDict(pypath.PathJoin(profileDir, "profile.yaml"), botsMetaKey))
	return ok
}

// Reports whether any profile of the install is Bot Mode managed
func anyManaged(root string) (bool, error) {
	roster, err := rosterFn(root)
	if err != nil {
		return false, err
	}
	for _, p := range roster {
		if isBotManaged(p.Dir) {
			return true, nil
		}
	}
	return false, nil
}

// IsBotModeManaged is true when any profile on the install of a home is Bot Mode managed, never failing
func IsBotModeManaged(h string) bool {
	return swallow(func() (bool, error) { return anyManaged(InstallRoot(resolveHome(h))) }, false)
}

// Non empty parts joined by an em dash
func roleLine(parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " — ")
}

// Roster bullet of a backticked handle plus each non empty part
func bullet(handle string, parts ...string) string {
	return roleLine(append([]string{"- `" + handle + "`"}, parts...)...)
}

// Title and display name of a Bot Mode block, stripped, empty when unset
func friendlyFields(data map[string]any) (string, string) {
	meta, _ := botsMeta(data)
	return pystr.Strip(strOr(meta["title"])), pystr.Strip(strOr(data["display_name"]))
}

// One line teammate role of a profile, display name, title and description, at most 160 characters
func profileRole(profileDir string) string {
	return swallow(func() (string, error) {
		data := readYAMLDict(pypath.PathJoin(profileDir, "profile.yaml"), "")
		title, display := friendlyFields(data)
		lowered := pystr.Lower(display)
		if lowered == pystr.Lower(pypath.PathName(profileDir)) || lowered == pystr.Lower(title) {
			display = ""
		}
		line := roleLine(display, title, pystr.Strip(strOr(data["description"])))
		return headRunes(strings.Join(pystr.SplitWhitespace(line, -1), " "), 160), nil
	}, "")
}

// Bot Mode title and profile display name of a profile, empty when unset
func friendlyNames(profileDir string) (string, string) {
	return friendlyFields(readYAMLDict(pypath.PathJoin(profileDir, "profile.yaml"), ""))
}

// DisplayName is the sender name, Bot Mode title, then display name, else the handle
func DisplayName(name, profileDir string, names []string) string {
	title, display := friendlyNames(profileDir)
	if title != "" {
		return title
	}
	if display != "" {
		return display
	}
	return Handle(name, names)
}

var (
	reservedAliases = map[string]bool{"all": true, "everyone": true, "user": true, "default": true, defaultHandle: true}
	aliasNonSlugRe  = regexp.MustCompile(`[^a-z0-9_-]+`)
	aliasFormRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// AliasForms gives the lower cased slug and collapsed mention forms of a friendly name, reserved and empty dropped
func AliasForms(value string) map[string]bool {
	name := pystr.Lower(pystr.Strip(value))
	slug := strings.Trim(aliasNonSlugRe.ReplaceAllString(name, "-"), "-")
	collapsed := aliasNonSlugRe.ReplaceAllString(name, "")
	out := map[string]bool{}
	for _, f := range []string{slug, collapsed} {
		if f != "" && aliasFormRe.MatchString(f) && !reservedAliases[f] {
			out[f] = true
		}
	}
	return out
}

// Alias form to folder ids of every local profile friendly name, propagating a roster failure
func localAliasMap(root string) (map[string]map[string]bool, error) {
	roster, err := rosterFn(root)
	if err != nil {
		return nil, err
	}
	aliases := map[string]map[string]bool{}
	for _, p := range roster {
		title, display := friendlyNames(p.Dir)
		for _, friendly := range []string{title, display} {
			for form := range AliasForms(friendly) {
				if aliases[form] == nil {
					aliases[form] = map[string]bool{}
				}
				aliases[form][p.Name] = true
			}
		}
	}
	return aliases, nil
}

// LocalAliasMap maps each friendly name alias form to the folder ids answering to it, empty on failure
func LocalAliasMap(root string) map[string]map[string]bool {
	aliases, err := localAliasMap(root)
	if err != nil {
		return map[string]map[string]bool{}
	}
	return aliases
}

// Folder ids, handles and friendly name slugs this install answers to, propagating a roster failure
func localTakenForms(h string) (map[string]bool, error) {
	root := InstallRoot(h)
	roster, err := rosterFn(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(roster))
	for _, p := range roster {
		names = append(names, p.Name)
	}
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
		out[Handle(name, names)] = true
	}
	for form := range LocalAliasMap(root) {
		out[form] = true
	}
	return out, nil
}

// LocalTakenForms gives the bare forms local profiles answer to, which a remote row must never take
func LocalTakenForms(h string) (map[string]bool, error) {
	return localTakenForms(h)
}

// Registered peer gateway names of an install root, sorted and empty on any failure
func Peers(root string) []string { return peers(root) }

// Registered peer gateway names from the install config, sorted, empty on any failure
func peers(root string) []string {
	doc, err := config.ReadConfigDocReadonly(context.Background(), pypath.PathJoin(root, "config.yaml"))
	if err != nil {
		return nil
	}
	m, ok := asDict(doc)
	if !ok {
		return nil
	}
	table, ok := asDict(m["bot_peers"])
	if !ok {
		return nil
	}
	var out []string
	for name := range table {
		if pystr.Strip(name) != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Addendum for agents on other connected machines, empty when the relay roster is empty
func remoteParagraph(h string) (string, error) {
	roster := ReadRemoteRoster(h)
	if len(roster) == 0 {
		return "", nil
	}
	taken, err := localTakenForms(h)
	if err != nil {
		return "", err
	}
	forms := RemoteTargetForms(roster, taken)
	lines := make([]string, 0, len(roster))
	for i, row := range roster {
		label := row.ConnectionLabel
		if label == "" {
			label = row.ConnectionID
		}
		lines = append(lines, bullet("@"+forms[i], "on "+label, row.Title, row.Description))
	}
	return "\n\nTeammates on OTHER connected machines (reachable through the " +
		"Desktop relay — message them with message_agent exactly like local " +
		"teammates; replies arrive as completion notifications the same " +
		"way, or via reply_delivery=\"poll\" as below):\n" + strings.Join(lines, "\n"), nil
}

// Addendum for cross machine messages, empty when no peer gateway is registered
func peerParagraph(root string) string {
	names := peers(root)
	if len(names) == 0 {
		return ""
	}
	quoted := make([]string, len(names))
	for i, p := range names {
		quoted[i] = "`" + p + "`"
	}
	return "\n\nTeammates on OTHER machines: this install also has peer gateways " +
		"registered (" + strings.Join(quoted, ", ") + "). Message an agent on a peer the same way — " +
		"message_agent with target \"<peer>/<agent-name>\" (or \"<peer>\" alone " +
		"for the peer's main agent). Run `icebath peer list` for the live " +
		"peer list."
}

// Teammate protocol section of a home, empty when no profile is managed
func buildSection(h string) (string, error) {
	root := InstallRoot(h)
	me := profileName(h)
	managed, err := anyManaged(root)
	if err != nil || !managed {
		return "", err
	}
	roster, err := rosterFn(root)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(roster))
	for _, p := range roster {
		names = append(names, p.Name)
	}
	var lines []string
	for _, p := range roster {
		if p.Name != me {
			lines = append(lines, bullet("@"+Handle(p.Name, names), profileRole(p.Dir)))
		}
	}
	block := strings.Join(lines, "\n")
	if block == "" {
		block = "- (no teammates yet)"
	}
	remote, err := remoteParagraph(h)
	if err != nil {
		return "", err
	}
	return tools.BotModeProtocolHeading + "\n" +
		"This install runs Bot Mode: each Icebath profile is an agent teammate with " +
		"one canonical \"Bot Chat\" conversation, and you have the `message_agent` " +
		"tool to DM any of them. It is FIRE-AND-FORGET: it delivers your message " +
		"with your attribution prefixed automatically and returns an acknowledgement " +
		"immediately — it never returns the reply. Send it, finish your turn, and " +
		"the reply arrives later as a background-process completion notification " +
		"that wakes you; relay it to the user then, attributed to that agent — unless " +
		"the ack returns reply_delivery=\"poll\", in which case follow its " +
		"process(action=\"wait\") instruction before ending the turn. " +
		"COMPOSE every message yourself — say what YOU need from that agent; never " +
		"forward the user's words verbatim, and never reveal private 1:1 chat " +
		"content. When the user says \"ask <name>\" or \"tell <name> ...\", that is " +
		"a handoff: pick the right teammate from the roster below, message them " +
		"with message_agent, and report back naming which agent replied. Message " +
		"ONE clearly relevant teammate; don't fan out to several unless the user " +
		"explicitly asked.\n" +
		"When YOU receive a \"Message from \U0001f916 <name> (@<handle>):\" message, a " +
		"teammate agent is talking to you (not the user): address them, reply " +
		"concisely via message_agent to their handle, and if it is a pure FYI " +
		"with nothing to add, staying silent is fine — never ping-pong " +
		"acknowledgements.\n" +
		"You are `@" + Handle(me, names) + "`. Your teammates (live roster; roles from their " +
		"profiles):\n" +
		block + remote + peerParagraph(root), nil
}

// ProtocolSection is the cached teammate section of the agent's own home, rebuilt when forced, empty on failure
func ProtocolSection(h string, forceRefresh bool) string {
	resolved := resolveHome(h)
	sectionMu.Lock()
	defer sectionMu.Unlock()
	if cached, ok := sectionCache[resolved]; ok && !forceRefresh {
		return cached
	}
	section := swallow(func() (string, error) { return buildSection(resolved) }, "")
	sectionCache[resolved] = section
	return section
}

// ResetProtocolCache drops every cached teammate section
func ResetProtocolCache() {
	sectionMu.Lock()
	sectionCache = map[string]string{}
	sectionMu.Unlock()
}

const epochPrefix = "Capability epoch: "

var epochRe = regexp.MustCompile(`Capability epoch: ([0-9a-f]{12})`)

// Model overrides whose flip changes a rebuilt prompt, vision as a strict bool and a positive context window
func modelPromptCapabilitySurface(modelCfg any) (map[string]any, error) {
	cfg, _ := asDict(modelCfg)
	var ctx any
	switch raw := cfg["context_length"].(type) {
	case bool:
	case string:
		stripped := pystr.Strip(raw)
		if stripped != "" && allDigits(stripped) {
			n, ok := pyint.Parse(stripped)
			if !ok {
				return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(stripped))
			}
			if n.Sign() > 0 {
				ctx = n
			}
		}
	default:
		if n, ok := pyrepr.Int(raw); ok && n.Sign() > 0 {
			ctx = n
		}
	}
	var vision any
	if v, ok := tools.CoerceCapabilityBool(cfg["supports_vision"]); ok {
		vision = v
	}
	return map[string]any{"supports_vision": vision, "context_length": ctx}, nil
}

// Reports whether every character of a non empty string is a Python digit
func allDigits(s string) bool {
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// Items Python iteration yields for a config value, falsy values empty, a non iterable an error
func pyIterate(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		var out []any
		for _, r := range pystr.Runes(x) {
			out = append(out, pystr.FromRunes([]rune{r}))
		}
		return out, nil
	}
	if m, ok := asDict(v); ok {
		out := make([]any, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// json.dumps with sorted keys and str for values JSON cannot hold
func dumpsSorted(v any) (string, error) {
	return pyjson.Dumps(v, pyjson.SortKeys(true), pyjson.Default(func(x any) (any, error) { return pyrepr.Str(x), nil }))
}

// Config axes of the capability surface, stopping at the first failure as the Python try block does
func configSurface(resolved string, surface map[string]any) error {
	tree, err := config.LoadReadonly(context.Background(), resolved, config.NewSecrets(resolved).Lookup())
	if err != nil {
		return err
	}
	cfg := map[string]any(tree)
	skillsCfg, _ := asDict(cfg["skills"])
	modelCfg, _ := asDict(cfg["model"])
	caps, err := modelPromptCapabilitySurface(modelCfg)
	if err != nil {
		return err
	}
	surface["model_capabilities"] = caps
	disabledRaw, err := pyIterate(skillsCfg["disabled"])
	if err != nil {
		return err
	}
	disabled := make([]string, 0, len(disabledRaw))
	for _, s := range disabledRaw {
		disabled = append(disabled, pystr.Lower(pyrepr.Str(s)))
	}
	sort.Strings(disabled)
	surface["disabled_skills"] = disabled
	agentCfg, _ := asDict(cfg["agent"])
	var platformToolsets any = map[string]any{}
	if pyrepr.Truthy(cfg["platform_toolsets"]) {
		platformToolsets = cfg["platform_toolsets"]
	}
	dumped, err := dumpsSorted(platformToolsets)
	if err != nil {
		return err
	}
	surface["platform_toolsets"] = dumped
	disabledToolsets := append([]string{}, tools.ParseConfigStringList(agentCfg["disabled_toolsets"])...)
	sort.Strings(disabledToolsets)
	surface["disabled_toolsets"] = disabledToolsets
	surface["mcp"] = ""
	if mcp, ok := asDict(cfg["mcp_servers"]); ok {
		dumped, err := dumpsSorted(mcp)
		if err != nil {
			return err
		}
		surface["mcp"] = dumped
	}
	return nil
}

// Digest of SOUL.md, empty when it is not a file or unreadable
func soulDigest(resolved string) string {
	soul := pypath.PathJoin(resolved, "SOUL.md")
	if st, err := os.Stat(soul); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(soul)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Sorted skill dirs relative to the skills root through the shared skill index walk
func skillNames(resolved string) []string {
	root := pypath.PathJoin(resolved, "skills")
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return []string{}
	}
	out := []string{}
	for _, p := range skills.IterSkillIndexFiles(root, "SKILL.md") {
		rel, ok := pypath.PathRelativeTo(pypath.PathParent(p), root)
		if !ok {
			return []string{}
		}
		out = append(out, filepath.FromSlash(rel))
	}
	sort.Strings(out)
	return out
}

// CapabilityFingerprint is the 12 hex digest of the capability surface of a profile, unavailable on failure
func CapabilityFingerprint(h string) string {
	resolved := resolveHome(h)
	root := InstallRoot(resolved)
	surface := map[string]any{}
	_ = configSurface(resolved, surface)
	surface["soul"] = soulDigest(resolved)
	surface["skills"] = skillNames(resolved)
	if roster, err := rosterFn(root); err == nil {
		managed := []string{}
		roles := []string{}
		for _, p := range roster {
			if isBotManaged(p.Dir) {
				managed = append(managed, p.Name)
			}
			roles = append(roles, p.Name+":"+profileRole(p.Dir))
		}
		sort.Strings(managed)
		sort.Strings(roles)
		surface["roster"] = managed
		surface["roster_roles"] = roles
	} else {
		surface["roster"] = []string{}
	}
	surface["protocol_version"] = 2
	peerNames := peers(root)
	if peerNames == nil {
		peerNames = []string{}
	}
	surface["peers"] = peerNames
	remote := []string{}
	for _, r := range ReadRemoteRoster(resolved) {
		remote = append(remote, r.ConnectionID+":"+r.Profile+":"+r.Title)
	}
	sort.Strings(remote)
	surface["remote_roster"] = remote
	text, err := pyjson.Dumps(surface, pyjson.SortKeys(true))
	if err != nil {
		return "unavailable"
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12]
}

// EpochLine is the epoch stamp appended to a Bot Chat prompt
func EpochLine(h string) string {
	return epochPrefix + CapabilityFingerprint(h)
}

// StoredPromptCapabilityStale is true when a stamped prompt's epoch no longer matches disk, unstamped never stale
func StoredPromptCapabilityStale(storedPrompt, h string) bool {
	m := epochRe.FindStringSubmatch(storedPrompt)
	if m == nil {
		return false
	}
	current := CapabilityFingerprint(h)
	return current != "unavailable" && m[1] != current
}

// StoredBotChatPromptNeedsUpgrade is true for an unstamped Bot Chat prompt when the probe would emit a section
func StoredBotChatPromptNeedsUpgrade(storedPrompt, h string) bool {
	if strings.Contains(storedPrompt, epochPrefix) {
		return false
	}
	return ProtocolSection(h, false) != ""
}
