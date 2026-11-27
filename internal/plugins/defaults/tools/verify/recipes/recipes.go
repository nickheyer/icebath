// Package recipes finds how a project is built, checked and started, from a saved manifest or from its files, and runs a verification pass
// of it: bootstrap, build and test, then the app started in the background until it answers, then stopped
package recipes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/strs"
)

// How a project is verified: commands run in its root as shell lines, phase by phase, and the app it starts with where it answers
type Recipe struct {
	Name      string
	Kind      string
	Bootstrap []string
	Build     []string
	Test      []string
	// Command that starts the app in the foreground, none when it starts none
	Start string
	// Port it answers on, 0 when not known
	Port int
	// Path readiness is asked of, starting with a slash
	ReadinessPath string
	// What the detection found, line by line
	Evidence []string
}

// Recipe as a manifest keeps it
type recipeFile struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Bootstrap     []string `json:"bootstrap"`
	Build         []string `json:"build"`
	Test          []string `json:"test"`
	Start         *string  `json:"start"`
	Port          *int     `json:"port"`
	ReadinessPath string   `json:"readinessPath"`
	Evidence      []string `json:"evidence"`
}

func (r *Recipe) MarshalJSON() ([]byte, error) {
	out := recipeFile{Name: r.Name, Kind: r.Kind, Bootstrap: strs.List(r.Bootstrap), Build: strs.List(r.Build), Test: strs.List(r.Test), ReadinessPath: r.ReadinessPath,
		Evidence: strs.List(r.Evidence)}
	if r.Start != "" {
		out.Start = &r.Start
	}
	if r.Port != 0 {
		out.Port = &r.Port
	}
	return json.Marshal(out)
}

// First of the keys whose value is set and not empty, as a manifest may name a field the way grok writes it
func field(raw map[string]any, keys ...string) any {
	var value any
	for _, key := range keys {
		if value = raw[key]; set(value) {
			return value
		}
	}
	return value
}

func set(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case bool:
		return v
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// Trimmed lines of a text or a list of them, empty ones and anything else left out
func lines(value any) []string {
	var items []any
	switch v := value.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	}
	out := []string{}
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}

// Recipe a manifest holds, its fields under these names or those grok writes; nil without a name
func parse(raw map[string]any) *Recipe {
	name, _ := field(raw, "name", "appLabel").(string)
	if strings.TrimSpace(name) == "" {
		return nil
	}
	kind, _ := field(raw, "kind", "appKind").(string)
	if strings.TrimSpace(kind) == "" {
		kind = "unknown"
	}
	r := &Recipe{Name: strings.TrimSpace(name), Kind: strings.TrimSpace(kind), ReadinessPath: "/"}
	if start, ok := field(raw, "start", "startCommand").(string); ok {
		r.Start = strings.TrimSpace(start)
	}
	switch port := field(raw, "port", "startPort").(type) {
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(port)); err == nil && n > 0 && n < 65536 {
			r.Port = n
		}
	case float64:
		if port == float64(int(port)) && port > 0 && port < 65536 {
			r.Port = int(port)
		}
	}
	if path, ok := field(raw, "readinessPath", "readiness_path").(string); ok && strings.HasPrefix(path, "/") {
		r.ReadinessPath = path
	}
	r.Bootstrap = lines(field(raw, "bootstrap", "installCommands"))
	r.Build = lines(field(raw, "build", "buildCommands"))
	r.Test = lines(field(raw, "test", "testCommands"))
	r.Evidence = lines(raw["evidence"])
	return r
}

// Version of the manifest's wrapper
const manifestVersion = 1

// Manifest of a project's recipe, under its root
func ManifestPath(root string) string {
	return filepath.Join(root, ".icebath", "environment.json")
}

// Text of a file with a leading byte order mark dropped and line ends made \n
func text(data []byte) string {
	text := strings.TrimPrefix(string(data), "\ufeff")
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Recipe the project's manifest saves, in its wrapper or bare; nil when there is none or it can't be read as one
func LoadManifest(root string) *Recipe {
	data, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		return nil
	}
	var manifest map[string]any
	if json.Unmarshal([]byte(text(data)), &manifest) != nil {
		return nil
	}
	if wrapped, ok := manifest["recipe"]; ok {
		inner, _ := wrapped.(map[string]any)
		return parse(inner)
	}
	return parse(manifest)
}

// Saves the recipe as the project's manifest, in its versioned wrapper: the path it went to
func SaveManifest(root string, recipe *Recipe) (string, error) {
	path := ManifestPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	payload := struct {
		Version int     `json:"version"`
		Recipe  *Recipe `json:"recipe"`
		Updated string  `json:"updatedAt"`
	}{manifestVersion, recipe, time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		return "", err
	}
	data := out.Bytes()
	if runtime.GOOS == "windows" {
		data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	}
	if err := os.WriteFile(path, data, 0o666); err != nil {
		return "", err
	}
	return path, nil
}

