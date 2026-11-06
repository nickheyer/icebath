package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// PlatformDeclarationError is a declaration parse or validation failure
type PlatformDeclarationError struct{ Msg string }

func (e *PlatformDeclarationError) Error() string { return e.Msg }

// PlatformAppSpec holds one app definition per OS family in declaration order
type PlatformAppSpec struct {
	PerOS map[string]PlatformAppDef
	Order []string
}

// App definition declared for an OS family
func (s *PlatformAppSpec) ForOS(osFamily string) (PlatformAppDef, bool) {
	d, ok := s.PerOS[osFamily]
	return d, ok
}

// PlatformRequiresSpec is what a server needs before it is offered
type PlatformRequiresSpec struct {
	App        bool
	MinVersion *string
	GPU        *string
}

// PlatformDeclaration is a parsed app and requires declaration of one server or skill
type PlatformDeclaration struct {
	Name     string
	App      *PlatformAppSpec
	Requires PlatformRequiresSpec
}

func (d *PlatformDeclaration) RequiresApp() bool    { return d.Requires.App }
func (d *PlatformDeclaration) MinVersion() *string  { return d.Requires.MinVersion }
func (d *PlatformDeclaration) RequiredGPU() *string { return d.Requires.GPU }

// App definition for an OS family, false without an app block or a block for that OS
func (d *PlatformDeclaration) AppFor(osFamily string) (PlatformAppDef, bool) {
	if d.App == nil {
		return PlatformAppDef{}, false
	}
	return d.App.ForOS(osFamily)
}

var (
	platformAppOSFamilies   = []string{"win32", "darwin", "linux"}
	platformAppPresence     = []string{"executable", "bundle"}
	platformAppVersionKinds = map[string]string{"pe_resource": "win32", "uninstall_registry": "win32", "plist": "darwin", "none": ""}
	platformLivenessKinds   = []string{"server_json", "none"}
	platformVersionRe       = regexp.MustCompile(`^\p{Nd}+(\.\p{Nd}+)*$`)
	platformDriveRe         = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	platformGPULabels       = map[string]string{"nvidia": "an NVIDIA GPU"}
)

// User facing phrase naming a requires.gpu value, empty for None
func PlatformGPULabel(gpu string) string {
	if label, ok := platformGPULabels[gpu]; ok {
		return label
	}
	return "a supported GPU"
}

