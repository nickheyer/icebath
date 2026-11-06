package verify

import (
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Recipe is a runnable verification recipe whose command lists are shell strings run in the project root
type Recipe struct {
	Name          string
	Kind          string
	Bootstrap     []string
	Build         []string
	Test          []string
	Start         *string
	Port          *int
	PortTrue      bool
	ReadinessPath string
	Evidence      []string
}

// Recipe with the dataclass defaults, kind unknown and readiness path slash
func NewRecipe(name string) *Recipe {
	return &Recipe{Name: name, Kind: "unknown", ReadinessPath: "/"}
}

// Python str of the port, True when a manifest stored the bool true
func (r *Recipe) portText() string {
	if r.PortTrue {
		return "True"
	}
	if r.Port == nil {
		return "None"
	}
	return fmt.Sprint(*r.Port)
}

func strList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Manifest mapping with the grok field names in dataclass order
func (r *Recipe) ToDict() *pyjson.Object {
	var start, port any
	if r.Start != nil {
		start = *r.Start
	}
	if r.PortTrue {
		port = true
	} else if r.Port != nil {
		port = int64(*r.Port)
	}
	return pyjson.ObjectOf("name", r.Name, "kind", r.Kind, "bootstrap", strList(r.Bootstrap),
		"build", strList(r.Build), "test", strList(r.Test), "start", start, "port", port,
		"readinessPath", r.ReadinessPath, "evidence", strList(r.Evidence))
}

// Mapping view of a decoded JSON object or a Go map, false for any other value
func asDict(raw any) (func(string) any, bool) {
	switch d := raw.(type) {
	case *pyjson.Object:
		return func(k string) any { v, _ := d.Get(k); return v }, true
	case map[string]any:
		return func(k string) any { return d[k] }, true
	}
	return nil, false
}

// First truthy value of the keys like a chain of dict.get or, the last value otherwise
func firstOr(get func(string) any, keys ...string) any {
	var v any
	for _, k := range keys {
		if v = get(k); pyrepr.Truthy(v) {
			return v
		}
	}
	return v
}

// Stripped non empty strings of a string or a list, anything else giving none
func asStrings(value any) []string {
	var values []any
	switch v := value.(type) {
	case string:
		values = []any{v}
	case []any:
		values = v
	case []string:
		values = strList(v)
	}
	out := []string{}
	for _, v := range values {
		if s, ok := v.(string); ok && pystr.Strip(s) != "" {
			out = append(out, pystr.Strip(s))
		}
	}
	return out
}

// Tolerant recipe loader accepting these field names and the grok camelCase aliases, nil without a usable name
func RecipeFromDict(raw any) (*Recipe, error) {
	get, ok := asDict(raw)
	if !ok {
		return nil, nil
	}
	name, ok := firstOr(get, "name", "appLabel").(string)
	if !ok || pystr.Strip(name) == "" {
		return nil, nil
	}
	kind, ok := firstOr(get, "kind", "appKind").(string)
	if !ok || pystr.Strip(kind) == "" {
		kind = "unknown"
	}
	r := &Recipe{Name: pystr.Strip(name), Kind: pystr.Strip(kind), ReadinessPath: "/"}
	if start, ok := firstOr(get, "start", "startCommand").(string); ok && pystr.Strip(start) != "" {
		s := pystr.Strip(start)
		r.Start = &s
	}
	portRaw := firstOr(get, "port", "startPort")
	if s, ok := portRaw.(string); ok && pystr.StrIsDigit(pystr.Strip(s)) {
		n, err := pyint.Of(pystr.Strip(s))
		if err != nil {
			return nil, err
		}
		portRaw = n
	}
	if b, ok := portRaw.(bool); ok {
		r.PortTrue = b
		if b {
			one := 1
			r.Port = &one
		}
	} else if n, ok := pyint.Instance(portRaw); ok && n.Sign() > 0 && n.Cmp(big.NewInt(65536)) < 0 {
		p := int(n.Int64())
		r.Port = &p
	}
	if readiness, ok := firstOr(get, "readinessPath", "readiness_path").(string); ok && strings.HasPrefix(readiness, "/") {
		r.ReadinessPath = readiness
	}
	r.Bootstrap = asStrings(firstOr(get, "bootstrap", "installCommands"))
	r.Build = asStrings(firstOr(get, "build", "buildCommands"))
	r.Test = asStrings(firstOr(get, "test", "testCommands"))
	r.Evidence = asStrings(get("evidence"))
	return r, nil
}

// OSError of a failed filesystem call with the CPython errno text and quoted filename
type OSError struct {
	Class string
	Err   error
}

func (e *OSError) Error() string {
	var pe *fs.PathError
	if errors.As(e.Err, &pe) {
		return pyhttpx.OSErrorText(pe.Err) + ": " + pyrepr.Repr(pe.Path)
	}
	return pyhttpx.OSErrorText(e.Err)
}

func (e *OSError) Unwrap() error { return e.Err }

// Text read with utf-8-sig and universal newlines, nil when the read raises OSError
func readText(root, name string) (*string, error) {
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return nil, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	text = pystr.TranslateNewlines(text)
	return &text, nil
}

// package.json as a mapping, nil when it is absent, undecodable, malformed or not an object
func readPackageJSON(root string) *pyjson.Object {
	text, err := readText(root, "package.json")
	if err != nil {
		return nil
	}
	src := ""
	if text != nil {
		src = *text
	}
	parsed, err := pyjson.Loads(src)
	if err != nil {
		return nil
	}
	obj, _ := parsed.(*pyjson.Object)
	return obj
}

// pathlib exists, the ignored stat errors answering false and any other error raised
func exists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if pypath.PathlibIgnoredError(err) {
			return false, nil
		}
		return false, &OSError{Err: err}
	}
	return true, nil
}

