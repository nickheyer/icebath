package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Skill slash command table keyed by "/slug" with name, description, skill_md_path and skill_dir
type SkillCommands = map[string]map[string]any

// Frontmatter parser of the command scan, swapped by tests to observe scan interleaving
var commandFrontmatter = ParseFrontmatter

var scanSkipParts = map[string]bool{".git": true, ".github": true, ".hub": true, ".archive": true, ".locks": true}

const (
	maxWatchedScopes = 32
	maxCachedScopes  = 2 * maxWatchedScopes
	maxStackedSkills = 5
)

var (
	coreCommandMu       sync.RWMutex
	coreCommandResolver func(name string) bool
)

// Installs the core slash command lookup, pkg/commands registers its Resolve at init
func RegisterCoreCommandResolver(fn func(name string) bool) {
	coreCommandMu.Lock()
	coreCommandResolver = fn
	coreCommandMu.Unlock()
}

// Reports whether a core command or alias owns the name, panicking when pkg/commands never registered
func coreCommandExists(name string) bool {
	coreCommandMu.RLock()
	fn := coreCommandResolver
	coreCommandMu.RUnlock()
	if fn == nil {
		panic("skills: core command resolver is not registered, pkg/commands must call RegisterCoreCommandResolver at init")
	}
	return fn(name)
}

// Platform, home and project root a skill command map is scanned for
type skillCommandScope struct{ platform, home, project string }

type pluginCommandsTag struct {
	home  string
	names []string
}

func (t *pluginCommandsTag) equal(o pluginCommandsTag) bool {
	return t != nil && t.home == o.home && strings.Join(t.names, "\x00") == strings.Join(o.names, "\x00") && len(t.names) == len(o.names)
}

var skillCmds = struct {
	mu              sync.Mutex
	byKey           map[skillCommandScope]SkillCommands
	sigByKey        map[skillCommandScope]string
	lastInteractive map[skillCommandScope]SkillCommands
	recency         []skillCommandScope
	plugin          SkillCommands
	pluginTag       *pluginCommandsTag
}{
	byKey:           map[skillCommandScope]SkillCommands{},
	sigByKey:        map[skillCommandScope]string{},
	lastInteractive: map[skillCommandScope]SkillCommands{},
	plugin:          SkillCommands{},
}

func (s *Scope) commandScopeKey() skillCommandScope {
	return skillCommandScope{s.Platform, s.Home, findProjectRoot(s.Cwd)}
}

// Note shown when the slug of a skill name is a core command or alias, false otherwise
func SkillCommandCollisionNote(name string) (string, bool) {
	cmd := SlugifySkillName(name)
	if cmd == "" || !coreCommandExists(cmd) {
		return "", false
	}
	return fmt.Sprintf("slash command /%s unavailable — name taken by built-in; use /skill %s", cmd, name), true
}

// Marks a scope most recently used storing value in table when set, evicting the oldest past the cap
func publishScope(table map[skillCommandScope]SkillCommands, key skillCommandScope, value SkillCommands) {
	if table != nil {
		table[key] = value
	}
	for i, k := range skillCmds.recency {
		if k == key {
			skillCmds.recency = append(skillCmds.recency[:i:i], skillCmds.recency[i+1:]...)
			break
		}
	}
	skillCmds.recency = append(skillCmds.recency, key)
	for len(skillCmds.recency) > maxCachedScopes {
		stale := skillCmds.recency[0]
		skillCmds.recency = skillCmds.recency[1:]
		delete(skillCmds.byKey, stale)
		delete(skillCmds.sigByKey, stale)
		delete(skillCmds.lastInteractive, stale)
	}
}