// Python tuple repr of strings
func pyTupleRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyrepr.StrRepr(s)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// Python repr of the sorted list of strings
func pySortedListRepr(items []string) string {
	sorted := slices.Clone(items)
	slices.Sort(sorted)
	parts := make([]string, len(sorted))
	for i, s := range sorted {
		parts[i] = pyrepr.StrRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Ordered keys and lookup of a decoded mapping, ok false when the value is not a dict
func platformMapping(raw any) ([]string, func(string) (any, bool), bool) {
	switch m := raw.(type) {
	case *pyjson.Object:
		return m.Keys, m.Get, true
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys, func(k string) (any, bool) { v, ok := m[k]; return v, ok }, true
	}
	return nil, nil, false
}

func platformRequireMapping(where, key string, raw any) ([]string, func(string) (any, bool), error) {
	keys, get, ok := platformMapping(raw)
	if !ok {
		return nil, nil, &PlatformDeclarationError{fmt.Sprintf("%s: '%s' must be a mapping", where, key)}
	}
	return keys, get, nil
}

// dict.get with a default
func platformGet(get func(string) (any, bool), key string, def any) any {
	if v, ok := get(key); ok {
		return v
	}
	return def
}

// True for a path rooted at a drive, slash, home or variable, never with .. or a URL scheme
func platformLocationIsRooted(location, osf string) bool {
	if slices.Contains(strings.Split(strings.ReplaceAll(location, `\`, "/"), "/"), "..") || strings.Contains(location, "://") {
		return false
	}
	if strings.HasPrefix(location, "~") || strings.HasPrefix(location, "%") || strings.HasPrefix(location, "$") {
		return true
	}
	if osf == "win32" {
		return platformDriveRe.MatchString(location)
	}
	return strings.HasPrefix(location, "/")
}

// True for a stripped relative name without empty parts, .., a drive colon, or dirs unless allowed
func platformIsRelativeName(value any, allowDirs bool) bool {
	s, ok := value.(string)
	if !ok || pystr.Strip(s) == "" || strings.Contains(s, "://") {
		return false
	}
	parts := strings.Split(strings.ReplaceAll(pystr.Strip(s), `\`, "/"), "/")
	return !slices.Contains(parts, "..") && !slices.Contains(parts, "") && (allowDirs || len(parts) == 1) && !strings.Contains(parts[0], ":")
}

func platformLocationKindNames() []string {
	names := make([]string, 0, len(platformLocationKinds))
	for k := range platformLocationKinds {
		names = append(names, k)
	}
	return names
}

// One location entry, a string path where star stands for a versioned folder or a kind mapping
func platformParseLocation(where, osf, presence, label string, raw any) (PlatformAppLocation, error) {
	if s, ok := raw.(string); ok {
		path := pystr.Strip(s)
		if path == "" {
			return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s is required", where, label)}
		}
		if !platformLocationIsRooted(path, osf) || strings.Contains(path, "**") {
			return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s must be absolute or start with ~ / %%VAR%% / $VAR, without '..', '**' or a URL scheme", where, label)}
		}
		return PlatformAppLocation{Kind: "path", Value: path}, nil
	}
	_, get, isMap := platformMapping(raw)
	var kind string
	if isMap {
		kind, _ = platformGet(get, "kind", nil).(string)
	}
	spec, known := platformLocationKinds[kind]
	if !isMap || !known {
		return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s must be a path or a mapping with kind one of %s", where, label, pySortedListRepr(platformLocationKindNames()))}
	}
	if spec.OnlyOn != "" && spec.OnlyOn != osf {
		return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s kind %s is only valid under app.%s", where, label, pyrepr.StrRepr(kind), spec.OnlyOn)}
	}
	if spec.Yields != presence {
		return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s kind %s needs app.%s.presence %s", where, label, pyrepr.StrRepr(kind), osf, spec.Yields)}
	}
	value := platformGet(get, spec.Key, nil)
	if !platformIsRelativeName(value, false) {
		return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s.%s must be a bare name", where, label, spec.Key)}
	}
	file := platformGet(get, "file", "")
	if spec.NeedsFile && !platformIsRelativeName(file, true) {
		return PlatformAppLocation{}, &PlatformDeclarationError{fmt.Sprintf("%s: %s.file must be a relative path inside the install folder", where, label)}
	}
	return PlatformAppLocation{Kind: kind, Value: pystr.Strip(value.(string)), File: pystr.Strip(pyrepr.Str(file))}, nil
}

// str(value or default) with Python truthiness
func platformStrOr(v any, def string) string {
	if !pyTruthy(v) {
		return def
	}
	return pyrepr.Str(v)
}

// Python TypeError of a dict or set membership test on an unhashable value
func platformHashable(v any) error {
	switch v.(type) {
	case []any:
		return fmt.Errorf("unhashable type: 'list'")
	case map[string]any, *pyjson.Object:
		return fmt.Errorf("unhashable type: 'dict'")
	}
	return nil
}

// One app.<os> block with presence, locations, version source and liveness source
func platformParseAppOS(where, name, osf string, raw any) (PlatformAppDef, error) {
	_, get, err := platformRequireMapping(where, "app."+osf, raw)
	if err != nil {
		return PlatformAppDef{}, err
	}
	presence, _ := platformGet(get, "presence", nil).(string)
	if !slices.Contains(platformAppPresence, presence) {
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.presence must be one of %s", where, osf, pyTupleRepr(platformAppPresence))}
	}
	var locations []PlatformAppLocation
	switch location := platformGet(get, "location", nil).(type) {
	case []any:
		if len(location) == 0 {
			return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.location is required", where, osf)}
		}
		for i, item := range location {
			loc, err := platformParseLocation(where, osf, presence, fmt.Sprintf("app.%s.location[%d]", osf, i), item)
			if err != nil {
				return PlatformAppDef{}, err
			}
			locations = append(locations, loc)
		}
	case string:
		loc, err := platformParseLocation(where, osf, presence, fmt.Sprintf("app.%s.location", osf), location)
		if err != nil {
			return PlatformAppDef{}, err
		}
		locations = []PlatformAppLocation{loc}
	default:
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.location is required", where, osf)}
	}
	version := platformGet(get, "version", nil)
	if !pyTruthy(version) {
		version = map[string]any{"kind": "none"}
	}
	_, vget, err := platformRequireMapping(where, "app."+osf+".version", version)
	if err != nil {
		return PlatformAppDef{}, err
	}
	rawKind := platformGet(vget, "kind", "none")
	if err := platformHashable(rawKind); err != nil {
		return PlatformAppDef{}, err
	}
	vkind, _ := rawKind.(string)
	onlyOn, known := platformAppVersionKinds[vkind]
	if _, isStr := rawKind.(string); !isStr || !known {
		kinds := make([]string, 0, len(platformAppVersionKinds))
		for k := range platformAppVersionKinds {
			kinds = append(kinds, k)
		}
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.version.kind must be one of %s", where, osf, pySortedListRepr(kinds))}
	}
	if onlyOn != "" && onlyOn != osf {
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.version.kind %s is only valid under app.%s", where, osf, pyrepr.StrRepr(vkind), onlyOn)}
	}
	varg := platformStrOr(platformGet(vget, "display_name_prefix", nil), "")
	if vkind == "uninstall_registry" && varg == "" {
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.version.display_name_prefix is required for uninstall_registry", where, osf)}
	}
	liveness := platformGet(get, "liveness", nil)
	if !pyTruthy(liveness) {
		liveness = map[string]any{"kind": "none"}
	}
	_, lget, err := platformRequireMapping(where, "app."+osf+".liveness", liveness)
	if err != nil {
		return PlatformAppDef{}, err
	}
	lkind, isStr := platformGet(lget, "kind", "none").(string)
	if !isStr || !slices.Contains(platformLivenessKinds, lkind) {
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.liveness.kind must be one of %s", where, osf, pyTupleRepr(platformLivenessKinds))}
	}
	lpath := platformStrOr(platformGet(lget, "path", nil), "")
	if lkind == "server_json" && lpath == "" {
		return PlatformAppDef{}, &PlatformDeclarationError{fmt.Sprintf("%s: app.%s.liveness.path is required for server_json", where, osf)}
	}
	return PlatformAppDef{
		AppID: name, OSFamily: osf, Presence: presence, Locations: locations,
		VersionKind: vkind, VersionArg: varg,
		LivenessKind: lkind, LivenessPath: lpath,
		LivenessPIDKey:   platformStrOr(platformGet(lget, "pid_key", nil), "pid"),
		LivenessURLKey:   platformStrOr(platformGet(lget, "url_key", nil), "http"),
		LivenessTokenKey: platformStrOr(platformGet(lget, "token_key", nil), "token"),
		EndpointPath:     platformStrOr(platformGet(lget, "endpoint_path", nil), "/mcp"),
	}, nil
}

// Parses an already decoded app block into one definition per OS, nil when absent
func ParsePlatformApp(raw any, name, where string) (*PlatformAppSpec, error) {
	if raw == nil {
		return nil, nil
	}
	keys, get, err := platformRequireMapping(where, "app", raw)
	if err != nil {
		return nil, err
	}
	var unknown []string
	for _, k := range keys {
		if !slices.Contains(platformAppOSFamilies, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, &PlatformDeclarationError{fmt.Sprintf("%s: app has unknown OS keys %s; use %s", where, pySortedListRepr(unknown), pyTupleRepr(platformAppOSFamilies))}
	}
	if len(keys) == 0 {
		return nil, &PlatformDeclarationError{fmt.Sprintf("%s: app needs at least one OS block", where)}
	}
	spec := &PlatformAppSpec{PerOS: map[string]PlatformAppDef{}}
	for _, osf := range keys {
		block, _ := get(osf)
		def, err := platformParseAppOS(where, name, osf, block)
		if err != nil {
			return nil, err
		}
		spec.PerOS[osf] = def
		spec.Order = append(spec.Order, osf)
	}
	return spec, nil
}

// Parses the requires block, cross checking version policy against the parsed app
func ParsePlatformRequires(raw any, app *PlatformAppSpec, where string) (PlatformRequiresSpec, error) {
	if raw == nil {
		return PlatformRequiresSpec{}, nil
	}
	keys, get, err := platformRequireMapping(where, "requires", raw)
	if err != nil {
		return PlatformRequiresSpec{}, err
	}
	var unknown []string
	for _, k := range keys {
		if k != "app" && k != "min_version" && k != "gpu" {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires has unknown keys %s", where, pySortedListRepr(unknown))}
	}
	var gpu *string
	if rawGPU := platformGet(get, "gpu", nil); rawGPU != nil {
		s, ok := rawGPU.(string)
		if _, known := platformGPULabels[s]; !ok || !known {
			names := make([]string, 0, len(platformGPULabels))
			for k := range platformGPULabels {
				names = append(names, k)
			}
			return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires.gpu must be one of %s", where, pySortedListRepr(names))}
		}
		gpu = &s
	}
	needsApp, ok := platformGet(get, "app", false).(bool)
	if !ok {
		return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires.app must be a boolean", where)}
	}
	if needsApp && app == nil {
		return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires.app is true but the declaration has no 'app' block", where)}
	}
	var minVersion *string
	if rawMin := platformGet(get, "min_version", nil); rawMin != nil {
		s, ok := rawMin.(string)
		if !ok || !platformVersionRe.MatchString(s) {
			return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires.min_version must be a dotted numeric string", where)}
		}
		if !needsApp {
			return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires.min_version needs requires.app: true", where)}
		}
		families := slices.Sorted(func(yield func(string) bool) {
			for k := range app.PerOS {
				if !yield(k) {
					return
				}
			}
		})
		for _, osf := range families {
			if app.PerOS[osf].VersionKind == "none" && (osf != "linux" || len(app.PerOS) == 1) {
				return PlatformRequiresSpec{}, &PlatformDeclarationError{fmt.Sprintf("%s: requires.min_version needs a version source under app.%s (kind is none)", where, osf)}
			}
		}
		minVersion = &s
	}
	return PlatformRequiresSpec{App: needsApp, MinVersion: minVersion, GPU: gpu}, nil
}

// Parses both blocks of one declaration, where labelling every error message
func ParsePlatformDeclaration(name string, rawApp, rawRequires any, where string) (*PlatformDeclaration, error) {
	app, err := ParsePlatformApp(rawApp, name, where)
	if err != nil {
		return nil, err
	}
	requires, err := ParsePlatformRequires(rawRequires, app, where)
	if err != nil {
		return nil, err
	}
	return &PlatformDeclaration{Name: name, App: app, Requires: requires}, nil
}

var platformRegistry = struct {
	sync.Mutex
	decls    map[string]*PlatformDeclaration
	onChange func()
}{decls: map[string]*PlatformDeclaration{}}

// Installs the single hook run after every registry mutation, nil removing it
func SetPlatformDeclarationOnChange(fn func()) {
	platformRegistry.Lock()
	platformRegistry.onChange = fn
	platformRegistry.Unlock()
}

func platformDeclarationsChanged() {
	platformRegistry.Lock()
	fn := platformRegistry.onChange
	platformRegistry.Unlock()
	if fn != nil {
		fn()
	}
}

// Records which declaration gates a server name
func RegisterPlatformDeclaration(serverName string, decl *PlatformDeclaration) {
	platformRegistry.Lock()
	platformRegistry.decls[serverName] = decl
	platformRegistry.Unlock()
	platformDeclarationsChanged()
}

// Removes the declaration registered for a server name
func UnregisterPlatformDeclaration(serverName string) {
	platformRegistry.Lock()
	delete(platformRegistry.decls, serverName)
	platformRegistry.Unlock()
	platformDeclarationsChanged()
}

// Declaration registered for a server name, nil when none
func LookupPlatformDeclaration(serverName string) *PlatformDeclaration {
	platformRegistry.Lock()
	defer platformRegistry.Unlock()
	return platformRegistry.decls[serverName]
}

// Drops every registration
func ClearPlatformDeclarations() {
	platformRegistry.Lock()
	clear(platformRegistry.decls)
	platformRegistry.Unlock()
	platformDeclarationsChanged()
}
