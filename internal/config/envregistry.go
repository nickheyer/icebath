package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Optional variable shown on the keys page and in setup checklists, a nil URL or Password left unsaid
type EnvVar struct {
	Description string
	Prompt      string
	Help        string
	URL         *string
	Tools       []string
	Password    *bool
	Category    string
	Advanced    bool
}

// One variable a platform manifest declares, with whether the platform needs it and whether its value is a secret
type PlatformEnvVar struct {
	Name     string
	Required bool
	Secret   bool
	Var      EnvVar
}

// Variables one platform declares in its manifest, required ones first, under its folder name and manifest name
type PlatformEnvSet struct {
	Dir  string
	Name string
	Vars []PlatformEnvVar
}

// Suffixes that make a platform variable a secret unless its declaration says otherwise
func PlatformSecretSuffixes() []string {
	return []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_JSON"}
}

// Suffixes of platform variables that configure themselves, which setup forms leave out
func setupHiddenSuffixes() []string {
	return []string{
		"_HOME_CHANNEL", "_HOME_CHANNEL_NAME", "_HOME_CHANNEL_THREAD_ID", "_HOME_ADDRESS", "_ALLOW_ALL_USERS",
		"_REPLY_TO_MODE", "_REPLY_MODE", "_REQUIRE_MENTION", "_AUTO_THREAD", "_FREE_RESPONSE_CHANNELS",
		"_FREE_RESPONSE_ROOMS", "_ALLOWED_CHANNELS", "_PROXY",
	}
}

// Reports a variable setup forms hide because the platform configures it on its own, required ones still shown by callers
func IsSetupHiddenEnv(name string) bool {
	return slices.ContainsFunc(setupHiddenSuffixes(), func(suffix string) bool { return strings.HasSuffix(name, suffix) })
}

// Shape of an environment setting key, upper case letters, digits and underscores after a leading letter
var envSettingShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Optional variables of one process, core entries first, then platform and provider entries in the order they joined
type EnvRegistry struct {
	mu        sync.RWMutex
	order     []string
	entries   map[string]EnvVar
	core      map[string]bool
	providers map[string]bool
	launch    []string
	bundled   []string
}

// Registry holding the core entries and the entries of the platforms compiled into the program
func newEnvRegistry() *EnvRegistry {
	r := &EnvRegistry{entries: map[string]EnvVar{}, core: map[string]bool{}, providers: map[string]bool{}, launch: LaunchEnvNames()}
	for _, e := range coreEnvVars() {
		r.order = append(r.order, e.name)
		r.entries[e.name] = e.v
		r.core[e.name] = true
	}
	secrets := map[string]bool{}
	for _, set := range BundledPlatformEnv() {
		for _, v := range set.Vars {
			r.add(v.Name, v.Var)
			if v.Secret && v.Var.Category == "messaging" {
				secrets[upperText(v.Name)] = true
			}
		}
	}
	for name := range secrets {
		r.bundled = append(r.bundled, name)
	}
	slices.Sort(r.bundled)
	return r
}

// Registry of optional variables the env view, setup and the keys page read
func (s *Store) EnvRegistry() *EnvRegistry { return s.envs.registry }

// Adds an entry unless the name is already registered, the caller holding the write lock once shared
func (r *EnvRegistry) add(name string, v EnvVar) {
	if _, ok := r.entries[name]; ok {
		return
	}
	r.order = append(r.order, name)
	r.entries[name] = v
}

// Entry of a variable, ok false when it is not registered
func (r *EnvRegistry) Get(name string) (EnvVar, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.entries[name]
	return v, ok
}

// Registered names in the order they joined
func (r *EnvRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.order)
}

// Text of a manifest field, empty unless the field holds a non-empty string
func manifestText(entry *yaml.Node, key string) string {
	if n := childOf(entry, key); isStringNode(n) {
		return n.Value
	}
	return ""
}