// Recipe of the project: the one its manifest saves, else the one its files make; and which it is, manifest or detected
func LoadOrDetect(root string) (*Recipe, string, error) {
	if saved := LoadManifest(root); saved != nil {
		return saved, "manifest", nil
	}
	detected, err := Detect(root)
	return detected, "detected", err
}

// Whether the path is there; a failure to look other than its absence is an error
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return false, nil
	}
	return false, err
}

// First of the names there under the root, empty when none is
func firstThere(root string, names ...string) (string, error) {
	for _, name := range names {
		if ok, err := exists(filepath.Join(root, name)); err != nil || ok {
			return name, err
		}
	}
	return "", nil
}

// Lockfiles, the first found naming the project's package manager
var lockfiles = [][2]string{
	{"pnpm-lock.yaml", "pnpm"}, {"bun.lock", "bun"}, {"bun.lockb", "bun"}, {"yarn.lock", "yarn"},
	{"package-lock.json", "npm"}, {"uv.lock", "uv"}, {"poetry.lock", "poetry"}, {"Pipfile.lock", "pipenv"},
}

// Package manager the first lockfile there names, empty when none is
func packageManager(root string) (string, error) {
	for _, lockfile := range lockfiles {
		ok, err := exists(filepath.Join(root, lockfile[0]))
		if err != nil {
			return "", err
		}
		if ok {
			return lockfile[1], nil
		}
	}
	return "", nil
}

var (
	// Port a start command names, by flag or by assignment
	portFlag   = regexp.MustCompile(`(?:--port|-p)\s+([0-9]{2,5})`)
	portAssign = regexp.MustCompile(`\bPORT=([0-9]{2,5})\b`)
	// Target a Makefile line defines
	makeTarget = regexp.MustCompile(`^([A-Za-z0-9_.-]+):(?:\s|$)`)
)

// Port a start command names, 0 when it names none
func portOf(command string) int {
	for _, re := range []*regexp.Regexp{portFlag, portAssign} {
		if m := re.FindStringSubmatch(command); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// Trimmed values that are not empty, each once in the order first seen
func dedupe(values ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

var (
	scriptRunners = map[string]string{"pnpm": "pnpm %s", "bun": "bun run %s", "yarn": "yarn %s"}
	nodeInstalls  = map[string]string{"pnpm": "pnpm install", "bun": "bun install", "yarn": "yarn install"}
	// Frameworks by the dependencies that mark them, the first found deciding the kind and the port
	nodeFrameworks = []struct {
		packages    []string
		kind, label string
		port        int
	}{
		{[]string{"next"}, "nextjs", "Next.js", 3000},
		{[]string{"@sveltejs/kit"}, "sveltekit", "SvelteKit", 5173},
		{[]string{"astro"}, "astro", "Astro", 4321},
		{[]string{"@remix-run/dev", "@remix-run/react"}, "remix", "Remix", 3000},
		{[]string{"react-scripts"}, "cra", "Create React App", 3000},
		{[]string{"vite"}, "vite", "Vite", 5173},
	}
)

// Command running a package script with the package manager, npm run for one with no runner of its own
func runScript(manager, script string) string {
	format, ok := scriptRunners[manager]
	if !ok {
		format = "npm run %s"
	}
	return fmt.Sprintf(format, script)
}

// Scripts of a package.json in the order it lists them
type scripts struct {
	names  []string
	values map[string]any
}

func (s *scripts) UnmarshalJSON(data []byte) error {
	s.values = map[string]any{}
	if err := json.Unmarshal(data, &s.values); err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.Token(); err != nil {
		return nil
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		if name, ok := key.(string); ok {
			s.names = append(s.names, name)
		}
		var skip json.RawMessage
		if decoder.Decode(&skip) != nil {
			return nil
		}
	}
	return nil
}

// Whether the package has the script, a value that is set
func (s scripts) has(name string) bool { return set(s.values[name]) }

// What a package.json holds of a recipe
type nodePackage struct {
	Scripts         scripts        `json:"scripts"`
	Dependencies    map[string]any `json:"dependencies"`
	DevDependencies map[string]any `json:"devDependencies"`
}

// The project's package.json, false when it is missing or no JSON object
func readPackage(root string) (*nodePackage, bool) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(text(data)), &probe) != nil {
		return nil, false
	}
	pkg := &nodePackage{}
	json.Unmarshal([]byte(text(data)), pkg)
	return pkg, true
}

// Recipe of a Node project from its scripts and dependencies, the first framework found deciding its kind and port
func detectNode(root string, pkg *nodePackage) (*Recipe, error) {
	manager, err := packageManager(root)
	if err != nil {
		return nil, err
	}
	kind, label, port := "node", "Node.js app", 0
	for _, framework := range nodeFrameworks {
		found := false
		for _, name := range framework.packages {
			_, dep := pkg.Dependencies[name]
			_, dev := pkg.DevDependencies[name]
			found = found || dep || dev
		}
		if found {
			kind, label, port = framework.kind, framework.label, framework.port
			break
		}
	}
	runners := func(names ...string) []string {
		var commands []string
		for _, name := range names {
			if pkg.Scripts.has(name) {
				commands = append(commands, runScript(manager, name))
			}
		}
		return dedupe(commands...)
	}
	r := &Recipe{Name: label, Kind: kind, ReadinessPath: "/"}
	for _, name := range []string{"dev", "start"} {
		if !pkg.Scripts.has(name) {
			continue
		}
		script, ok := pkg.Scripts.values[name].(string)
		if !ok {
			return nil, fmt.Errorf("The %s script of package.json is not text.", name)
		}
		r.Start = runners(name)[0]
		if r.Port = portOf(script); r.Port == 0 {
			r.Port = port
		}
		break
	}
	install, ok := nodeInstalls[manager]
	if !ok {
		install = "npm install"
	}
	r.Bootstrap = []string{install}
	r.Build = runners("build", "typecheck")
	r.Test = runners("test", "check", "lint")
	names := strings.Join(pkg.Scripts.names, ", ")
	if names == "" {
		names = "(none)"
	}
	managerLine := ""
	if manager != "" {
		managerLine = "Package manager: " + manager
	}
	r.Evidence = dedupe("Detected package.json", managerLine, "Scripts: "+names)
	return r, nil
}

// Text of a file under the root with its byte order mark and line ends cleaned, false when it can't be read
func readText(root, name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return "", false
	}
	return text(data), true
}

