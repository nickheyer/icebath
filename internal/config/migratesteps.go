package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Scheme prefix of a URL
var urlSchemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// Lower case URL host without user info and port, empty when absent, ok false for unbalanced brackets
func hostnameOf(raw string) (host string, ok bool) {
	rest := raw
	if loc := urlSchemePrefix.FindStringIndex(raw); loc != nil {
		rest = raw[loc[1]:]
	}
	if !strings.HasPrefix(rest, "//") {
		return "", true
	}
	host = rest[2:]
	if at := strings.IndexAny(host, "/?#"); at >= 0 {
		host = host[:at]
	}
	if strings.Contains(host, "[") != strings.Contains(host, "]") {
		return "", false
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if open := strings.Index(host, "["); open >= 0 {
		inner, _, _ := strings.Cut(host[open+1:], "]")
		return strings.ToLower(inner), true
	}
	host, _, _ = strings.Cut(host, ":")
	return strings.ToLower(host), true
}

// Runs of dashes a provider key collapses to one
var dashRun = regexp.MustCompile(`-{2,}`)

// Moves the legacy custom_providers list into the keyed providers section
func migrateTo12(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	list := childOf(config, "custom_providers")
	if !isSequence(list) || len(list.Content) == 0 {
		return nil
	}
	providers := dictAt(config, "providers")
	var migrated []string
	for _, entry := range list.Content {
		if !isMapping(entry) {
			continue
		}
		name := ""
		if n := childOf(entry, "name"); isStringNode(n) {
			name = n.Value
		}
		var oldURL *yaml.Node
		for _, k := range []string{"base_url", "url", "api"} {
			if v := childOf(entry, k); isSet(v) {
				oldURL = v
				break
			}
		}
		if oldURL == nil {
			continue
		}
		key := strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(strings.ToLower(trimEnvSpace(name)))
		key = strings.Trim(dashRun.ReplaceAllString(key, "-"), "-")
		if key == "" {
			key = fmt.Sprintf("endpoint-%d", len(migrated))
			if isStringNode(oldURL) {
				if host, ok := hostnameOf(oldURL.Value); ok && host == "" {
					key = "endpoint"
				} else if ok {
					key = strings.ReplaceAll(host, ".", "-")
				}
			}
		}
		base := key
		for suffix := len(migrated); mappingIndex(providers, key) >= 0; suffix++ {
			key = fmt.Sprintf("%s-%d", base, suffix)
		}
		converted := m.s.customProviderToProviderConfig(entry, key)
		if converted == nil {
			continue
		}
		if name == "" {
			dropKey(converted, "name")
		}
		if k := childOf(converted, "api_key"); isStringNode(k) && slices.Contains([]string{"no-key", "no-key-required", ""}, k.Value) {
			dropKey(converted, "api_key")
		}
		putKey(providers, key, converted)
		migrated = append(migrated, key)
	}
	if len(migrated) == 0 {
		return nil
	}
	putKey(config, "providers", providers)
	dropKey(config, "custom_providers")
	if err := m.persist(config); err != nil {
		return err
	}
	m.say(fmt.Sprintf("  ✓ Migrated %d custom provider(s) to providers: section", len(migrated)))
	keys := mappingKeys(providers)
	for _, key := range keys[len(keys)-len(migrated):] {
		m.say(fmt.Sprintf("    → %s: %s", key, textOr(childOf(providers, key), "api", "")))
	}
	return nil
}

// Clears the dead model variables an old setup wizard wrote to the env file
func migrateTo13(m *migration) error {
	if m.configOnly {
		return nil
	}
	env := m.s.Env()
	for _, dead := range []string{"LLM_MODEL", "OPENAI_MODEL"} {
		value, _, err := env.Get(m.ctx, dead)
		if err != nil || value == "" {
			continue
		}
		if err := env.Set(m.ctx, dead, ""); err != nil {
			continue
		}
		m.say(fmt.Sprintf("  ✓ Cleared %s from .env (no longer used — config.yaml is source of truth)", dead))
	}
	return nil
}

// Model names the local transcription engine accepts
func localWhisperModels() []string {
	return []string{
		"tiny.en", "tiny", "base.en", "base", "small.en", "small", "medium.en", "medium", "large-v1", "large-v2", "large-v3",
		"large", "distil-large-v2", "distil-medium.en", "distil-small.en", "distil-large-v3", "distil-large-v3.5",
		"large-v3-turbo", "turbo",
	}
}

// Moves a flat stt.model under the section of the provider it was meant for
func migrateTo14(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	rawSTT := plainCopy(childOf(config, "stt"))
	if !isMapping(rawSTT) || mappingIndex(rawSTT, "model") < 0 {
		return nil
	}
	legacy := childOf(rawSTT, "model")
	provider := "local"
	if at := mappingIndex(rawSTT, "provider"); at >= 0 && isStringNode(rawSTT.Content[at]) {
		provider = rawSTT.Content[at].Value
	}
	stt := childOf(config, "stt")
	dropKey(stt, "model")
	place := func(section string) {
		if existing := childOf(rawSTT, section); isMapping(existing) && mappingIndex(existing, "model") >= 0 {
			return
		}
		target := childOf(stt, section)
		if !isMapping(target) {
			target = emptyMapping()
			putKey(stt, section, target)
		}
		putKey(target, "model", plainCopy(legacy))
	}
	if provider == "local" || provider == "local_command" {
		if isStringNode(legacy) && slices.Contains(localWhisperModels(), legacy.Value) {
			place("local")
		}
	} else {
		place(provider)
	}
	return m.commit(config, "", "  ✓ Migrated legacy stt.model to provider-specific config")
}

// Folds display.tool_progress_overrides into the per platform display blocks
func migrateTo16(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	display := dictAt(config, "display")
	overrides := childOf(display, "tool_progress_overrides")
	if !isMapping(overrides) || len(overrides.Content) == 0 {
		return nil
	}
	platforms := dictAt(display, "platforms")
	var moved []string
	for i := 0; i+1 < len(overrides.Content); i += 2 {
		platform, mode := overrides.Content[i].Value, overrides.Content[i+1]
		target := childOf(platforms, platform)
		if !isMapping(target) {
			target = emptyMapping()
			putKey(platforms, platform, target)
		}
		if mappingIndex(target, "tool_progress") < 0 {
			putKey(target, "tool_progress", plainCopy(mode))
		}
		moved = append(moved, platform+"="+displayText(mode))
	}
	putKey(display, "platforms", platforms)
	putKey(config, "display", display)
	return m.commit(config, "display.platforms (migrated from tool_progress_overrides)",
		"  ✓ Migrated tool_progress_overrides → display.platforms: "+strings.Join(moved, ", "))
}

// Removes the compression.summary keys, carrying set values to auxiliary.compression without overriding it
func migrateTo17(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	comp := childOf(config, "compression")
	if comp == nil {
		comp = emptyMapping()
	}
	if !isMapping(comp) {
		return nil
	}
	var moved []string
	any := false
	for _, k := range []string{"model", "provider", "base_url"} {
		raw := childOf(comp, "summary_"+k)
		dropKey(comp, "summary_"+k)
		any = any || (raw != nil && !isNull(raw))
		val := trimEnvSpace(orText(raw))
		if val == "" || (k == "provider" && val == "auto") {
			continue
		}
		aux := childOf(config, "auxiliary")
		if !isMapping(aux) {
			aux = emptyMapping()
			putKey(config, "auxiliary", aux)
		}
		auxComp := childOf(aux, "compression")
		if !isMapping(auxComp) {
			auxComp = emptyMapping()
			putKey(aux, "compression", auxComp)
		}
		if cur := childOf(auxComp, k); !isSet(cur) || (k == "provider" && isStringNode(cur) && cur.Value == "auto") {
			putKey(auxComp, k, stringNode(val))
			moved = append(moved, k+"="+displayText(raw))
		}
	}
	if len(moved) == 0 && !any {
		return nil
	}
	putKey(config, "compression", comp)
	message := "  ✓ Removed unused compression.summary_* keys"
	if len(moved) > 0 {
		message = "  ✓ Migrated compression.summary_* → auxiliary.compression: " + strings.Join(moved, ", ")
	}
	return m.commit(config, "", message)
}

// Names of the plugins installed under the plugins folder of a home with a manifest, without the disabled ones
func installedUserPlugins(h string, disabled []string) []string {
	root := home.Plugins.In(h)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		manifest := filepath.Join(dir, "plugin.yaml")
		if !fileExists(manifest) {
			manifest = filepath.Join(dir, "plugin.yml")
		}
		if !fileExists(manifest) {
			continue
		}
		name := entry.Name()
		var doc yaml.Node
		if data, err := os.ReadFile(manifest); err == nil && LoadYAML(data, &doc) == nil {
			if !isMapping(&doc) && isSet(&doc) {
				return nil
			}
			if declared := childOf(&doc, "name"); isSet(declared) {
				name = displayText(declared)
			}
		}
		if !slices.Contains(disabled, name) {
			found = append(found, name)
		}
	}
	return found
}