func copyCommands(c SkillCommands) SkillCommands {
	out := make(SkillCommands, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// Text of a SKILL.md decoded as utf-8-sig, a UnicodeDecodeError on invalid bytes
func readSkillMDStrict(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	if i, ok := pystr.ValidIndex(data); !ok {
		return "", pystr.UTF8DecodeError(data, i)
	}
	return string(data), nil
}

// First non heading body line clipped to 80 characters, empty when none
func firstBodyLine(body string) string {
	for _, line := range strings.Split(pystr.Strip(body), "\n") {
		if l := pystr.Strip(line); l != "" && !strings.HasPrefix(l, "#") {
			return runeHead(l, 80)
		}
	}
	return ""
}

// Registers one SKILL.md in commands unless filtered, disabled, already seen or colliding
func (s *Scope) scanSkillMD(skillMD string, disabled, seen map[string]bool, commands SkillCommands) error {
	_, parts := pypath.PathParts(skillMD)
	for _, p := range parts {
		if scanSkipParts[p] {
			return nil
		}
	}
	text, err := readSkillMDStrict(skillMD)
	if err != nil {
		return err
	}
	fm, body := commandFrontmatter(text)
	if !skillMatchesPlatform(fm) || !s.skillMatchesEnvironment(fm) || !skillMatchesApps(fm) {
		return nil
	}
	nameV, ok := fm["name"]
	if !ok {
		nameV = pypath.PathName(pypath.PathParent(skillMD))
	}
	name, isStr := nameV.(string)
	if !isStr {
		return fmt.Errorf("'%s' object has no attribute 'lower'", pyrepr.TypeName(nameV))
	}
	if seen[name] || disabled[name] {
		return nil
	}
	description, ok := fm["description"]
	if !ok {
		description = ""
	}
	if !fmTruthy(description) {
		description = firstBodyLine(body)
	}
	seen[name] = true
	cmd := SlugifySkillName(name)
	if cmd == "" {
		return nil
	}
	if _, collides := SkillCommandCollisionNote(name); collides {
		s.logger().Warn(fmt.Sprintf("Skill %s generates slash command '/%s' which collides with a core Icebath command; skipping auto-registration. Use '/skill %s' instead.", pyrepr.Repr(name), cmd, name))
		return nil
	}
	key := "/" + cmd
	if existing, ok := commands[key]; ok {
		s.logger().Warn(fmt.Sprintf("Skill %s maps to slash command %s already claimed by %s; keeping the first and skipping this one.", pyrepr.Repr(name), key, pyrepr.Repr(existing["name"])))
		return nil
	}
	if !fmTruthy(description) {
		description = "Invoke the " + name + " skill"
	}
	md := pypath.PathStr(skillMD)
	commands[key] = map[string]any{"name": name, "description": description, "skill_md_path": md, "skill_dir": pypath.PathParent(md)}
	return nil
}

// Scans the resolved catalog into a fresh command map and publishes it for the scope
func (s *Scope) ScanSkillCommands() SkillCommands {
	if config.SafeWorkerEnabled() {
		return SkillCommands{}
	}
	key := s.commandScopeKey()
	signature, hasSignature := s.skillDirsSignature()
	commands := SkillCommands{}
	disabled := s.DisabledSkillNames("")
	seen := map[string]bool{}
	for _, e := range s.skillCatalog(false, false) {
		if e.Status == "unique" {
			if err := s.scanSkillMD(e.Path, disabled, seen, commands); err != nil {
				continue
			}
		}
	}
	skillCmds.mu.Lock()
	if hasSignature {
		skillCmds.sigByKey[key] = signature
	} else {
		delete(skillCmds.sigByKey, key)
	}
	publishScope(skillCmds.byKey, key, commands)
	skillCmds.mu.Unlock()
	return commands
}

// One watched skill scope and its directory change signature
type PublishedScopeRow struct {
	Home, Project, Signature string
	// Set when the signature could not be computed because a root failed to stat
	NoSignature bool
}

// Scope over another home and project, its config loaded from that home
func (s *Scope) scopeFor(ctx context.Context, h, project string) *Scope {
	sc := *s
	if project == "" {
		project = string(os.PathSeparator)
	}
	sc.Cwd = project
	if h != s.Home {
		sc.Home = h
		sc.SkillsDirOverride = ""
		sc.Secrets = config.NewSecrets(h)
		cfg, err := config.LoadReadonly(ctx, h, sc.Secrets.Lookup())
		if err != nil {
			s.logger().Debug(fmt.Sprintf("skill scope config read failed for %s: %v", h, err))
			cfg = config.Defaults()
		}
		sc.Config = cfg
	}
	return &sc
}

// Directory signature of the ambient scope plus every published home and project scope
func (s *Scope) PublishedScopesSignature(ctx context.Context) []PublishedScopeRow {
	skillCmds.mu.Lock()
	type hp struct{ home, project string }
	set := map[hp]bool{}
	for k := range skillCmds.byKey {
		if k.home != "" {
			set[hp{k.home, k.project}] = true
		}
	}
	skillCmds.mu.Unlock()
	scopes := make([]hp, 0, len(set))
	for k := range set {
		scopes = append(scopes, k)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].home != scopes[j].home {
			return scopes[i].home < scopes[j].home
		}
		return scopes[i].project < scopes[j].project
	})
	if len(scopes) > maxWatchedScopes {
		scopes = scopes[:maxWatchedScopes]
	}
	sig, has := s.skillDirsSignature()
	rows := []PublishedScopeRow{{Signature: sig, NoSignature: !has}}
	for _, sc := range scopes {
		sig, has := s.scopeFor(ctx, sc.home, sc.project).skillDirsSignature()
		rows = append(rows, PublishedScopeRow{Home: sc.home, Project: sc.project, Signature: sig, NoSignature: !has})
	}
	return rows
}