// Variables a platform manifest declares, required ones first, each secret by its declaration or else by its name
func PlatformManifestEnv(dirName string, manifest *yaml.Node) PlatformEnvSet {
	set := PlatformEnvSet{Dir: dirName}
	manifest = resolveAlias(manifest)
	if manifest != nil && manifest.Kind == yaml.DocumentNode && len(manifest.Content) > 0 {
		manifest = manifest.Content[0]
	}
	if !isMapping(manifest) {
		return set
	}
	if name := childOf(manifest, "name"); name != nil {
		set.Name = displayText(name)
	}
	label := orText(childOf(manifest, "label"))
	if label == "" {
		label = orText(childOf(manifest, "name"))
	}
	if label == "" {
		label = dirName
	}
	for _, key := range []string{"requires_env", "optional_env"} {
		list := childOf(manifest, key)
		if !isSequence(list) {
			continue
		}
		for _, entry := range list.Content {
			meta := emptyMapping()
			name := ""
			switch {
			case isStringNode(entry):
				name = entry.Value
			case isMapping(entry):
				meta, name = entry, manifestText(entry, "name")
			}
			if name == "" {
				continue
			}
			password := childOf(meta, "password")
			secret := isSet(password) || isSet(childOf(meta, "secret"))
			if tag, text := scalarKind(orNull(password)); !secret && !(tag == tagBool && text == "false") {
				secret = slices.ContainsFunc(PlatformSecretSuffixes(), func(s string) bool { return strings.HasSuffix(upperText(name), s) })
			}
			v := EnvVar{Description: orText(childOf(meta, "description")), Prompt: orText(childOf(meta, "prompt")),
				Password: &secret, Category: orText(childOf(meta, "category"))}
			if v.Description == "" {
				v.Description = label + " configuration"
			}
			if v.Prompt == "" {
				v.Prompt = name
			}
			if url := orText(childOf(meta, "url")); url != "" {
				v.URL = &url
			}
			if v.Category == "" {
				v.Category = "messaging"
			}
			set.Vars = append(set.Vars, PlatformEnvVar{Name: name, Required: key == "requires_env", Secret: secret, Var: v})
		}
	}
	return set
}

// Adds the variables an installed platform plugin manifest declares, a name already registered keeping its entry
func (r *EnvRegistry) RegisterPlatformManifest(dirName string, manifest *yaml.Node) {
	set := PlatformManifestEnv(dirName, manifest)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range set.Vars {
		r.add(v.Name, v.Var)
	}
}

// Adds the key and base URL variables of one API key provider profile, core entries and earlier provider entries kept
func (r *EnvRegistry) RegisterProvider(label, signupURL string, envVars []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range envVars {
		if r.core[name] || r.providers[name] {
			continue
		}
		key := !strings.HasSuffix(name, "_BASE_URL") && !strings.HasSuffix(name, "_URL")
		v := EnvVar{Description: label + " base URL override", Prompt: label + " base URL (leave empty for default)",
			Password: &key, Category: "provider", Advanced: true}
		if key {
			v.Description, v.Prompt = label+" API key", label+" API key"
		}
		if signupURL != "" {
			v.URL = &signupURL
		}
		if _, ok := r.entries[name]; !ok {
			r.order = append(r.order, name)
		}
		r.entries[name] = v
		r.providers[name] = true
	}
}

// Adds names that choose the executable or arguments of an external process provider, which the env writer refuses
func (r *EnvRegistry) RegisterLaunchEnv(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if name != "" && !slices.Contains(r.launch, name) {
			r.launch = append(r.launch, name)
		}
	}
}

// Names the env writer refuses because they launch a provider program
func (r *EnvRegistry) launchNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.launch)
}