// Makes plugins opt in, enabling the installed user plugins that were not disabled
func migrateTo21(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	plugins := dictAt(config, "plugins")
	if mappingIndex(plugins, "enabled") >= 0 {
		return nil
	}
	var disabled []string
	if d := childOf(plugins, "disabled"); isSequence(d) {
		for _, it := range d.Content {
			disabled = append(disabled, displayText(it))
		}
	}
	grandfathered := installedUserPlugins(m.home, disabled)
	putKey(plugins, "enabled", stringSeq(grandfathered))
	putKey(config, "plugins", plugins)
	message := "  ✓ Plugins now opt-in: no existing plugins to grandfather. Use `icebath plugins enable <name>` to activate."
	if len(grandfathered) > 0 {
		message = fmt.Sprintf("  ✓ Plugins now opt-in: grandfathered %d existing plugin(s) into plugins.enabled", len(grandfathered))
	}
	return m.commit(config, fmt.Sprintf("plugins.enabled (opt-in allow-list, %d grandfathered)", len(grandfathered)), message)
}

// Seeds the curator sections with the defaults the file lacks and creates the curator log folder
func migrateTo23(m *migration) error {
	defaults, err := m.s.defaults()
	if err != nil {
		return err
	}
	if !m.configOnly {
		dir := home.LogsCurator.In(m.home)
		if err := m.s.homes.Mkdir(m.ctx, dir); err != nil {
			m.report.Warnings = append(m.report.Warnings, fmt.Sprintf("Could not create %s: %v", dir, err))
		}
	}
	config, err := m.raw()
	if err != nil {
		return err
	}
	seed := func(section, def *yaml.Node) []string {
		var added []string
		for i := 0; isMapping(def) && i+1 < len(def.Content); i += 2 {
			if k := def.Content[i].Value; mappingIndex(section, k) < 0 {
				putKey(section, k, plainCopy(def.Content[i+1]))
				added = append(added, k)
			}
		}
		return added
	}
	curator := dictAt(config, "curator")
	addedCurator := seed(curator, childOf(defaults, "curator"))
	if len(addedCurator) > 0 {
		putKey(config, "curator", curator)
	}
	aux := dictAt(config, "auxiliary")
	auxCurator := dictAt(aux, "curator")
	addedAux := seed(auxCurator, treeGet(defaults, "auxiliary", "curator"))
	if len(addedAux) > 0 {
		putKey(aux, "curator", auxCurator)
		putKey(config, "auxiliary", aux)
	}
	if len(addedCurator) == 0 && len(addedAux) == 0 {
		return nil
	}
	if err := m.persist(config); err != nil {
		return err
	}
	for _, part := range []struct {
		label, shown string
		added        []string
	}{{"curator", "Curator", addedCurator}, {"auxiliary.curator", "auxiliary.curator", addedAux}} {
		if len(part.added) == 0 {
			continue
		}
		m.report.ConfigAdded = append(m.report.ConfigAdded, fmt.Sprintf("%s (%d default key(s))", part.label, len(part.added)))
		m.say(fmt.Sprintf("  ✓ %s settings now available (%s) — edit via `icebath config set`", part.shown, strings.Join(part.added, ", ")))
	}
	return nil
}