var lockfileManagers = [][2]string{
	{"pnpm-lock.yaml", "pnpm"}, {"bun.lock", "bun"}, {"bun.lockb", "bun"}, {"yarn.lock", "yarn"},
	{"package-lock.json", "npm"}, {"uv.lock", "uv"}, {"poetry.lock", "poetry"}, {"Pipfile.lock", "pipenv"},
}

// First of names present under root, empty when none is
func firstExisting(root string, names ...string) (string, error) {
	for _, n := range names {
		ok, err := exists(filepath.Join(root, n))
		if err != nil || ok {
			return n, err
		}
	}
	return "", nil
}

// Package manager named by the first lockfile present, empty when none is
func DetectPackageManager(root string) (string, error) {
	for _, lm := range lockfileManagers {
		ok, err := exists(filepath.Join(root, lm[0]))
		if err != nil {
			return "", err
		}
		if ok {
			return lm[1], nil
		}
	}
	return "", nil
}

var (
	portFlagRE = mustPyre(`(?:--port|-p)\s+(\d{2,5})`)
	portEnvRE  = mustPyre(`\bPORT=(\d{2,5})\b`)
	makeTarget = mustPyre(`^([A-Za-z0-9_.-]+):(?:\s|$)`)
)

func mustPyre(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Port named by a --port or -p flag or a PORT assignment in a start command, nil when none is
func inferPortFromCommand(command any) (*int, error) {
	if !pyrepr.Truthy(command) {
		return nil, nil
	}
	s, ok := command.(string)
	if !ok {
		return nil, &TypeError{Msg: "expected string or bytes-like object, got '" + pyrepr.TypeName(command) + "'"}
	}
	for _, re := range []*regexp2.Regexp{portFlagRE, portEnvRE} {
		m, err := re.FindStringMatch(s)
		if err != nil {
			return nil, err
		}
		if m != nil {
			n, ok := pystr.IntText(m.GroupByNumber(1).String())
			if !ok {
				return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(m.GroupByNumber(1).String()))
			}
			p := int(n.Int64())
			return &p, nil
		}
	}
	return nil, nil
}

// TypeError is the Python TypeError a non string start script raises in port inference
type TypeError struct{ Msg string }

