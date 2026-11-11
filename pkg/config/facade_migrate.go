package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
)

// Streams the facade prompts read from and its plain output is printed to
var (
	FacadeStdout io.Writer = pysys.Stdout
	FacadeStdin  io.Reader = os.Stdin
)

// Env variables every install must set, none since the setup wizard picks the provider
var RequiredEnvVars = map[string]EnvVar{}

// Env variables introduced per config version, migration offers only those new since the previous version
var EnvVarsByVersion = map[int][]string{
	3:  {"FIRECRAWL_API_KEY", "BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID", "FAL_KEY"},
	4:  {"VOICE_TOOLS_OPENAI_KEY", "ELEVENLABS_API_KEY"},
	5:  {"WHATSAPP_ENABLED", "WHATSAPP_MODE", "WHATSAPP_ALLOWED_USERS", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_ALLOWED_USERS"},
	10: {"TAVILY_API_KEY"},
	11: {"TERMINAL_MODAL_MODE"},
}

// Collaborators migrate_config reaches in packages that import config
type MigrateCollaborators struct {
	Personality             PersonalitySources
	ValidateMCPServerEntry  func(name string, entry map[string]any) []string
	PlatformToolsetWarnings func(cfg map[string]any) ([]string, error)
	SkillConfigVars         func() ([]SkillConfigVar, error)
}

var (
	migrateMu     sync.RWMutex
	migrateCollab MigrateCollaborators
)

// Installs the personality, mcp security, toolset and skill readers the migration pass consults
func RegisterMigrateCollaborators(c MigrateCollaborators) {
	migrateMu.Lock()
	migrateCollab = c
	migrateMu.Unlock()
}

func migrateCollaborators() MigrateCollaborators {
	migrateMu.RLock()
	defer migrateMu.RUnlock()
	return migrateCollab
}

// One env variable the install lacks, its catalog entry and whether it is required
type MissingEnvVar struct {
	Name string
	EnvVar
	IsRequired bool
}

// Config file stamp hooks the facade registration supplies
func facadeStampHooks() StampHooks {
	if fn := hooks().Stamp; fn != nil {
		return fn()
	}
	return StampHooks{}
}

// Sets a mapping key keeping the position of an existing key and appending a new one
func setOrdered(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		AppendMappingKey(m, key)
	}
	m[key] = value
}

// Saves a migrated config with defaults stripped, applying it in memory when the backend has no file
func persistMigration(ctx context.Context, h string, cfg map[string]any) error {
	files, err := SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if !files {
		backend, err := GetConfigBackend(ctx)
		if err != nil {
			return err
		}
		return backend.ApplyInMemory(ctx, filepath.Dir(home.ConfigPath(h)), cfg)
	}
	return SaveConfig(AppliedWrite(ctx), h, cfg, SaveOptions{})
}

// Reads one line from the facade input like input, io.EOF when the stream ends before any text
func lineInput(prompt string) (string, error) {
	fmt.Fprint(FacadeStdout, prompt)
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := FacadeStdin.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, buf[0])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), nil
			}
			return "", err
		}
	}
}

// Prompts for one env variable, masked for passwords, saves it and records it, false when skipped
func promptAndSaveEnv(ctx context.Context, h, name string, info EnvVar, prompt string, results *MigrationResults) (bool, error) {
	var value string
	var err error
	if info.Password {
		masked := hooks().MaskedSecretPrompt
		if masked == nil {
			return false, errors.New("masked secret prompt is not registered")
		}
		value, err = masked(prompt)
	} else {
		value, err = lineInput(prompt)
		value = strings.TrimSpace(value)
	}
	if err != nil {
		return false, err
	}
	if value == "" {
		return false, nil
	}
	if err := SaveEnvValueContext(ctx, h, name, value); err != nil {
		return false, err
	}
	results.EnvAdded = append(results.EnvAdded, name)
	fmt.Fprintf(FacadeStdout, "  ✓ Saved %s\n", name)
	return true, nil
}