var pythonInstalls = map[string]string{"uv": "uv sync", "poetry": "poetry install", "pipenv": "pipenv install"}

// Recipe of a Python project, Django first, then FastAPI or Uvicorn, then Flask; nil without a Python marker
func detectPython(root string) (*Recipe, error) {
	pyproject, _ := readText(root, "pyproject.toml")
	requirements, _ := readText(root, "requirements.txt")
	manage, err := exists(filepath.Join(root, "manage.py"))
	if err != nil {
		return nil, err
	}
	if pyproject == "" && requirements == "" && !manage {
		if setup, err := exists(filepath.Join(root, "setup.py")); err != nil || !setup {
			return nil, err
		}
	}
	lower := strings.ToLower(pyproject + "\n" + requirements)
	manager, err := packageManager(root)
	if err != nil {
		return nil, err
	}
	install := pythonInstalls[manager]
	if install == "" {
		install = "pip install -r requirements.txt"
		if pyproject != "" && requirements == "" {
			install = "pip install -e ."
		}
	}
	tests, err := exists(filepath.Join(root, "tests"))
	if err != nil {
		return nil, err
	}
	pytest := []string{}
	if tests {
		pytest = []string{"pytest"}
	}
	r := &Recipe{Kind: "python", ReadinessPath: "/", Bootstrap: []string{install}}
	switch {
	case manage || strings.Contains(lower, "django"):
		evidence := "Detected Django dependency"
		if manage {
			evidence = "Detected manage.py"
		}
		r.Name, r.Kind, r.Test = "Django app", "django", []string{"python manage.py test"}
		r.Start, r.Port = "python manage.py runserver 0.0.0.0:8000", 8000
		r.Evidence = []string{evidence}
		if pyproject != "" {
			r.Evidence = append(r.Evidence, "Detected pyproject.toml")
		}
	case strings.Contains(lower, "fastapi") || strings.Contains(lower, "uvicorn"):
		file, err := firstThere(root, "main.py", "app.py")
		if err != nil {
			return nil, err
		}
		if file == "" {
			file = "main.py"
		}
		r.Name, r.Kind, r.Test = "FastAPI app", "fastapi", pytest
		r.Start, r.Port = "uvicorn "+strings.TrimSuffix(file, ".py")+":app --host 0.0.0.0 --port 8000", 8000
		r.Evidence = []string{"Detected Python project", "Detected FastAPI/Uvicorn dependency"}
	case strings.Contains(lower, "flask"):
		file, err := firstThere(root, "app.py", "main.py")
		if err != nil {
			return nil, err
		}
		if file == "" {
			file = "app.py"
		}
		r.Name, r.Kind, r.Test = "Flask app", "flask", pytest
		r.Start, r.Port = "flask --app "+file+" run --host 0.0.0.0 --port 5000", 5000
		r.Evidence = []string{"Detected Python project", "Detected Flask dependency"}
	default:
		r.Name, r.Test = "Python project", pytest
		if len(r.Test) == 0 {
			r.Test = []string{"python -m unittest discover"}
		}
		r.Evidence = []string{"Detected Python project"}
	}
	return r, nil
}