// Names declared by the core and by provider profiles, which no platform manifest reclassifies, sorted
func (r *EnvRegistry) CoreDeclaredNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.core)+len(r.providers))
	for name := range r.core {
		out = append(out, name)
	}
	for name := range r.providers {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Upper case secret messaging variables the given platforms declare, names the core declares left out, sorted
func (r *EnvRegistry) SecretPlatformEnv(sets []PlatformEnvSet) []string {
	core := map[string]bool{}
	for _, name := range r.CoreDeclaredNames() {
		core[upperText(name)] = true
	}
	var out []string
	for _, set := range sets {
		for _, v := range set.Vars {
			if name := upperText(v.Name); v.Secret && v.Var.Category == "messaging" && !core[name] {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Upper case secret variables of the platforms compiled into the program, names the core declares left out, sorted
func (r *EnvRegistry) BundledPlatformSecrets() []string {
	core := map[string]bool{}
	for _, name := range r.CoreDeclaredNames() {
		core[upperText(name)] = true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.DeleteFunc(slices.Clone(r.bundled), func(name string) bool { return core[name] })
}

// Reports a name the program enumerates, in the registry, among the extra env keys or self configuring
func (r *EnvRegistry) IsRegistered(name string) bool {
	_, ok := r.Get(name)
	return ok || slices.Contains(ExtraEnvKeys(), name) || IsSetupHiddenEnv(name)
}

// Reports a bare key config commands store in the env file, any upper snake name or registered name
func (r *EnvRegistry) IsSettingKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	return envSettingShape.MatchString(key) || r.IsRegistered(upperText(key))
}

// Removes the top level config copies of an env setting, as typed and in upper case, reporting whether any existed
func (e *Env) dropConfigCopies(ctx context.Context, key string) (bool, error) {
	_, path, err := e.s.configPath(ctx)
	if err != nil {
		return false, err
	}
	locked, release, err := e.s.lockWrite(ctx, path)
	if err != nil {
		return false, err
	}
	raw, err := e.s.requireReadable(locked, path)
	if err != nil {
		return false, errors.Join(err, release())
	}
	stale := false
	for _, name := range []string{key, upperText(key)} {
		if at := mappingIndex(raw, name); at >= 0 {
			removeSlot(pathHop{raw, at})
			stale = true
		}
	}
	if !stale {
		return false, release()
	}
	return true, errors.Join(e.s.writeState(locked, path, raw, true, ""), release())
}

// Stores an env setting in the home env file under its upper case name and drops its stale config copies
func (e *Env) SaveSetting(ctx context.Context, key, value string) error {
	name := upperText(key)
	if err := e.RequireWritable(ctx, name, "set"); err != nil {
		return err
	}
	if err := e.Set(ctx, name, value); err != nil {
		return err
	}
	_, err := e.dropConfigCopies(ctx, key)
	return err
}

// Removes an env setting from the home env file and any stale config copy, false when neither existed
func (e *Env) RemoveSetting(ctx context.Context, key string) (bool, error) {
	name := upperText(key)
	if err := e.RequireWritable(ctx, name, "remove"); err != nil {
		return false, err
	}
	removed, err := e.Remove(ctx, name)
	if err != nil {
		return false, err
	}
	dropped, err := e.dropConfigCopies(ctx, key)
	return dropped || removed, err
}

// Value an env setting resolves to, Stale marking a top level config copy the env file does not hold
type EnvSetting struct {
	Value *yaml.Node
	Stale bool
}

// Line telling the person a setting came from a stale top level config copy and how to converge it
func StaleSettingNote(key string) string {
	return fmt.Sprintf("  (note: %s is a stale top-level config.yaml copy; `icebath config set %s <value>` "+
		"moves it to .env, `icebath config unset %s` removes it)", key, key, key)
}

// Env setting as the gateway resolves it, else its stale top level config copy, ok false when absent
func (e *Env) ReadSetting(ctx context.Context, key string) (EnvSetting, bool, error) {
	if v, ok, err := e.Get(ctx, upperText(key)); ok || err != nil {
		return EnvSetting{Value: stringNode(v)}, ok, err
	}
	raw, err := e.s.RawDocumentReadonly(ctx)
	if err != nil {
		return EnvSetting{}, false, err
	}
	stale := childOf(raw.root, key)
	if stale == nil || isNull(stale) {
		return EnvSetting{}, false, nil
	}
	return EnvSetting{Value: plainCopy(stale), Stale: true}, true, nil
}