func (e *TypeError) Error() string { return e.Msg }

// Stripped non empty values in first occurrence order
func dedupe(values ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = pystr.Strip(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

var (
	scriptRunners  = map[string]string{"pnpm": "pnpm %s", "bun": "bun run %s", "yarn": "yarn %s"}
	nodeInstall    = map[string]string{"pnpm": "pnpm install", "bun": "bun install", "yarn": "yarn install"}
	nodeFrameworks = []struct {
		names       []string
		kind, label string
		port        int
	}{
		{[]string{"next"}, "nextjs", "Next.js", 3000}, {[]string{"@sveltejs/kit"}, "sveltekit", "SvelteKit", 5173},
		{[]string{"astro"}, "astro", "Astro", 4321},
		{[]string{"@remix-run/dev", "@remix-run/react"}, "remix", "Remix", 3000},
		{[]string{"react-scripts"}, "cra", "Create React App", 3000}, {[]string{"vite"}, "vite", "Vite", 5173},
	}
)

// Package script invocation for the package manager, npm run when it has no runner of its own
func scriptRunner(packageManager, entry string) string {
	format, ok := scriptRunners[packageManager]
	if !ok {
		format = "npm run %s"
	}
	return fmt.Sprintf(format, entry)
}

// Node recipe from package.json scripts and dependencies, the first framework dependency deciding kind and port
func detectNodeRecipe(root string, pkg *pyjson.Object) (*Recipe, error) {
	scripts := pyjson.NewObject()
	if raw, _ := pkg.Get("scripts"); raw != nil {
		if s, ok := raw.(*pyjson.Object); ok {
			scripts = s
		}
	}
	deps := map[string]bool{}
	for _, key := range []string{"dependencies", "devDependencies"} {
		if section, ok := pkgGet(pkg, key).(*pyjson.Object); ok {
			for _, k := range section.Keys {
				deps[k] = true
			}
		}
	}
	packageManager, err := DetectPackageManager(root)
	if err != nil {
		return nil, err
	}
	kind, label, defaultPort := "node", "Node.js app", 0
	for _, fw := range nodeFrameworks {
		if hasAny(deps, fw.names) {
			kind, label, defaultPort = fw.kind, fw.label, fw.port
			break
		}
	}
	runners := func(names ...string) []string {
		var cmds []string
		for _, s := range names {
			if pyrepr.Truthy(pkgGet(scripts, s)) {
				cmds = append(cmds, scriptRunner(packageManager, s))
			}
		}
		return dedupe(cmds...)
	}
	r := &Recipe{Name: label, Kind: kind, ReadinessPath: "/"}
	for _, s := range []string{"dev", "start"} {
		if pyrepr.Truthy(pkgGet(scripts, s)) {
			start := runners(s)[0]
			r.Start = &start
			port, err := inferPortFromCommand(pkgGet(scripts, s))
			if err != nil {
				return nil, err
			}
			if port == nil || *port == 0 {
				port = nil
				if defaultPort != 0 {
					port = &defaultPort
				}
			}
			r.Port = port
			break
		}
	}
	install, ok := nodeInstall[packageManager]
	if !ok {
		install = "npm install"
	}
	r.Bootstrap = []string{install}
	r.Build = runners("build", "typecheck")
	r.Test = runners("test", "check", "lint")
	scriptNames := strings.Join(scripts.Keys, ", ")
	if scriptNames == "" {
		scriptNames = "(none)"
	}
	pmLine := ""
	if packageManager != "" {
		pmLine = "Package manager: " + packageManager
	}
	r.Evidence = dedupe("Detected package.json", pmLine, "Scripts: "+scriptNames)
	return r, nil
}

func pkgGet(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

func hasAny(set map[string]bool, names []string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

var pythonInstall = map[string]string{"uv": "uv sync", "poetry": "poetry install", "pipenv": "pipenv install"}

// Python recipe with Django, then FastAPI or Uvicorn, then Flask precedence, nil without a Python marker
func detectPythonRecipe(root string) (*Recipe, error) {
	pyproject, err := readText(root, "pyproject.toml")
	if err != nil {
		return nil, err
	}
	requirements, err := readText(root, "requirements.txt")
	if err != nil {
		return nil, err
	}
	manage, err := exists(filepath.Join(root, "manage.py"))
	if err != nil {
		return nil, err
	}
	hasPyproject := pyproject != nil && *pyproject != ""
	hasRequirements := requirements != nil && *requirements != ""
	if !(hasPyproject || hasRequirements || manage) {
		setup, err := exists(filepath.Join(root, "setup.py"))
		if err != nil || !setup {
			return nil, err
		}
	}
	lower := pystr.Lower(deref(pyproject) + "\n" + deref(requirements))
	pm, err := DetectPackageManager(root)
	if err != nil {
		return nil, err
	}
	install := pythonInstall[pm]
	if install == "" {
		if hasPyproject && !hasRequirements {
			install = "pip install -e ."
		} else {
			install = "pip install -r requirements.txt"
		}
	}
	tests, err := exists(filepath.Join(root, "tests"))
	if err != nil {
		return nil, err
	}
	pytestOrEmpty := []string{}
	if tests {
		pytestOrEmpty = []string{"pytest"}
	}
	r := &Recipe{Kind: "python", ReadinessPath: "/", Bootstrap: []string{install}}
	switch {
	case manage || strings.Contains(lower, "django"):
		ev := "Detected Django dependency"
		if manage {
			ev = "Detected manage.py"
		}
		r.Name, r.Kind, r.Test = "Django app", "django", []string{"python manage.py test"}
		r.Start, r.Port = strPtr("python manage.py runserver 0.0.0.0:8000"), intPtr(8000)
		r.Evidence = []string{ev}
		if hasPyproject {
			r.Evidence = append(r.Evidence, "Detected pyproject.toml")
		}
	case strings.Contains(lower, "fastapi") || strings.Contains(lower, "uvicorn"):
		file, err := firstExisting(root, "main.py", "app.py")
		if err != nil {
			return nil, err
		}
		if file == "" {
			file = "main.py"
		}
		appModule := strings.TrimSuffix(file, ".py") + ":app"
		r.Name, r.Kind, r.Test = "FastAPI app", "fastapi", pytestOrEmpty
		r.Start, r.Port = strPtr("uvicorn "+appModule+" --host 0.0.0.0 --port 8000"), intPtr(8000)
		r.Evidence = []string{"Detected Python project", "Detected FastAPI/Uvicorn dependency"}
	case strings.Contains(lower, "flask"):
		file, err := firstExisting(root, "app.py", "main.py")
		if err != nil {
			return nil, err
		}
		if file == "" {
			file = "app.py"
		}
		r.Name, r.Kind, r.Test = "Flask app", "flask", pytestOrEmpty
		r.Start, r.Port = strPtr("flask --app "+file+" run --host 0.0.0.0 --port 5000"), intPtr(5000)
		r.Evidence = []string{"Detected Python project", "Detected Flask dependency"}
	default:
		r.Name, r.Test = "Python project", pytestOrEmpty
		if len(r.Test) == 0 {
			r.Test = []string{"python -m unittest discover"}
		}
		r.Evidence = []string{"Detected Python project"}
	}
	return r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }

var simpleToolchains = []struct{ manifest, label, kind, build, test, start, entry string }{
	{"go.mod", "Go project", "go", "go build ./...", "go test ./...", "go run .", "main.go"},
	{"Cargo.toml", "Rust project", "rust", "cargo build", "cargo test", "cargo run", filepath.Join("src", "main.rs")},
}

// Go or Rust recipe from its single manifest, a start command only when the entry file exists
func detectSimpleRecipe(root string) (*Recipe, error) {
	for _, tc := range simpleToolchains {
		ok, err := exists(filepath.Join(root, tc.manifest))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		r := &Recipe{Name: tc.label, Kind: tc.kind, Build: []string{tc.build}, Test: []string{tc.test},
			ReadinessPath: "/", Evidence: []string{"Detected " + tc.manifest}}
		entry, err := exists(filepath.Join(root, tc.entry))
		if err != nil {
			return nil, err
		}
		if entry {
			r.Start = strPtr(tc.start)
		}
		return r, nil
	}
	return nil, nil
}

// Maven recipe from pom.xml, else Gradle preferring the wrapper, nil without either
func detectJavaRecipe(root string) (*Recipe, error) {
	pom, err := exists(filepath.Join(root, "pom.xml"))
	if err != nil {
		return nil, err
	}
	if pom {
		return &Recipe{Name: "Maven project", Kind: "maven", Build: []string{"mvn package"}, Test: []string{"mvn test"},
			ReadinessPath: "/", Evidence: []string{"Detected pom.xml"}}, nil
	}
	file, err := firstExisting(root, "build.gradle", "build.gradle.kts")
	if err != nil || file == "" {
		return nil, err
	}
	gradle := "gradle"
	wrapper, err := exists(filepath.Join(root, "gradlew"))
	if err != nil {
		return nil, err
	}
	if wrapper {
		gradle = "./gradlew"
	}
	return &Recipe{Name: "Gradle project", Kind: "gradle", Build: []string{gradle + " build"}, Test: []string{gradle + " test"},
		ReadinessPath: "/", Evidence: []string{"Detected Gradle build file"}}, nil
}

var makePhaseTargets = []struct {
	phase string
	names []string
}{
	{"bootstrap", []string{"install", "setup", "bootstrap"}},
	{"build", []string{"build", "compile"}},
	{"test", []string{"test", "check"}},
	{"start", []string{"run", "start", "serve", "dev"}},
}

// Makefile recipe whose phases take the first present candidate target, nil without a Makefile
func detectMakeRecipe(root string) (*Recipe, error) {
	makefile, err := readText(root, "Makefile")
	if err != nil || makefile == nil {
		return nil, err
	}
	var targets []string
	present := map[string]bool{}
	for _, line := range pystr.Splitlines(*makefile, false) {
		m, err := makeTarget.FindStringMatch(line)
		if err != nil {
			return nil, err
		}
		if m != nil {
			t := m.GroupByNumber(1).String()
			targets = append(targets, t)
			present[t] = true
		}
	}
	picked := map[string][]string{}
	for _, pt := range makePhaseTargets {
		picked[pt.phase] = []string{}
		for _, n := range pt.names {
			if present[n] {
				picked[pt.phase] = []string{"make " + n}
				break
			}
		}
	}
	names := strings.Join(targets, ", ")
	if names == "" {
		names = "(none)"
	}
	r := &Recipe{Name: "Makefile-driven project", Kind: "make", ReadinessPath: "/",
		Bootstrap: picked["bootstrap"], Build: picked["build"], Test: picked["test"],
		Evidence: []string{"Detected Makefile", "Targets: " + names}}
	if len(picked["start"]) > 0 {
		r.Start = strPtr(picked["start"][0])
	}
	return r, nil
}

var composeFiles = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

// docker compose recipe from the first compose file present, nil without one
func detectComposeRecipe(root string) (*Recipe, error) {
	file, err := firstExisting(root, composeFiles...)
	if err != nil || file == "" {
		return nil, err
	}
	return &Recipe{Name: "docker-compose project", Kind: "compose", Build: []string{"docker compose build"},
		Start: strPtr("docker compose up"), ReadinessPath: "/", Evidence: []string{"Detected " + file}}, nil
}

// Recipe for root in grok order, package.json then Python, Go, Rust, Java, Makefile and compose, nil when none matches
func DetectRecipe(root string) (*Recipe, error) {
	if pkg := readPackageJSON(root); pkg != nil {
		return detectNodeRecipe(root, pkg)
	}
	for _, detect := range []func(string) (*Recipe, error){
		detectPythonRecipe, detectSimpleRecipe, detectJavaRecipe, detectMakeRecipe, detectComposeRecipe,
	} {
		if r, err := detect(root); err != nil || r != nil {
			return r, err
		}
	}
	return nil, nil
}