// Renames the write_mode word of memory and skills to the write_approval switch
func migrateTo29(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	touched := false
	for _, subsystem := range []string{"memory", "skills"} {
		sub := childOf(config, subsystem)
		at := mappingIndex(sub, "write_mode")
		if at < 0 {
			continue
		}
		old := sub.Content[at]
		approve := isStringNode(old) && strings.ToLower(trimEnvSpace(old.Value)) == "approve"
		dropKey(sub, "write_mode")
		putKey(sub, "write_approval", boolNode(approve))
		touched = true
		m.report.ConfigAdded = append(m.report.ConfigAdded, fmt.Sprintf("%s.write_mode → write_approval=%s", subsystem, displayText(boolNode(approve))))
	}
	if !touched {
		return nil
	}
	return m.commit(config, "", "  ✓ Renamed write_mode → write_approval (boolean gate)")
}

// Folds a raised delegation.max_async_children into max_concurrent_children and drops the key
func migrateTo33(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	deleg := childOf(config, "delegation")
	at := mappingIndex(deleg, "max_async_children")
	if at < 0 {
		return nil
	}
	old, ok := wholeOf(deleg.Content[at])
	dropKey(deleg, "max_async_children")
	if ok && old > 3 {
		cur := 3
		if at := mappingIndex(deleg, "max_concurrent_children"); at >= 0 {
			if v, ok := wholeOf(deleg.Content[at]); ok {
				cur = v
			}
		}
		if old > cur {
			putKey(deleg, "max_concurrent_children", intNode(old))
			m.report.ConfigAdded = append(m.report.ConfigAdded,
				fmt.Sprintf("delegation.max_concurrent_children=%d (folded from deprecated max_async_children)", old))
		}
	}
	return m.commit(config, "", "  ✓ Removed deprecated delegation.max_async_children — "+
		"delegation.max_concurrent_children now caps background delegations too.")
}

