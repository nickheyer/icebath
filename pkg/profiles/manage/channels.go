package manage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// GatewayPolicyID is the pseudo platform owning gateway wide allowlist and allow all policy keys
const GatewayPolicyID = "gateway"

// ChannelPlatformEntry is a gateway platform registry entry as the channel inventory reads it
type ChannelPlatformEntry struct {
	Name                 string
	RequiredEnv          []string
	AllowedUsersEnv      string
	AllowAllEnv          string
	CronDeliverEnvVar    string
	EnvEnableCredentials []string
	EnvOverrides         []ChannelEnvStep
	SharedEnvPrefixes    []string
}

// ChannelEnvStep is one env override step, an EnvCred row or a partial of the generic env helper
type ChannelEnvStep struct {
	Cred    *ChannelEnvCred
	Partial *ChannelEnvPartial
}

// ChannelEnvCred is a gateway EnvCred row, env specs being a name or a nested list of names
type ChannelEnvCred struct {
	Platform            string
	Creds               []any
	Token               string
	FixedEnv            []any
	OptionalEnv         []any
	OptionalStrippedEnv []any
	WarnMissing         []string
	Home                string
}

// ChannelEnvPartial holds the env, env_base and platform keywords of a partial env step, empty when absent
type ChannelEnvPartial struct {
	Env      string
	EnvBase  string
	Platform string
}

// ChannelEnableCredentials is one row of the core platform enable credential table
type ChannelEnableCredentials struct {
	Platform string
	Names    []string
}

var platformEnvPrefixAliases = map[string][]string{
	"email": {"EMAIL_"},
	"qqbot": {"QQ_"},
	"relay": {"GATEWAY_RELAY_"},
	"sms":   {"TWILIO_"},
}

var gatewayPolicyKeys = []string{"GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"}

var sharedWithToolsBase = []sharedPrefixes{{"sms", []string{"TWILIO_"}}, {"email", []string{"EMAIL_"}}}

var policyMarkers = []string{"_ALLOWED_USERS", "_ALLOW_ALL_USERS", "_ALLOWED_CHATS", "_HOME_CHANNEL", "_HOME_ADDRESS"}

var gatewayOwnerKeys = []string{"multiplex_profiles", "profile_routes"}

var credentialSuffixes = []string{
	"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_APP_ID", "_CLIENT_ID", "_BOT_ID", "_ACCOUNT_SID",
	"_SERVICE_ACCOUNT_JSON", "_PROJECT_ID",
}

const pyWS = `[\t\n\x0B\f\r\x1C-\x1F \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`

var envLineRe = regexp.MustCompile(`^` + pyWS + `*(?:export` + pyWS + `+)?([A-Za-z_][A-Za-z0-9_]*)` + pyWS + `*=`)

// Clone channel strip and served profile record, replaceable by tests observing a clone
var (
	stripChannelSettingsFn = StripChannelSettings
	recordedServedProfiles = gatewayRecordedServedProfiles
)

type sharedPrefixes struct {
	pid      string
	prefixes []string
}

// Installs the core env override table as the tools secret policy source
func init() {
	tools.ConfigEnvTableKeys = func() []string { return ConfigEnvTableKeys(nil).Keys() }
}

// Ordered env key to platform mapping with Python dict update semantics
type envOwners struct {
	keys []string
	pid  map[string]string
}

func newEnvOwners() *envOwners { return &envOwners{pid: map[string]string{}} }