// Reports a yes answer, an ended or interrupted input counting as no
func askYesNo(prompt string) bool {
	answer, err := lineInput(prompt)
	if err != nil {
		answer = "n"
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// Ladder collaborators, the facade owned readers and writers over the registered cross package ones
func migrationSources(ctx context.Context, h string) MigrationSources {
	var src MigrationSources
	if fn := hooks().Migration; fn != nil {
		src = fn(h)
	}
	src.Personality = migrateCollaborators().Personality
	src.Out = FacadeStdout
	src.ReadRaw = func(h string) (map[string]any, error) { return ReadRawConfig(ctx, h) }
	src.Persist = func(h string, cfg map[string]any) error { return persistMigration(ctx, h, cfg) }
	src.GetEnvValue = func(name string) string {
		v, _, err := GetEnvValue(ctx, h, name)
		if err != nil {
			panic(err)
		}
		return v
	}
	src.GetEnvValuePreferDotenv = func(name string) string {
		v, err := GetEnvValuePreferDotenv(ctx, h, name)
		if err != nil {
			panic(err)
		}
		return v
	}
	src.SaveEnvValue = func(name, value string) error { return SaveEnvValueContext(ctx, h, name, value) }
	src.RemoveEnvValue = func(name string) (bool, error) { return RemoveHomeEnvValue(ctx, h, name) }
	return src
}

// Ladder collaborators of a home as the migration facade assembles them
func MigrationFacadeSources(ctx context.Context, h string) MigrationSources {
	return migrationSources(ctx, h)
}

// Migrates the config to the latest version, prompting for missing env and skill settings when interactive
func MigrateConfig(ctx context.Context, h string, interactive, quiet bool) (*MigrationResults, error) {
	results := &MigrationResults{EnvAdded: []string{}, ConfigAdded: []string{}, Warnings: []string{}}
	if _, err := StampMissingConfig(ctx, h, facadeStampHooks()); err != nil {
		return nil, err
	}
	stamp, latest, err := ReadConfigVersionStamp(ctx, h, true)
	if err != nil {
		return nil, err
	}
	current := 0
	if stamp != nil {
		current = *stamp
	}
	if fixes, err := SanitizeEnvFile(ctx, h); err == nil && fixes > 0 && !quiet {
		fmt.Fprintf(FacadeStdout, "  ✓ Normalized .env line formatting (%d line(s) changed)\n", fixes)
	}
	hasExplicit := stamp != nil
	floorRefused := hasExplicit && current < SupportFloorVersion && current < latest
	if floorRefused {
		msg := SupportFloorMessage(h)
		results.Warnings = append(results.Warnings, msg)
		fmt.Fprintf(FacadeNoticeOut, "⚠ icebath config: %s\n", msg)
		if !quiet {
			fmt.Fprintf(FacadeStdout, "  ⚠ %s\n", msg)
		}
	} else {
		RunMigrations(h, current, results, quiet, MigrationOptions{Unversioned: !hasExplicit}, migrationSources(ctx, h))
	}
	if err := disableSuspiciousMCPServers(ctx, h, results, quiet); err != nil {
		return nil, err
	}
	warnInvalidPlatformToolsets(ctx, h, results, quiet)
	if current < latest && !quiet && !floorRefused {
		fmt.Fprintf(FacadeStdout, "Config version: %d → %d\n", current, latest)
	}
	missingEnv, err := GetMissingEnvVars(ctx, h, true)
	if err != nil {
		return nil, err
	}
	if len(missingEnv) > 0 && !quiet {
		fmt.Fprintln(FacadeStdout, "\n⚠️  Missing required environment variables:")
		for _, v := range missingEnv {
			fmt.Fprintf(FacadeStdout, "   • %s: %s\n", v.Name, v.Description)
		}
	}
	if interactive && len(missingEnv) > 0 {
		fmt.Fprint(FacadeStdout, "\nLet's configure them now:\n\n")
		for _, v := range missingEnv {
			if v.URL != "" {
				fmt.Fprintf(FacadeStdout, "  Get your key at: %s\n", v.URL)
			}
			saved, err := promptAndSaveEnv(ctx, h, v.Name, v.EnvVar, "  "+v.Prompt+": ", results)
			if err != nil {
				return nil, err
			}
			if !saved {
				results.Warnings = append(results.Warnings, "Skipped "+v.Name+" - some features may not work")
			}
			fmt.Fprintln(FacadeStdout)
		}
	}
	if interactive && !quiet {
		if err := offerNewOptionalEnvVars(ctx, h, current, latest, results); err != nil {
			return nil, err
		}
	}
	fields, err := GetMissingConfigFields(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		results.ConfigAdded = append(results.ConfigAdded, f.Key)
	}
	if current < latest && !floorRefused {
		cfg, err := ReadRawConfig(ctx, h)
		if err != nil {
			return nil, err
		}
		setOrdered(cfg, "_config_version", latest)
		if err := persistMigration(ctx, h, cfg); err != nil {
			return nil, err
		}
	}
	missingSkill, err := GetMissingSkillConfigVars(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(missingSkill) > 0 && interactive && !quiet {
		if err := offerSkillConfigVars(ctx, h, missingSkill, results); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// Marks exfiltration shaped mcp stdio entries disabled, keeping the stanza for review
func disableSuspiciousMCPServers(ctx context.Context, h string, results *MigrationResults, quiet bool) error {
	cfg, err := ReadRawConfig(ctx, h)
	if err != nil {
		return err
	}
	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		return nil
	}
	validate := migrateCollaborators().ValidateMCPServerEntry
	if validate == nil {
		return nil
	}
	touched := false
	for _, name := range MappingKeys(servers) {
		entry, isMap := servers[name].(map[string]any)
		if !isMap {
			continue
		}
		issues := validate(name, entry)
		if len(issues) == 0 {
			continue
		}
		setOrdered(entry, "enabled", false)
		touched = true
		results.Warnings = append(results.Warnings, fmt.Sprintf("Disabled suspicious MCP server '%s'", name))
		if !quiet {
			for _, issue := range issues {
				fmt.Fprintf(FacadeStdout, "  ⚠ %s\n", issue)
			}
			fmt.Fprintf(FacadeStdout, "  ⚠ Disabled MCP server '%s' pending review\n", name)
		}
	}
	if !touched {
		return nil
	}
	setOrdered(cfg, "mcp_servers", servers)
	return persistMigration(ctx, h, cfg)
}

// Records invalid platform toolset names as warnings, a failed check only logged at debug
func warnInvalidPlatformToolsets(ctx context.Context, h string, results *MigrationResults, quiet bool) {
	check := migrateCollaborators().PlatformToolsetWarnings
	if check == nil {
		slog.Debug("platform_toolsets validation skipped: toolset validation is not registered")
		return
	}
	cfg, err := ReadRawConfig(ctx, h)
	var warnings []string
	if err == nil {
		warnings, err = check(cfg)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("platform_toolsets validation skipped: %v", err))
		return
	}
	for _, w := range warnings {
		results.Warnings = append(results.Warnings, w)
		if !quiet {
			fmt.Fprintf(FacadeStdout, "  ⚠ %s\n", w)
		}
	}
}

// Prints a bulleted offer and asks, printing the set later hint when declined
func offerList(heading string, items []string, question string) bool {
	fmt.Fprintln(FacadeStdout, heading)
	for _, item := range items {
		fmt.Fprintf(FacadeStdout, "    • %s\n", item)
	}
	fmt.Fprintln(FacadeStdout)
	if !askYesNo(question) {
		fmt.Fprintln(FacadeStdout, "  Set later with: icebath config set <key> <value>")
		return false
	}
	fmt.Fprintln(FacadeStdout)
	return true
}

// Offers the optional env variables new since the previous config version that are still unset
func offerNewOptionalEnvVars(ctx context.Context, h string, current, latest int, results *MigrationResults) error {
	names := map[string]bool{}
	for ver := current + 1; ver <= latest; ver++ {
		for _, n := range EnvVarsByVersion[ver] {
			names[n] = true
		}
	}
	var unset []string
	for _, name := range sortedSetKeys(names) {
		v, _, err := GetEnvValue(ctx, h, name)
		if err != nil {
			return err
		}
		if known := OptionalEnvVars.Has(name); v == "" && known {
			unset = append(unset, name)
		}
	}
	if len(unset) == 0 {
		return nil
	}
	items := make([]string, 0, len(unset))
	for _, name := range unset {
		items = append(items, name+" — "+OptionalEnvVars.Entry(name).Description)
	}
	if !offerList(fmt.Sprintf("\n  %d new optional key(s) in this update:", len(unset)), items, "  Configure new keys? [y/N]: ") {
		return nil
	}
	for _, name := range unset {
		info := OptionalEnvVars.Entry(name)
		fmt.Fprintf(FacadeStdout, "  %s\n", firstNonEmpty(info.Description, name))
		if info.URL != "" {
			fmt.Fprintf(FacadeStdout, "  Get your key at: %s\n", info.URL)
		}
		if _, err := promptAndSaveEnv(ctx, h, name, info, "  "+firstNonEmpty(info.Prompt, name)+" (Enter to skip): ", results); err != nil {
			return err
		}
		fmt.Fprintln(FacadeStdout)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Prompts for skill declared settings that are missing and persists the answers
func offerSkillConfigVars(ctx context.Context, h string, missing []SkillConfigVar, results *MigrationResults) error {
	items := make([]string, 0, len(missing))
	for _, v := range missing {
		items = append(items, fmt.Sprintf("%s — %s (from skill: %s)", v.Key, v.Description, firstNonEmpty(v.Skill, "unknown")))
	}
	if !offerList(fmt.Sprintf("\n  %d skill setting(s) not configured:", len(missing)), items, "  Configure skill settings? [y/N]: ") {
		return nil
	}
	cfg, err := ReadRawConfig(ctx, h)
	if err != nil {
		return err
	}
	for _, v := range missing {
		def := ""
		if PyTruthy(v.Default) {
			def = PyStr(v.Default)
		}
		hint := ""
		if def != "" {
			hint = " (default: " + def + ")"
		}
		answer, err := lineInput("  " + v.Prompt + hint + ": ")
		if err != nil {
			return err
		}
		value := strings.TrimSpace(answer)
		if value == "" {
			value = def
		}
		if value != "" {
			if err := Tree(cfg).Set(skillConfigPrefix+"."+v.Key, value); err != nil {
				return err
			}
			results.ConfigAdded = append(results.ConfigAdded, v.Key)
			fmt.Fprintf(FacadeStdout, "  ✓ Saved %s = %s\n", v.Key, value)
		} else {
			results.Warnings = append(results.Warnings, fmt.Sprintf("Skipped %s — skill '%s' may ask for it later", v.Key, firstNonEmpty(v.Skill, "?")))
		}
		fmt.Fprintln(FacadeStdout)
	}
	return persistMigration(ctx, h, cfg)
}

const skillConfigPrefix = "skills.config"

// Env variables with no value in the scoped process env or the home .env, required ones first
func GetMissingEnvVars(ctx context.Context, h string, requiredOnly bool) ([]MissingEnvVar, error) {
	type group struct {
		table    map[string]EnvVar
		names    []string
		required bool
	}
	groups := []group{{RequiredEnvVars, SortedEnvNames(RequiredEnvVars), true}}
	if !requiredOnly {
		groups = append(groups, group{OptionalEnvVars.Copy(), OptionalEnvVars.Names(), false})
	}
	var out []MissingEnvVar
	for _, g := range groups {
		for _, name := range g.names {
			v, _, err := GetEnvValue(ctx, h, name)
			if err != nil {
				return nil, err
			}
			if v == "" {
				out = append(out, MissingEnvVar{Name: name, EnvVar: g.table[name], IsRequired: g.required})
			}
		}
	}
	return out, nil
}

// Names of an env catalog in sorted order
func SortedEnvNames(table map[string]EnvVar) []string {
	out := make([]string, 0, len(table))
	for name := range table {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Skill declared settings under skills.config that are missing or blank in the loaded config
func GetMissingSkillConfigVars(ctx context.Context, h string) ([]SkillConfigVar, error) {
	discover := migrateCollaborators().SkillConfigVars
	if discover == nil {
		return nil, nil
	}
	all, err := discover()
	if err != nil {
		slog.Debug(fmt.Sprintf("discover_all_skill_config_vars failed: %v", err))
		return nil, nil
	}
	if len(all) == 0 {
		return nil, nil
	}
	cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		return nil, err
	}
	var out []SkillConfigVar
	for _, v := range all {
		value, _ := CfgGet(cfg, strings.Split(skillConfigPrefix+"."+v.Key, ".")...)
		if s, isStr := value.(string); value == nil || (isStr && strings.TrimSpace(s) == "") {
			out = append(out, v)
		}
	}
	return out, nil
}

// Decodes bytes as utf-8-sig with invalid sequences replaced and universal newlines, keeping line ends
func readEnvTextLines(raw []byte) []string {
	raw = trimUTF8BOM(raw)
	var b strings.Builder
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			raw = raw[pyInvalidUTF8Len(raw):]
			continue
		}
		b.WriteRune(r)
		raw = raw[size:]
	}
	text := strings.ReplaceAll(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\r", "\n")
	return strings.SplitAfter(text, "\n")
}

func trimUTF8BOM(raw []byte) []byte {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return raw[3:]
	}
	return raw
}

// Bytes Python's replace handler consumes for one invalid sequence, the maximal valid prefix of a multibyte form
func pyInvalidUTF8Len(raw []byte) int {
	lead := raw[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for i := 1; i <= need && i < len(raw); i++ {
		c := raw[i]
		if i == 1 && (c < lo || c > hi) || i > 1 && (c < 0x80 || c > 0xBF) {
			break
		}
		n++
	}
	return n
}

var (
	envWriteLockHoldersGuard sync.Mutex
	envWriteLockHolders      = map[string]*pyfs.LockHolder{}
)

// Reentrancy tracker of one .env path so multiplexed profiles each get their own
func envWriteLockHolder(envPath string) *pyfs.LockHolder {
	envWriteLockHoldersGuard.Lock()
	defer envWriteLockHoldersGuard.Unlock()
	holder, ok := envWriteLockHolders[envPath]
	if !ok {
		holder = &pyfs.LockHolder{}
		envWriteLockHolders[envPath] = holder
	}
	return holder
}

// Serializes a whole .env read modify write cycle across goroutines and processes, reentrant per goroutine
func envWriteLock(envPath string) (func(), error) {
	return pyfs.HolderFileLock(envPath+".lock", envWriteLockHolder(envPath), 15*time.Second,
		fmt.Sprintf("Timed out waiting for the environment file lock (%s)", envPath))
}

// Holds the .env write lock for the duration of fn
func withEnvWriteLock(path string, fn func() error) error {
	unlock, err := envWriteLock(path)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// Rewrites the home .env with normalized line formatting and returns how many lines changed
func SanitizeEnvFile(ctx context.Context, h string) (int, error) {
	path := home.EnvPath(h)
	if _, err := os.Stat(path); err != nil {
		return 0, nil
	}
	fixes := 0
	err := withEnvWriteLock(path, func() error {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		original := readEnvTextLines(raw)
		if n := len(original); n > 0 && original[n-1] == "" {
			original = original[:n-1]
		}
		sanitized := SanitizeEnvLines(original)
		if slices.Equal(sanitized, original) {
			return nil
		}
		if d := len(sanitized) - len(original); d != 0 {
			fixes = max(d, -d)
		} else {
			for i := range original {
				if original[i] != sanitized[i] {
					fixes++
				}
			}
		}
		return writeEnvLinesMode(ctx, h, path, sanitized, false)
	})
	if err == nil && fixes > 0 {
		InvalidateEnvCache()
	}
	return fixes, err
}