// Toolchains with one manifest: what builds, tests and runs them, run only when the entry file is there
var toolchains = []struct{ manifest, label, kind, build, test, start, entry string }{
	{"go.mod", "Go project", "go", "go build ./...", "go test ./...", "go run .", "main.go"},
	{"Cargo.toml", "Rust project", "rust", "cargo build", "cargo test", "cargo run", filepath.Join("src", "main.rs")},
}

// Recipe of a Go or Rust project from its manifest
func detectToolchain(root string) (*Recipe, error) {
	for _, chain := range toolchains {
		ok, err := exists(filepath.Join(root, chain.manifest))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		r := &Recipe{Name: chain.label, Kind: chain.kind, Build: []string{chain.build}, Test: []string{chain.test}, ReadinessPath: "/",
			Evidence: []string{"Detected " + chain.manifest}}
		entry, err := exists(filepath.Join(root, chain.entry))
		if err != nil {
			return nil, err
		}
		if entry {
			r.Start = chain.start
		}
		return r, nil
	}
	return nil, nil
}

// Recipe of a Maven project, else of a Gradle one preferring its wrapper
func detectJava(root string) (*Recipe, error) {
	pom, err := exists(filepath.Join(root, "pom.xml"))
	if err != nil {
		return nil, err
	}
	if pom {
		return &Recipe{Name: "Maven project", Kind: "maven", Build: []string{"mvn package"}, Test: []string{"mvn test"}, ReadinessPath: "/",
			Evidence: []string{"Detected pom.xml"}}, nil
	}
	file, err := firstThere(root, "build.gradle", "build.gradle.kts")
	if err != nil || file == "" {
		return nil, err
	}
	gradle := "gradle"
	if wrapper, err := exists(filepath.Join(root, "gradlew")); err != nil {
		return nil, err
	} else if wrapper {
		gradle = "./gradlew"
	}
	return &Recipe{Name: "Gradle project", Kind: "gradle", Build: []string{gradle + " build"}, Test: []string{gradle + " test"}, ReadinessPath: "/",
		Evidence: []string{"Detected Gradle build file"}}, nil
}

// Make targets each phase takes, the first the Makefile defines
var makePhases = []struct {
	phase   string
	targets []string
}{
	{"bootstrap", []string{"install", "setup", "bootstrap"}},
	{"build", []string{"build", "compile"}},
	{"test", []string{"test", "check"}},
	{"start", []string{"run", "start", "serve", "dev"}},
}

// Recipe of a project its Makefile drives
func detectMake(root string) (*Recipe, error) {
	makefile, ok := readText(root, "Makefile")
	if !ok {
		return nil, nil
	}
	var targets []string
	defined := map[string]bool{}
	for _, line := range strings.Split(makefile, "\n") {
		if m := makeTarget.FindStringSubmatch(line); m != nil {
			targets = append(targets, m[1])
			defined[m[1]] = true
		}
	}
	picked := map[string][]string{}
	for _, phase := range makePhases {
		picked[phase.phase] = []string{}
		for _, target := range phase.targets {
			if defined[target] {
				picked[phase.phase] = []string{"make " + target}
				break
			}
		}
	}
	names := strings.Join(targets, ", ")
	if names == "" {
		names = "(none)"
	}
	r := &Recipe{Name: "Makefile-driven project", Kind: "make", ReadinessPath: "/", Bootstrap: picked["bootstrap"], Build: picked["build"],
		Test: picked["test"], Evidence: []string{"Detected Makefile", "Targets: " + names}}
	if start := picked["start"]; len(start) > 0 {
		r.Start = start[0]
	}
	return r, nil
}

var composeFiles = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

// Recipe of a docker compose project
func detectCompose(root string) (*Recipe, error) {
	file, err := firstThere(root, composeFiles...)
	if err != nil || file == "" {
		return nil, err
	}
	return &Recipe{Name: "docker-compose project", Kind: "compose", Build: []string{"docker compose build"}, Start: "docker compose up",
		ReadinessPath: "/", Evidence: []string{"Detected " + file}}, nil
}

// Recipe the project's files make: package.json first, then Python, Go and Rust, Java, a Makefile and docker compose; nil when none matches
func Detect(root string) (*Recipe, error) {
	if pkg, ok := readPackage(root); ok {
		return detectNode(root, pkg)
	}
	for _, detect := range []func(string) (*Recipe, error){detectPython, detectToolchain, detectJava, detectMake, detectCompose} {
		if r, err := detect(root); err != nil || r != nil {
			return r, err
		}
	}
	return nil, nil
}