// Change signature of every skill root the catalog walks plus the disabled set and platform
func (s *Scope) skillDirsSignature() (string, bool) {
	roots, _ := s.skillSearchDirs()
	dirs := make([]string, len(roots))
	for i, r := range roots {
		dirs[i] = r.dir
	}
	sig, err := skillDirsSignatureSteps.scan(dirs, s.DisabledSkillNames(""))
	if err == nil {
		return sig, true
	}
	var pathErr *fs.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) || errors.As(err, &errno) {
		s.logger().Debug("Skill dir signature failed", "error", err)
		return "", false
	}
	panic(err)
}

// Scan signature step the freshness tests replace, an OS error meaning no signature
var skillDirsSignatureSteps = struct {
	scan func(dirs []string, disabled map[string]bool) (string, error)
}{scan: func(dirs []string, disabled map[string]bool) (string, error) {
	return skillsScanSignature(dirs, disabled), nil
}}

// Command map of the scope, rescanned when the scope is new or its skill dirs changed
func (s *Scope) GetSkillCommands() SkillCommands {
	if config.SafeWorkerEnabled() {
		return SkillCommands{}
	}
	key := s.commandScopeKey()
	skillCmds.mu.Lock()
	cached, ok := skillCmds.byKey[key]
	scannedAt, hasSig := skillCmds.sigByKey[key]
	if ok {
		publishScope(nil, key, nil)
	}
	skillCmds.mu.Unlock()
	if ok {
		if current, has := s.skillDirsSignature(); !has || !hasSig || current == scannedAt {
			return cached
		}
	}
	return s.ScanSkillCommands()
}

// Drops the cached plugin skill projection
func InvalidatePluginSkillCommands() {
	skillCmds.mu.Lock()
	skillCmds.plugin = SkillCommands{}
	skillCmds.pluginTag = nil
	skillCmds.mu.Unlock()
}

// Enabled plugin skills projected into the interactive slash namespace with disable lists applied each call
func (s *Scope) GetPluginSkillCommands(ctx context.Context) (SkillCommands, error) {
	host, err := s.pluginHost(ctx)
	if err != nil {
		return nil, err
	}
	host.Discover(ctx)
	metadata := host.ListPluginSkillMetadata()
	tag := pluginCommandsTag{home: s.Home}
	for _, m := range metadata {
		tag.names = append(tag.names, pyStrTruthy(m["name"]))
	}
	skillCmds.mu.Lock()
	cached, fresh := skillCmds.plugin, skillCmds.pluginTag.equal(tag)
	skillCmds.mu.Unlock()
	if !fresh {
		cached = s.scanPluginSkillCommands(host, metadata, tag)
	}
	disabled := s.DisabledSkillNames("")
	disabledPlugins := host.DisabledPlugins()
	if len(disabled) == 0 && len(disabledPlugins) == 0 {
		return cached, nil
	}
	out := SkillCommands{}
	for key, info := range cached {
		name, _ := info["name"].(string)
		if disabled[name] || disabled[strings.SplitN(name, ":", 2)[1]] {
			continue
		}
		owned := false
		for _, owner := range host.SkillOwners(name) {
			owned = owned || disabledPlugins[owner]
		}
		if !owned {
			out[key] = info
		}
	}
	return out, nil
}