// Assigns key to pid keeping the first insertion position
func (o *envOwners) set(key, pid string) {
	if _, ok := o.pid[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.pid[key] = pid
}

// Assigns every name to pid in order
func (o *envOwners) setAll(names []string, pid string) {
	for _, n := range names {
		o.set(n, pid)
	}
}

// Keys in insertion order
func (o *envOwners) Keys() []string { return append([]string(nil), o.keys...) }

// Platform owning key and whether it is present
func (o *envOwners) Get(key string) (string, bool) {
	v, ok := o.pid[key]
	return v, ok
}

// Context evaluating adapter discovery in the source profile plugin scope, the ambient scope when empty
func pluginScope(ctx context.Context, sourceDir string) context.Context {
	if sourceDir == "" {
		return ctx
	}
	return home.WithOverride(ctx, sourceDir)
}

// Env prefixes owned by one messaging platform, the canonical upper case one first then its aliases
func PlatformEnvPrefixes(platformID string) []string {
	canonical := strings.ReplaceAll(pystr.Upper(platformID), "-", "_") + "_"
	return append([]string{canonical}, platformEnvPrefixAliases[platformID]...)
}

// Registry entries after plugin discovery in ctx scope, empty when the registry cannot be read
func registryEntries(ctx context.Context) []ChannelPlatformEntry {
	plugins.DiscoverPlugins(ctx, false)
	entries, err := gatewayPlatformEntries(ctx)
	if err != nil {
		return nil
	}
	return entries
}

// Left core rows that still own a messaging platform
func leftCorePlatforms() []provider.LeftCoreFeature {
	var out []provider.LeftCoreFeature
	for _, f := range provider.LeftCore() {
		if f.Platform != "" {
			out = append(out, f)
		}
	}
	return out
}

// Shared with tools prefixes of the base table, left core platforms and plugin platforms in the source scope
func sharedWithTools(ctx context.Context, sourceDir string) []sharedPrefixes {
	shared := append([]sharedPrefixes(nil), sharedWithToolsBase...)
	put := func(pid string, prefixes []string) {
		for i := range shared {
			if shared[i].pid == pid {
				shared[i].prefixes = prefixes
				return
			}
		}
		shared = append(shared, sharedPrefixes{pid, prefixes})
	}
	for _, f := range leftCorePlatforms() {
		if len(f.ChannelEnvPrefixes) > 0 {
			put(f.Platform, f.ChannelEnvPrefixes)
		}
	}
	for _, entry := range registryEntries(pluginScope(ctx, sourceDir)) {
		if len(entry.SharedEnvPrefixes) > 0 {
			put(entry.Name, entry.SharedEnvPrefixes)
		}
	}
	return shared
}

// Prefixes a platform shares with tools, nil when it shares none
func sharedFor(shared []sharedPrefixes, pid string) []string {
	for _, s := range shared {
		if s.pid == pid {
			return s.prefixes
		}
	}
	return nil
}

// Sorted messaging platform ids from core members, left core rows and registry entries in the source scope
func PlatformIDs(ctx context.Context, sourceDir string) []string {
	ids := map[string]bool{}
	for _, v := range gatewayPlatformMembers() {
		if v != "local" {
			ids[v] = true
		}
	}
	for _, f := range leftCorePlatforms() {
		ids[f.Platform] = true
	}
	for _, entry := range registryEntries(pluginScope(ctx, sourceDir)) {
		ids[entry.Name] = true
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Adds every name of a name or nested name list spec
func flattenEnvSpec(spec any, add func(string)) {
	switch v := spec.(type) {
	case string:
		add(v)
	case []any:
		for _, item := range v {
			flattenEnvSpec(item, add)
		}
	case []string:
		for _, item := range v {
			add(item)
		}
	}
}

// Every env name an EnvCred row reads
func credRowEnvs(row *ChannelEnvCred) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	flattenEnvSpec(row.Creds, add)
	if row.Token != "" {
		add(row.Token)
	}
	for _, group := range [][]any{row.FixedEnv, row.OptionalEnv, row.OptionalStrippedEnv} {
		for _, spec := range group {
			flattenEnvSpec(spec, add)
		}
	}
	if len(row.WarnMissing) > 0 {
		add(row.WarnMissing[0])
	}
	if row.Home != "" {
		add(row.Home)
		add(row.Home + "_NAME")
		add(row.Home + "_THREAD_ID")
	}
	sort.Strings(names)
	return names
}

// Env keys the gateway env overrides read outright mapped to their platform, core rows only for nil entries
func ConfigEnvTableKeys(entries []ChannelPlatformEntry) *envOwners {
	keys := newEnvOwners()
	for _, row := range gatewayEnvEnableCredentials() {
		keys.setAll(row.Names, row.Platform)
	}
	for _, entry := range entries {
		keys.setAll(entry.EnvEnableCredentials, entry.Name)
		for _, step := range entry.EnvOverrides {
			if step.Cred != nil {
				keys.setAll(credRowEnvs(step.Cred), step.Cred.Platform)
			}
		}
	}
	return keys
}

// Every env override step the entries declare
func envOverrideSteps(entries []ChannelPlatformEntry) []ChannelEnvStep {
	var out []ChannelEnvStep
	for _, entry := range entries {
		out = append(out, entry.EnvOverrides...)
	}
	return out
}

// Env names adapters declare outright plus gateway policy, mapped to the owning platform
func DeclaredChannelEnvKeys(ctx context.Context, sourceDir string) *envOwners {
	keys := newEnvOwners()
	keys.setAll(gatewayPolicyKeys, GatewayPolicyID)
	for _, f := range leftCorePlatforms() {
		keys.setAll(f.EnableEnv, f.Platform)
	}
	entries := registryEntries(pluginScope(ctx, sourceDir))
	for _, entry := range entries {
		names := append(append([]string(nil), entry.RequiredEnv...), entry.AllowedUsersEnv, entry.AllowAllEnv, entry.CronDeliverEnvVar)
		for _, name := range names {
			if name != "" {
				keys.set(name, entry.Name)
			}
		}
	}
	table := ConfigEnvTableKeys(entries)
	for _, k := range table.keys {
		keys.set(k, table.pid[k])
	}
	for _, step := range envOverrideSteps(entries) {
		if p := step.Partial; p != nil && p.Platform != "" {
			for _, name := range []string{p.Env, p.EnvBase} {
				if name != "" {
					keys.set(name, p.Platform)
				}
			}
		}
	}
	return keys
}

// Allowlist, allow all and home channel names adapters declare, channel only by nature
func policyEnvKeys(ctx context.Context, sourceDir string) map[string]bool {
	names := map[string]bool{}
	for _, k := range gatewayPolicyKeys {
		names[k] = true
	}
	entries := registryEntries(pluginScope(ctx, sourceDir))
	for _, entry := range entries {
		for _, n := range []string{entry.AllowedUsersEnv, entry.AllowAllEnv, entry.CronDeliverEnvVar} {
			if n != "" {
				names[n] = true
			}
		}
	}
	for _, step := range envOverrideSteps(entries) {
		base := ""
		if step.Cred != nil {
			base = step.Cred.Home
		} else if step.Partial != nil {
			base = step.Partial.EnvBase
		}
		if base != "" {
			names[base], names[base+"_NAME"], names[base+"_THREAD_ID"] = true, true, true
		}
	}
	return names
}

// Keys that make an adapter connect as a bot mapped to their platform, enable flags and URLs excluded
func CredentialEnvKeys(ctx context.Context) *envOwners {
	keys := newEnvOwners()
	for _, f := range leftCorePlatforms() {
		keys.setAll(f.EnableEnv, f.Platform)
	}
	entries := registryEntries(ctx)
	for _, entry := range entries {
		keys.setAll(entry.RequiredEnv, entry.Name)
	}
	for _, row := range gatewayEnvEnableCredentials() {
		keys.setAll(row.Names, row.Platform)
	}
	for _, entry := range entries {
		keys.setAll(entry.EnvEnableCredentials, entry.Name)
	}
	for _, step := range envOverrideSteps(entries) {
		if step.Cred == nil {
			continue
		}
		seen := map[string]bool{}
		var creds []string
		add := func(n string) {
			if !seen[n] {
				seen[n] = true
				creds = append(creds, n)
			}
		}
		for _, group := range step.Cred.Creds {
			switch g := group.(type) {
			case string:
				add(g)
			case []any:
				for _, n := range g {
					if s, ok := n.(string); ok {
						add(s)
					}
				}
			case []string:
				for _, n := range g {
					add(n)
				}
			}
		}
		if step.Cred.Token != "" {
			add(step.Cred.Token)
		}
		sort.Strings(creds)
		keys.setAll(creds, step.Cred.Platform)
	}
	out := newEnvOwners()
	for _, k := range keys.keys {
		if hasAnySuffix(k, credentialSuffixes) {
			out.set(k, keys.pid[k])
		}
	}
	return out
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Reports whether p is a regular file following symlinks
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Non blank stripped assignments of a dotenv file, restricted to wanted keys when given
func envValues(envPath string, wanted *envOwners) map[string]string {
	values := map[string]string{}
	if !isFile(envPath) {
		return values
	}
	parsed, err := provider.DotenvSetValues(envPath)
	if err != nil {
		return values
	}
	for key, value := range parsed {
		if wanted != nil {
			if _, ok := wanted.pid[key]; !ok {
				continue
			}
		}
		if v := pystr.Strip(value); v != "" {
			values[key] = v
		}
	}
	return values
}

// Mapping value at key when it is a mapping, nil otherwise
func mapAt(raw map[string]any, key string) map[string]any {
	m, _ := raw[key].(map[string]any)
	return m
}

// Enabled flag of platforms.pid in either nesting when the config states it
func explicitEnabled(raw map[string]any, pid string) *bool {
	gateway := mapAt(raw, "gateway")
	for _, section := range []any{raw["platforms"], gateway["platforms"]} {
		sec, ok := section.(map[string]any)
		if !ok {
			continue
		}
		block, ok := sec[pid].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := block["enabled"]; ok {
			b := pyrepr.Truthy(v)
			return &b
		}
	}
	return nil
}

// Shared prefix platforms the source runs as a channel, explicitly enabled or auto enabled by a full credential set
func sharedAdaptersActive(ctx context.Context, sourceDir string, shared []sharedPrefixes) (map[string]bool, error) {
	active := map[string]bool{}
	if sourceDir == "" {
		for _, s := range shared {
			active[s.pid] = true
		}
		return active, nil
	}
	raw := map[string]any{}
	configPath := filepath.Join(sourceDir, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		return nil, err
	}
	if exists {
		if r, err := config.ReadUserConfigRaw(ctx, configPath); err == nil && r != nil {
			raw = r
		}
	}
	env := envValues(filepath.Join(sourceDir, ".env"), nil)
	creds := map[string][]string{}
	setDefault := func(pid string, names []string) {
		if _, ok := creds[pid]; !ok {
			creds[pid] = names
		}
	}
	for _, row := range gatewayEnvEnableCredentials() {
		creds[row.Platform] = row.Names
	}
	entries := registryEntries(pluginScope(ctx, sourceDir))
	for _, entry := range entries {
		if len(entry.EnvEnableCredentials) > 0 {
			setDefault(entry.Name, entry.EnvEnableCredentials)
		}
	}
	for _, f := range leftCorePlatforms() {
		setDefault(f.Platform, f.EnableEnv)
	}
	for _, entry := range entries {
		setDefault(entry.Name, entry.RequiredEnv)
	}
	for _, s := range shared {
		if explicit := explicitEnabled(raw, s.pid); explicit != nil {
			if *explicit {
				active[s.pid] = true
			}
			continue
		}
		names := creds[s.pid]
		if len(names) == 0 {
			continue
		}
		all := true
		for _, n := range names {
			if env[n] == "" {
				all = false
				break
			}
		}
		if all {
			active[s.pid] = true
		}
	}
	return active, nil
}

type prefixOwner struct{ prefix, pid string }

// ChannelKeyIndex resolves an env key to the messaging platform owning it in the source plugin scope
type ChannelKeyIndex struct {
	Platforms     []string
	declared      *envOwners
	policy        map[string]bool
	shared        []sharedPrefixes
	sharedActive  map[string]bool
	prefixesOrder []prefixOwner
}

// Builds the ownership index of sourceDir, the ambient scope with every shared adapter active when empty
func NewChannelKeyIndex(ctx context.Context, sourceDir string) (*ChannelKeyIndex, error) {
	ix := &ChannelKeyIndex{}
	ix.Platforms = PlatformIDs(ctx, sourceDir)
	ix.declared = DeclaredChannelEnvKeys(ctx, sourceDir)
	ix.policy = policyEnvKeys(ctx, sourceDir)
	ix.shared = sharedWithTools(ctx, sourceDir)
	active, err := sharedAdaptersActive(ctx, sourceDir, ix.shared)
	if err != nil {
		return nil, err
	}
	ix.sharedActive = active
	for _, pid := range ix.Platforms {
		seen := map[string]bool{}
		for _, prefix := range append(PlatformEnvPrefixes(pid), sharedFor(ix.shared, pid)...) {
			if !seen[prefix] {
				seen[prefix] = true
				ix.prefixesOrder = append(ix.prefixesOrder, prefixOwner{prefix, pid})
			}
		}
	}
	sort.SliceStable(ix.prefixesOrder, func(i, j int) bool {
		return len(ix.prefixesOrder[i].prefix) > len(ix.prefixesOrder[j].prefix)
	})
	return ix, nil
}

// Platform owning key, empty for a non channel key or a tool credential of an adapter the source never ran
func (ix *ChannelKeyIndex) PlatformFor(key string) string {
	pid := ix.declared.pid[key]
	if pid == "" {
		for _, po := range ix.prefixesOrder {
			if strings.HasPrefix(key, po.prefix) {
				pid = po.pid
				break
			}
		}
	}
	if pid == "" {
		return ""
	}
	if shared := sharedFor(ix.shared, pid); len(shared) > 0 && hasAnyPrefix(key, shared) && !ix.sharedActive[pid] && !ix.isPolicy(key) {
		return ""
	}
	return pid
}

func (ix *ChannelKeyIndex) isPolicy(key string) bool {
	if ix.policy[key] {
		return true
	}
	for _, m := range policyMarkers {
		if strings.Contains(key, m) {
			return true
		}
	}
	return false
}

// Assigned name of a dotenv line, empty when the line assigns nothing
func envKeyOfLine(line string) string {
	if m := envLineRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// Text of a file read as utf-8-sig with replacement and universal newlines
func readSigText(p string) (string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	return readTextNewlines(pystr.DecodeUTF8Replace(raw)), nil
}

// Index of the ambient scope when ix is nil
func orDefaultIndex(ctx context.Context, ix *ChannelKeyIndex) (*ChannelKeyIndex, error) {
	if ix != nil {
		return ix, nil
	}
	return NewChannelKeyIndex(ctx, "")
}

// Drops every messaging channel assignment of a dotenv file in place and returns removed keys by platform
func StripChannelEnvFile(ctx context.Context, envPath string, ix *ChannelKeyIndex) (*pyjson.Object, error) {
	removed := pyjson.NewObject()
	if !isFile(envPath) {
		return removed, nil
	}
	ix, err := orDefaultIndex(ctx, ix)
	if err != nil {
		return nil, err
	}
	text, err := readSigText(envPath)
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, line := range pystr.Splitlines(text, false) {
		key := envKeyOfLine(line)
		platform := ""
		if key != "" {
			platform = ix.PlatformFor(key)
		}
		if platform == "" {
			kept = append(kept, line)
			continue
		}
		prev, _ := removed.Get(platform)
		list, _ := prev.([]any)
		removed.Set(platform, append(list, key))
	}
	if removed.Len() > 0 {
		out := strings.Join(kept, "\n")
		if strings.HasSuffix(text, "\n") || len(kept) > 0 {
			out += "\n"
		}
		if err := os.WriteFile(envPath, []byte(textForWrite(out)), 0o666); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// Config paths holding platform identity, the platform blocks and the multiplexer owner keys in both nestings
func channelConfigPaths(raw map[string]any, platforms []string) [][]string {
	var paths [][]string
	gateway := mapAt(raw, "gateway")
	if _, ok := raw["platforms"]; ok {
		paths = append(paths, []string{"platforms"})
	}
	if _, ok := gateway["platforms"]; ok {
		paths = append(paths, []string{"gateway", "platforms"})
	}
	for _, key := range append(append([]string(nil), gatewayOwnerKeys...), platforms...) {
		if _, ok := raw[key]; ok {
			paths = append(paths, []string{key})
		}
		if _, ok := gateway[key]; ok {
			paths = append(paths, []string{"gateway", key})
		}
	}
	return paths
}

// Removes platform sections from a raw config.yaml in place and returns the dotted paths removed
func StripChannelConfig(ctx context.Context, configPath string, ix *ChannelKeyIndex) ([]string, error) {
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return nil, err
	}
	if ix, err = orDefaultIndex(ctx, ix); err != nil {
		return nil, err
	}
	raw, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return nil, err
	}
	paths := channelConfigPaths(raw, ix.Platforms)
	if len(paths) == 0 {
		return nil, nil
	}
	dotted := make([]string, len(paths))
	for i, path := range paths {
		node := raw
		for _, seg := range path[:len(path)-1] {
			node = node[seg].(map[string]any)
		}
		delete(node, path[len(path)-1])
		dotted[i] = strings.Join(path, ".")
	}
	if gw, ok := raw["gateway"].(map[string]any); ok && len(gw) == 0 {
		delete(raw, "gateway")
	}
	if err := config.AtomicConfigReplace(ctx, filepath.Dir(configPath), configPath, raw, ""); err != nil {
		return nil, err
	}
	return dotted, nil
}

var fixedChannelState = map[string]bool{
	"platforms": true, "pairing": true, "whatsapp": true, "gateway": true,
	"channel_directory.json": true, "channel_aliases.json": true,
}

// Root entries of a profile holding per bot runtime identity, sorted like Path objects
func ChannelStateEntries(ctx context.Context, root string, ix *ChannelKeyIndex) ([]string, error) {
	if !isDir(root) {
		return nil, nil
	}
	ix, err := orDefaultIndex(ctx, ix)
	if err != nil {
		return nil, err
	}
	platforms := ix.Platforms
	dirents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	prefixes := make([]string, len(platforms))
	for i, pid := range platforms {
		prefixes[i] = pid + "_"
	}
	var out []string
	for _, d := range dirents {
		if fixedChannelState[d.Name()] || hasAnyPrefix(d.Name(), prefixes) {
			out = append(out, d.Name())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return pathNameLess(out[i], out[j]) })
	for i, name := range out {
		out[i] = filepath.Join(root, name)
	}
	return out, nil
}

// Orders names as pathlib compares sibling paths, case folded on Windows
func pathNameLess(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.ToLower(a) < strings.ToLower(b)
	}
	return a < b
}

// Removes a state entry without following symlinks, a tree removal ignoring errors
func removeEntry(entry string) error {
	st, err := os.Lstat(entry)
	if err == nil && st.IsDir() {
		os.RemoveAll(entry)
		return nil
	}
	if err := os.Remove(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Strips channel credentials and identity from a fresh clone judged in the source plugin scope
func StripChannelSettings(ctx context.Context, profileDir string, includeState bool, sourceDir string) (*pyjson.Object, error) {
	ix, err := NewChannelKeyIndex(ctx, sourceDir)
	if err != nil {
		return nil, err
	}
	stripped, err := StripChannelEnvFile(ctx, filepath.Join(profileDir, ".env"), ix)
	if err != nil {
		return nil, err
	}
	configPaths, err := StripChannelConfig(ctx, filepath.Join(profileDir, "config.yaml"), ix)
	if err != nil {
		return nil, err
	}
	if len(configPaths) > 0 {
		stripped.Set("config", stringsAny(configPaths))
	}
	if includeState {
		entries, err := ChannelStateEntries(ctx, profileDir, ix)
		if err != nil {
			return nil, err
		}
		var dropped []string
		for _, entry := range entries {
			if err := removeEntry(entry); err != nil {
				return nil, err
			}
			dropped = append(dropped, filepath.Base(entry))
		}
		if len(dropped) > 0 {
			stripped.Set("state", stringsAny(dropped))
		}
	}
	return stripped, nil
}

func stringsAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Platform ids with any channel setting in a profile, read in its own plugin scope
func ChannelPlatformsConfigured(ctx context.Context, profileDir string) ([]string, error) {
	ix, err := NewChannelKeyIndex(ctx, profileDir)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	envPath := filepath.Join(profileDir, ".env")
	if isFile(envPath) {
		text, err := readSigText(envPath)
		if err != nil {
			return nil, err
		}
		for _, line := range pystr.Splitlines(text, false) {
			if key := envKeyOfLine(line); key != "" {
				if p := ix.PlatformFor(key); p != "" && p != GatewayPolicyID {
					found[p] = true
				}
			}
		}
	}
	configPath := filepath.Join(profileDir, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		return nil, err
	}
	if exists {
		raw, err := config.ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return nil, err
		}
		for _, path := range channelConfigPaths(raw, ix.Platforms) {
			var node any = raw
			for _, seg := range path {
				node = node.(map[string]any)[seg]
			}
			last := path[len(path)-1]
			if m, ok := node.(map[string]any); ok && last == "platforms" {
				for k := range m {
					found[k] = true
				}
			} else if contains(ix.Platforms, last) {
				found[last] = true
			}
		}
	}
	return sortedKeys(found), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Refusal text for cloning channels a live multiplexer already serves, empty when the copy is allowed
func CloneChannelsRefusal(ctx context.Context, sourceDir, sourceLabel string) (string, error) {
	served, _, err := recordedServedProfiles()
	if err != nil || len(served) < 2 {
		return "", err
	}
	label, err := profiles.NormalizeProfileName(sourceLabel)
	if err != nil {
		return "", err
	}
	match := false
	for _, p := range served {
		canon, err := profiles.NormalizeProfileName(p)
		if err != nil {
			return "", err
		}
		match = match || canon == label
	}
	if !match {
		return "", nil
	}
	platforms, err := ChannelPlatformsConfigured(ctx, sourceDir)
	if err != nil || len(platforms) == 0 {
		return "", err
	}
	return fmt.Sprintf("--clone-channels would copy %s from '%s', which the running "+
		"multiplexed gateway already serves: the bot can only belong to one profile, so the copy would be "+
		"parked as a duplicate credential. Clone without --clone-channels and give the new profile its own bot "+
		"(icebath -p <name> setup), or route its chats with gateway.profile_routes instead.",
		strings.Join(platforms, ", "), sourceLabel), nil
}

// Platform tokens of platforms.p.token or api_key in both nestings
func configPlatformTokens(ctx context.Context, configPath string) (map[string]string, error) {
	tokens := map[string]string{}
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return tokens, err
	}
	raw, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return nil, err
	}
	gateway := mapAt(raw, "gateway")
	for _, section := range []any{raw["platforms"], gateway["platforms"]} {
		sec, ok := section.(map[string]any)
		if !ok {
			continue
		}
		for pid, block := range sec {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			token := b["token"]
			if !pyrepr.Truthy(token) {
				token = b["api_key"]
			}
			if s, ok := token.(string); ok && pystr.Strip(s) != "" {
				tokens[pid] = pystr.Strip(s)
			}
		}
	}
	return tokens, nil
}

// Platforms whose connecting credential in a profile is byte identical to the source profile one
func SharedChannelCredentials(ctx context.Context, profileDir, sourceDir string) ([]string, error) {
	wanted := CredentialEnvKeys(ctx)
	mine := envValues(filepath.Join(profileDir, ".env"), wanted)
	theirs := envValues(filepath.Join(sourceDir, ".env"), wanted)
	shared := map[string]bool{}
	for key, value := range mine {
		if t, ok := theirs[key]; ok && t == value {
			pid, _ := wanted.Get(key)
			shared[pid] = true
		}
	}
	mineCfg, err := configPlatformTokens(ctx, filepath.Join(profileDir, "config.yaml"))
	if err != nil {
		return nil, err
	}
	theirsCfg, err := configPlatformTokens(ctx, filepath.Join(sourceDir, "config.yaml"))
	if err != nil {
		return nil, err
	}
	for pid, token := range mineCfg {
		if t, ok := theirsCfg[pid]; ok && t == token {
			shared[pid] = true
		}
	}
	return sortedKeys(shared), nil
}

// Warning that a profile shares a bot credential with its source
func SharedCredentialWarning(profile string, platforms []string, source string) string {
	return fmt.Sprintf("⚠ Profile '%s' shares its %s credential with %s: the bot can "+
		"only belong to one profile. Give '%s' its own bot (icebath -p %s setup, or the "+
		"dashboard Messaging page) or remove the token from '%s'; a multiplexed gateway parks "+
		"the duplicate and `icebath gateway migrate --multiplex` refuses until it is gone.",
		profile, strings.Join(platforms, ", "), source, profile, profile, profile)
}

// Lines printed after a channel less clone naming what stayed behind and how to configure new bots
func FormatStrippedNotice(profile string, platforms []string, cloneFlag string) []string {
	if len(platforms) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("Messaging channels were NOT cloned (%s): a copied bot token or allowlist "+
			"would make two gateways fight over one bot.", strings.Join(platforms, ", ")),
		fmt.Sprintf("  Configure this profile's own bots:  icebath -p %s setup   (or the dashboard Messaging page)", profile),
		fmt.Sprintf("  To copy the source's channels anyway:  icebath profile create %s %s --clone-channels", profile, cloneFlag),
	}
}