// Text of a prompt setting written as a string, a list of lines or any other value
func promptText(n *yaml.Node) string {
	switch {
	case n == nil || isNull(n):
		return ""
	case isSequence(n):
		var lines []string
		for _, it := range n.Content {
			if line := trimEnvSpace(displayText(it)); line != "" {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	return trimEnvSpace(displayText(n))
}

// What the personality layer knows that the one time personality reset needs
type PersonalityFacts struct {
	Neutral  []string
	Rendered []string
}

// Resets a stale display.personality name and scrubs a system prompt that is a personality text
func migrateTo34(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	display, agent := childOf(config, "display"), childOf(config, "agent")
	if !isMapping(display) && !isMapping(agent) {
		return nil
	}
	if m.s.know.Personalities == nil {
		return &SeamError{Seam: "personalities"}
	}
	facts := m.s.know.Personalities(plainCopy(config))
	touched, scrubbed, oldName := false, false, ""
	if isMapping(display) {
		oldName = strings.ToLower(trimEnvSpace(orText(childOf(display, "personality"))))
		if slices.Contains(facts.Neutral, oldName) {
			oldName = ""
		}
		if oldName != "" {
			putKey(display, "personality", stringNode(""))
			touched = true
		}
	}
	if isMapping(agent) {
		if manual := promptText(childOf(agent, "system_prompt")); manual != "" && slices.Contains(facts.Rendered, manual) {
			putKey(agent, "system_prompt", stringNode(""))
			touched, scrubbed = true, true
		}
	}
	if !touched {
		return nil
	}
	if err := m.commit(config, "display.personality=none (one-time reset)", ""); err != nil {
		return err
	}
	if oldName != "" {
		m.say(fmt.Sprintf("  ✓ Personality reset to none (was '%s'). Personality state was previously saved inconsistently across surfaces and "+
			"could re-enable a personality you had turned off. Run /personality %s to turn it back on.", oldName, oldName))
	}
	if scrubbed {
		m.say("  ✓ Removed personality text from agent.system_prompt (written by an older /personality). That field is now reserved for " +
			"manual system prompts; personalities live in display.personality.")
	}
	return nil
}

// Drops the removed bundled relay plugin from plugins.enabled
func migrateTo38(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	plugins := childOf(config, "plugins")
	enabled := childOf(plugins, "enabled")
	if !isMapping(plugins) || !isSequence(enabled) {
		return nil
	}
	legacy := []string{"nemo_relay", "observability/nemo_relay"}
	var removed []string
	kept := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
	for _, it := range enabled.Content {
		if isStringNode(it) && slices.Contains(legacy, it.Value) {
			removed = append(removed, it.Value)
			continue
		}
		kept.Content = append(kept.Content, it)
	}
	if len(removed) == 0 {
		return nil
	}
	putKey(plugins, "enabled", kept)
	if err := m.persist(config); err != nil {
		return err
	}
	message := "Removed legacy Relay plugin from plugins.enabled: " + strings.Join(sortedSet(removed...), ", ") +
		". Configure a standard user or system Relay plugins.toml, or use ICEBATH_NEMO_RELAY_PLUGINS_TOML for an explicit user-file override."
	m.report.Warnings = append(m.report.Warnings, message)
	m.say("  ⚠ " + message)
	return nil
}

// Strips the retired bfl toolset from every saved toolset list
func migrateTo39(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	changed := false
	for _, section := range []string{"platform_toolsets", "known_builtin_toolsets"} {
		mapping := childOf(config, section)
		if !isMapping(mapping) {
			continue
		}
		for i := 1; i < len(mapping.Content); i += 2 {
			list := mapping.Content[i]
			if !isSequence(list) || !slices.ContainsFunc(list.Content, func(it *yaml.Node) bool { return isStringNode(it) && it.Value == "bfl" }) {
				continue
			}
			list.Content = slices.DeleteFunc(list.Content, func(it *yaml.Node) bool { return isStringNode(it) && it.Value == "bfl" })
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return m.commit(config, "removed retired 'bfl' toolset from saved toolset lists",
		"  ✓ Removed the retired BFL FLUX 3 toolset from saved toolset lists — video generation now lives under `icebath tools` → "+
			"Video Generation (Icebath Portal Subscription or FAL).")
}

// Heading of the frozen teammate messaging section older builds appended to SOUL.md
const legacyProtocolHeading = "## Messaging other agents"

// Persona text without the frozen teammate messaging sections, each running to the next second level heading
func stripLegacyProtocol(text string) string {
	if !strings.Contains(text, legacyProtocolHeading) {
		return text
	}
	var b strings.Builder
	rest := text
	for {
		at := strings.Index(rest, legacyProtocolHeading)
		if at < 0 {
			break
		}
		after := strings.TrimLeft(rest[at+len(legacyProtocolHeading):], " \t")
		if !strings.HasPrefix(after, "\n") {
			b.WriteString(rest[:at+len(legacyProtocolHeading)])
			rest = rest[at+len(legacyProtocolHeading):]
			continue
		}
		b.WriteString(strings.TrimRight(rest[:at], "\n"))
		b.WriteString("\n")
		body := after[1:]
		if next := strings.Index(body, "\n## "); next >= 0 {
			rest = body[next:]
		} else {
			rest = ""
		}
	}
	b.WriteString(rest)
	return trimEnvSpace(b.String()) + "\n"
}

// Removes the frozen teammate messaging section from the SOUL.md of every profile of the install
func migrateTo41(m *migration) error {
	if m.configOnly {
		return nil
	}
	root, err := home.RootOf(m.home)
	if err != nil {
		return err
	}
	type profile struct{ name, dir string }
	roster := []profile{{home.DefaultProfile, root}}
	if entries, err := os.ReadDir(home.Profiles.In(root)); err == nil {
		for _, entry := range entries {
			dir := filepath.Join(home.Profiles.In(root), entry.Name())
			if entry.Name() != home.DefaultProfile && home.ValidProfileID(entry.Name()) && home.ProfileLive(dir) {
				roster = append(roster, profile{entry.Name(), dir})
			}
		}
	}
	var cleaned []string
	for _, p := range roster {
		soul := home.Soul.In(p.dir)
		if fi, err := os.Stat(soul); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		text, err := fileutil.ReadText(soul)
		if err != nil || !strings.Contains(text, legacyProtocolHeading) {
			continue
		}
		if err := fileutil.WriteText(m.ctx, soul, stripLegacyProtocol(text), fileutil.KeepPerm(), fileutil.KeepOwner()); err != nil {
			continue
		}
		cleaned = append(cleaned, p.name)
	}
	if len(cleaned) == 0 {
		return nil
	}
	names := strings.Join(cleaned, ", ")
	m.report.ConfigAdded = append(m.report.ConfigAdded, fmt.Sprintf("removed legacy Bot Mode section from SOUL.md (%s)", names))
	m.say(fmt.Sprintf("  ✓ Removed the plugin-era 'Messaging other agents' section from SOUL.md (%s) — "+
		"Bot Chat sessions now get the live roster instead.", names))
	return nil
}

// Appends the connections toolset to every saved platform list that predates it
func migrateTo45(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	saved := childOf(config, "platform_toolsets")
	if !isMapping(saved) {
		return nil
	}
	if slices.Contains(configStringList(childOf(dictAt(config, "agent"), "disabled_toolsets")), "connections") {
		return nil
	}
	if m.s.know.ExplicitToolsetKeys == nil || m.s.know.ToolsetAllowed == nil {
		return &SeamError{Seam: "toolset keys"}
	}
	explicit, err := m.s.know.ExplicitToolsetKeys(m.ctx)
	if err != nil {
		return err
	}
	known := dictAt(config, "known_builtin_toolsets")
	texts := func(list *yaml.Node) []string {
		out := make([]string, 0, len(list.Content))
		for _, it := range list.Content {
			out = append(out, displayText(it))
		}
		return out
	}
	var enabledFor []string
	for i := 0; i+1 < len(saved.Content); i += 2 {
		platform, toolsets := displayText(saved.Content[i]), saved.Content[i+1]
		if !isSequence(toolsets) || slices.Contains(texts(toolsets), "connections") || !m.s.know.ToolsetAllowed("connections", platform) {
			continue
		}
		if !slices.ContainsFunc(texts(toolsets), func(ts string) bool { return slices.Contains(explicit, ts) }) {
			continue
		}
		offered := childOf(known, saved.Content[i].Value)
		if isSequence(offered) && slices.Contains(texts(offered), "connections") {
			continue
		}
		saved.Content[i+1] = stringSeq(sortedSet(append(texts(toolsets), "connections")...))
		if isSequence(offered) {
			putKey(known, saved.Content[i].Value, stringSeq(sortedSet(append(texts(offered), "connections")...)))
		}
		enabledFor = append(enabledFor, platform)
	}
	if len(enabledFor) == 0 {
		return nil
	}
	if len(known.Content) > 0 {
		putKey(config, "known_builtin_toolsets", known)
	}
	platforms := strings.Join(sortedSet(enabledFor...), ", ")
	return m.commit(config, "enabled the connections toolset for "+platforms,
		"  ✓ Enabled the Connections toolset (Gmail, Linear, Notion, local MCP servers) for "+platforms+
			". Uncheck Connections in `icebath tools` to turn it off.")
}

// Switch a tool or platform setting spells, booleans and numbers by value and the usual words, def otherwise
func enabledFlag(n *yaml.Node, def bool) bool {
	if n == nil || n.Kind != yaml.ScalarNode {
		return def
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagBool, tagInt:
		return numberOf(n) != 0
	case tagStr:
		switch strings.ToLower(trimEnvSpace(text)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return def
}

// Carries the disabled mark an old editor put on MCP servers over to enabled false
func migrateTo46(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	servers := childOf(config, "mcp_servers")
	if !isMapping(servers) {
		return nil
	}
	var turnedOff []string
	for i := 0; i+1 < len(servers.Content); i += 2 {
		entry := servers.Content[i+1]
		at := mappingIndex(entry, "disabled")
		if at < 0 || !enabledFlag(entry.Content[at], false) {
			continue
		}
		dropKey(entry, "disabled")
		putKey(entry, "enabled", boolNode(false))
		turnedOff = append(turnedOff, displayText(servers.Content[i]))
	}
	if len(turnedOff) == 0 {
		return nil
	}
	names := strings.Join(sortedSet(turnedOff...), ", ")
	return m.commit(config, fmt.Sprintf("mcp_servers: disabled → enabled: false (%s)", names),
		fmt.Sprintf("  ✓ Turned off MCP servers the profile editor had marked disabled: %s.", names))
}

// Unsets sandbox images still equal to an old default so the file follows the current one
func migrateTo48(m *migration) error {
	for _, legacy := range LegacySandboxImages() {
		for _, k := range [][2]string{{"docker_image", legacy}, {"modal_image", legacy}, {"daytona_image", legacy}, {"singularity_image", "docker://" + legacy}} {
			err := m.rewrite(keyRewrite{section: "terminal", key: k[0], match: stillEquals(stringNode(k[1])),
				added: fmt.Sprintf("terminal.%s unset (follows the default, %s)", k[0], DefaultSandboxImage),
				message: fmt.Sprintf("  ✓ terminal.%s: was the old default; now follows the default sandbox image (%s)",
					k[0], DefaultSandboxImage)})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Drops the seeded Vercel runtime pin from the config and from its env file mirror
func migrateTo49(m *migration) error {
	err := m.rewrite(keyRewrite{section: "terminal", key: "vercel_runtime", match: stillEquals(stringNode(LegacyVercelRuntime)),
		added: fmt.Sprintf("terminal.vercel_runtime unset (fresh sandboxes use terminal.vercel_image, %s)", DefaultVercelImage),
		message: fmt.Sprintf("  ✓ terminal.vercel_runtime: was the old default; fresh sandboxes now use the managed image (%s)",
			DefaultVercelImage)})
	if err != nil || m.configOnly {
		return err
	}
	env := m.s.Env()
	value, _, err := env.GetPreferFile(m.ctx, "TERMINAL_VERCEL_RUNTIME")
	if err != nil || trimEnvSpace(value) != LegacyVercelRuntime {
		return err
	}
	if _, err := env.Remove(m.ctx, "TERMINAL_VERCEL_RUNTIME"); err != nil {
		return err
	}
	m.say("  ✓ Cleared TERMINAL_VERCEL_RUNTIME from .env (was the old default; the image is used instead)")
	return nil
}

// Removes the keys of the scanner that is no longer bundled
func migrateTo50(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	security := childOf(config, "security")
	if !isMapping(security) {
		return nil
	}
	present := false
	for _, key := range []string{"tirith_enabled", "tirith_path", "tirith_timeout", "tirith_fail_open"} {
		if mappingIndex(security, key) >= 0 {
			dropKey(security, key)
			present = true
		}
	}
	if !present {
		return nil
	}
	return m.commit(config, "removed security.tirith_* (scanner no longer bundled)",
		"  ✓ Removed security.tirith_* — the tirith scanner is no longer bundled with icebath.")
}