// Python str(value or "") of a metadata field
func pyStrTruthy(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Builds the plugin skill projection from the files that will load and publishes it with its tag
func (s *Scope) scanPluginSkillCommands(host PluginSkillHost, metadata []map[string]any, tag pluginCommandsTag) SkillCommands {
	commands := SkillCommands{}
	for _, entry := range metadata {
		qualified := pystr.Strip(pyStrTruthy(entry["name"]))
		if !strings.Contains(qualified, ":") {
			continue
		}
		md, ok := host.FindPluginSkill(qualified)
		if !ok {
			continue
		}
		if st, err := os.Stat(md); err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(md)
		if err != nil {
			continue
		}
		parsed, _ := ParseFrontmatter(pystr.DecodeUTF8Replace(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))))
		if !skillMatchesPlatform(parsed) || !s.skillMatchesEnvironment(parsed) || !skillMatchesApps(parsed) {
			continue
		}
		key := "/" + pystr.Lower(qualified)
		_, collides := SkillCommandCollisionNote(qualified)
		if _, taken := commands[key]; collides || taken {
			s.logger().Warn(fmt.Sprintf("Plugin skill %s collides with an existing slash command; skipping", pyrepr.Repr(qualified)))
			continue
		}
		desc := parsed["description"]
		if !pyrepr.Truthy(desc) {
			desc = entry["description"]
		}
		if !pyrepr.Truthy(desc) {
			desc = "Invoke the " + qualified + " plugin skill"
		}
		mdPath := pypath.PathStr(md)
		commands[key] = map[string]any{
			"name": qualified, "description": pystr.Strip(pyrepr.Str(desc)),
			"skill_identifier": qualified, "skill_md_path": mdPath,
			"skill_dir": pypath.PathParent(mdPath), "source": "plugin",
		}
	}
	skillCmds.mu.Lock()
	skillCmds.plugin, skillCmds.pluginTag = commands, &tag
	skillCmds.mu.Unlock()
	return commands
}

// Filesystem commands plus plugin commands, the filesystem side winning collisions
func (s *Scope) mergeInteractiveSkillCommands(filesystem, plugin SkillCommands) SkillCommands {
	commands := copyCommands(filesystem)
	keys := make([]string, 0, len(plugin))
	for k := range plugin {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, cmd := range keys {
		if existing, ok := commands[cmd]; ok {
			s.logger().Warn(fmt.Sprintf("Plugin skill %s collides with %s; keeping the first", pyrepr.Repr(cmd), pyrepr.Repr(existing["name"])))
			continue
		}
		commands[cmd] = plugin[cmd]
	}
	return commands
}

// Filesystem skills plus profile scoped plugin skills for interactive surfaces only
func (s *Scope) GetInteractiveSkillCommands(ctx context.Context) (SkillCommands, error) {
	key := s.commandScopeKey()
	filesystem := s.GetSkillCommands()
	plugin, err := s.GetPluginSkillCommands(ctx)
	if err != nil {
		return nil, err
	}
	commands := s.mergeInteractiveSkillCommands(filesystem, plugin)
	skillCmds.mu.Lock()
	publishScope(skillCmds.lastInteractive, key, copyCommands(commands))
	skillCmds.mu.Unlock()
	return commands, nil
}

// Added, removed, unchanged and total between two name to description snapshots
func DiffCommandSnapshots(before, after map[string]any) map[string]any {
	sortedKeys := func(m map[string]any, without map[string]any, onlyIn bool) []string {
		var out []string
		for k := range m {
			if _, in := without[k]; in == onlyIn {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	added, removed := []map[string]any{}, []map[string]any{}
	for _, n := range sortedKeys(after, before, false) {
		added = append(added, map[string]any{"name": n, "description": after[n]})
	}
	for _, n := range sortedKeys(before, after, false) {
		removed = append(removed, map[string]any{"name": n, "description": before[n]})
	}
	unchanged := sortedKeys(after, before, true)
	if unchanged == nil {
		unchanged = []string{}
	}
	return map[string]any{"added": added, "removed": removed, "unchanged": unchanged, "total": len(after)}
}

// Slug to description snapshot of a command map for diffing
func CommandSnapshot(cmds SkillCommands) map[string]any {
	out := make(map[string]any, len(cmds))
	for key, info := range cmds {
		var desc any = ""
		if info != nil && pyrepr.Truthy(info["description"]) {
			desc = info["description"]
		}
		out[strings.TrimLeft(key, "/")] = desc
	}
	return out
}

// Rescans every skill scope and the plugin projection and diffs the visible command map
func (s *Scope) ReloadSkills(ctx context.Context) (map[string]any, error) {
	key := s.commandScopeKey()
	skillCmds.mu.Lock()
	beforeCommands, ok := skillCmds.lastInteractive[key]
	publishScope(nil, key, nil)
	if !ok {
		beforeCommands = copyCommands(skillCmds.byKey[key])
		if skillCmds.pluginTag != nil && skillCmds.pluginTag.home == key.home {
			for cmd, info := range skillCmds.plugin {
				if _, taken := beforeCommands[cmd]; !taken {
					beforeCommands[cmd] = info
				}
			}
		}
	}
	skillCmds.mu.Unlock()
	before := CommandSnapshot(beforeCommands)
	skillCmds.mu.Lock()
	skillCmds.byKey = map[skillCommandScope]SkillCommands{}
	skillCmds.sigByKey = map[skillCommandScope]string{}
	skillCmds.mu.Unlock()
	ClearSkillsCache()
	InvalidatePluginSkillCommands()
	fresh := s.ScanSkillCommands()
	plugin, err := s.GetPluginSkillCommands(ctx)
	if err != nil {
		return nil, err
	}
	effective := s.mergeInteractiveSkillCommands(fresh, plugin)
	result := DiffCommandSnapshots(before, CommandSnapshot(effective))
	skillCmds.mu.Lock()
	publishScope(skillCmds.lastInteractive, key, effective)
	skillCmds.mu.Unlock()
	result["commands"] = len(effective)
	return result, nil
}

// Command key a typed slash command resolves to, plugin skills included on interactive surfaces
func (s *Scope) ResolveSkillCommandKey(ctx context.Context, command string, interactive bool) (string, bool, error) {
	if !interactive {
		key, ok := ResolveSlashKey(command, s.GetSkillCommands())
		return key, ok, nil
	}
	table, err := s.GetInteractiveSkillCommands(ctx)
	if err != nil {
		return "", false, err
	}
	key, ok := ResolveSlashKey(command, table)
	return key, ok, nil
}

// "/slug" for a command present in table, underscores tried as hyphens, false otherwise
func ResolveSlashKey[V any](command string, table map[string]V) (string, bool) {
	if command == "" {
		return "", false
	}
	exact := "/" + pystr.Lower(command)
	if _, ok := table[exact]; ok {
		return exact, true
	}
	key := "/" + pystr.Lower(strings.ReplaceAll(command, "_", "-"))
	_, ok := table[key]
	return key, ok
}

// Payload of a command entry by its plugin identifier or skill dir
func (s *Scope) loadCommandSkill(ctx context.Context, info map[string]any, taskID string) (loadedSkill, bool) {
	ident := info["skill_identifier"]
	if !pyrepr.Truthy(ident) {
		ident = info["skill_dir"]
	}
	payload, dir, name, ok := s.loadSkillPayload(ctx, pyrepr.Str(ident), taskID)
	return loadedSkill{payload, dir, name}, ok
}

// User message for one skill slash command, false when the command or its skill is missing
func (s *Scope) BuildSkillInvocationMessage(ctx context.Context, cmdKey, userInstruction, taskID, runtimeNote string) (string, bool, error) {
	commands, err := s.GetInteractiveSkillCommands(ctx)
	if err != nil {
		return "", false, err
	}
	info, ok := commands[cmdKey]
	if !ok {
		return "", false, nil
	}
	loaded, ok := s.loadCommandSkill(ctx, info, taskID)
	if !ok {
		return "", false, nil
	}
	note := fmt.Sprintf("[IMPORTANT: The user has invoked the \"%s\" skill, indicating they want you to follow its instructions. The full skill content is loaded below.]", loaded.name)
	return s.renderSkillBlock(ctx, loaded, note, taskID, userInstruction, runtimeNote), true, nil
}

// Further leading skill tokens of rest up to the stack cap, and the instruction that follows them
func (s *Scope) SplitStackedSkillCommands(ctx context.Context, rest string, interactive bool) ([]string, string, error) {
	var keys []string
	remaining := rest
	for len(keys) < maxStackedSkills-1 {
		stripped := pystr.Lstrip(remaining)
		if !strings.HasPrefix(stripped, "/") {
			break
		}
		parts := pystr.SplitWhitespace(stripped, 1)
		tail := ""
		if len(parts) > 1 {
			tail = parts[1]
		}
		key, ok, err := s.ResolveSkillCommandKey(ctx, strings.TrimLeft(parts[0], "/"), interactive)
		if err != nil {
			return nil, "", err
		}
		repeated := false
		for _, k := range keys {
			repeated = repeated || k == key
		}
		if !ok || repeated {
			break
		}
		keys = append(keys, key)
		remaining = tail
	}
	return keys, pystr.Strip(remaining), nil
}

// Message, loaded names and missing names of a stacked invocation, false when no skill loaded
func (s *Scope) BuildStackedSkillInvocationMessage(ctx context.Context, cmdKeys []string, userInstruction, taskID string) (string, []string, []string, bool, error) {
	commands, err := s.GetInteractiveSkillCommands(ctx)
	if err != nil {
		return "", nil, nil, false, err
	}
	var keys []string
	for _, k := range cmdKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	loadedNames, missing, _, blocks := s.loadSkillBlocks(ctx, keys,
		func(cmdKey string) (loadedSkill, bool) {
			info, ok := commands[cmdKey]
			if !ok {
				return loadedSkill{}, false
			}
			return s.loadCommandSkill(ctx, info, taskID)
		},
		func(name string) string {
			return fmt.Sprintf("[Loaded as part of the stacked skill invocation \"%s\".]", name)
		},
		taskID, skillBlockOpts{missingLabel: func(k string) string { return strings.TrimLeft(k, "/") }})
	if len(blocks) == 0 {
		return "", nil, nil, false, nil
	}
	header := scaffoldHeader(fmt.Sprintf("\"%s\" stacked skill bundle", strings.Join(keys, " ")), loadedNames, nil, missing, nil, "", userInstruction)
	return strings.Join(append([]string{header}, blocks...), "\n\n"), loadedNames, missing, true, nil
}

// Operator disabled skill names for a platform, the session platform when empty
func (s *Scope) commandDisabledSkillNames(platform string) map[string]bool {
	return s.DisabledSkillNames(platform)
}

// Options of a multi skill block load
type skillBlockOpts struct {
	missingLabel      func(string) string
	disabledNames     map[string]bool
	disabledAsMissing bool
	alreadyLoaded     map[string]bool
}

// Loads each distinct identifier and renders its block, returning loaded, missing, disabled and blocks
func (s *Scope) loadSkillBlocks(ctx context.Context, identifiers []string, load func(string) (loadedSkill, bool), activationNote func(string) string, taskID string, opts skillBlockOpts) ([]string, []string, []string, []string) {
	var loadedNames, missing, disabled, blocks []string
	seen := map[string]bool{}
	for _, id := range identifiers {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		loaded, ok := load(id)
		if !ok {
			label := id
			if opts.missingLabel != nil {
				label = opts.missingLabel(id)
			}
			missing = append(missing, label)
			continue
		}
		name := loaded.name
		if len(opts.disabledNames) > 0 && (opts.disabledNames[name] || opts.disabledNames[id] ||
			(strings.Contains(name, ":") && opts.disabledNames[strings.SplitN(name, ":", 2)[1]])) {
			if opts.disabledAsMissing {
				missing = append(missing, id)
			} else if name != "" {
				disabled = append(disabled, name)
			} else {
				disabled = append(disabled, id)
			}
			continue
		}
		if opts.alreadyLoaded[name] {
			loadedNames = append(loadedNames, name)
			continue
		}
		blocks = append(blocks, s.renderSkillBlock(ctx, loaded, activationNote(name), taskID, "", ""))
		loadedNames = append(loadedNames, name)
	}
	return loadedNames, missing, disabled, blocks
}

// Session preload prompt, loaded names and missing identifiers, disabled skills counted as missing
func (s *Scope) BuildPreloadedSkillsPrompt(ctx context.Context, identifiers []string, taskID string, excludedLoadedNames map[string]bool) (string, []string, []string) {
	if config.SafeWorkerEnabled() {
		return "", nil, append([]string{}, identifiers...)
	}
	ids := make([]string, len(identifiers))
	for i, raw := range identifiers {
		ids[i] = pystr.Strip(raw)
	}
	loadedNames, missing, _, parts := s.loadSkillBlocks(ctx, ids,
		func(id string) (loadedSkill, bool) {
			payload, dir, name, ok := s.loadSkillPayload(ctx, id, taskID)
			return loadedSkill{payload, dir, name}, ok
		},
		func(name string) string {
			return fmt.Sprintf("[IMPORTANT: The user launched this CLI session with the \"%s\" skill preloaded. Treat its instructions as active guidance for the duration of this session unless the user overrides them.]", name)
		},
		taskID, skillBlockOpts{
			missingLabel:  func(id string) string { return s.missingSkillLabel(ctx, id) },
			disabledNames: s.commandDisabledSkillNames(""), disabledAsMissing: true, alreadyLoaded: excludedLoadedNames,
		})
	return strings.Join(parts, "\n\n"), loadedNames, missing
}

// Deduplicated skills.auto_load names of userConfig, the scope config when nil
func (s *Scope) ResolveAutoLoadSkills(userConfig map[string]any) []string {
	if userConfig == nil {
		userConfig = map[string]any(s.tree())
	}
	block := config.AsMap(userConfig["skills"])
	if block == nil {
		return nil
	}
	var entries []any
	switch v := block["auto_load"].(type) {
	case []any:
		entries = v
	case []string:
		for _, e := range v {
			entries = append(entries, e)
		}
	default:
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, e := range entries {
		if str, ok := e.(string); ok {
			if n := pystr.Strip(str); n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}

// skills.auto_load rendered as loaded skill blocks with loaded and missing names, never failing session start
func (s *Scope) BuildAutoLoadPrompt(ctx context.Context, taskID string, userConfig map[string]any) (string, []string, []string) {
	auto := s.ResolveAutoLoadSkills(userConfig)
	if len(auto) == 0 {
		return "", nil, nil
	}
	loadedNames, missing, _, parts := s.loadSkillBlocks(ctx, auto,
		func(id string) (loadedSkill, bool) {
			payload, dir, name, ok := s.loadSkillPayload(ctx, id, taskID)
			return loadedSkill{payload, dir, name}, ok
		},
		func(name string) string {
			return fmt.Sprintf("[IMPORTANT: The \"%s\" skill is auto-loaded via config (skills.auto_load). Treat its instructions as active guidance for the duration of this session unless the user overrides them.]", name)
		},
		taskID, skillBlockOpts{disabledNames: s.commandDisabledSkillNames(""), disabledAsMissing: true})
	return strings.Join(parts, "\n\n"), loadedNames, missing
}

// Message for a typed skill slash line, stacked skills included, false when the line names no skill
func (s *Scope) Command(ctx context.Context, line, taskID string) (string, bool, error) {
	trimmed := pystr.Strip(line)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false, nil
	}
	parts := pystr.SplitWhitespace(trimmed, 1)
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}
	key, ok, err := s.ResolveSkillCommandKey(ctx, strings.TrimLeft(parts[0], "/"), true)
	if err != nil || !ok {
		return "", false, err
	}
	extra, instruction, err := s.SplitStackedSkillCommands(ctx, rest, true)
	if err != nil {
		return "", false, err
	}
	if len(extra) > 0 {
		msg, _, _, ok, err := s.BuildStackedSkillInvocationMessage(ctx, append([]string{key}, extra...), instruction, taskID)
		return msg, ok, err
	}
	return s.BuildSkillInvocationMessage(ctx, key, rest, taskID, "")
}
