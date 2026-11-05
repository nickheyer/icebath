package pm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
)

// StartupImports are the distribution, module and attribute triples a dependency environment must import
var StartupImports = [][3]string{
	{"ruamel.yaml", "ruamel.yaml", "YAML"},
	{"python-dotenv", "dotenv", "load_dotenv"},
	{"click", "click", "Command"},
	{"certifi", "certifi", "contents"},
	{"rich", "rich", "print"},
	{"cryptography", "cryptography.hazmat.bindings._rust", "openssl"},
	{"PyJWT", "jwt", "encode"},
}

// Python tuple literal of the startup import checks
func startupImportsLiteral() string {
	rows := make([]string, len(StartupImports))
	for i, row := range StartupImports {
		rows[i] = fmt.Sprintf("(%s, %s, %s)", pyRepr(row[0]), pyRepr(row[1]), pyRepr(row[2]))
	}
	return "(" + strings.Join(rows, ", ") + ")"
}

// Script the candidate interpreter runs to prove its startup imports and CA bundle
func validationScript() string {
	return "import importlib, importlib.metadata, pathlib, re, tomllib\n" +
		"project = tomllib.loads(pathlib.Path('pyproject.toml').read_text(encoding='utf-8-sig'))['project']\n" +
		"required = {re.split(r'[\\[<>=!~; @]', dep, 1)[0].lower().replace('_', '-')\n" +
		"            for dep in project.get('dependencies', [])}\n" +
		"checks = " + startupImportsLiteral() + "\n" +
		"for distribution, module, attribute in checks:\n" +
		"    try:\n" +
		"        importlib.metadata.distribution(distribution)\n" +
		"    except importlib.metadata.PackageNotFoundError:\n" +
		"        if distribution.lower().replace('_', '-') in required:\n" +
		"            raise\n" +
		"        continue\n" +
		"    loaded = importlib.import_module(module)\n" +
		"    getattr(loaded, attribute)\n" +
		"    if module == 'certifi':\n" +
		"        bundle = pathlib.Path(loaded.where())\n" +
		"        assert bundle.is_file() and bundle.stat().st_size >= 1024, 'CA bundle is missing'\n"
}

// Runs the startup import checks inside a candidate, through its argv prefix or else its isolated interpreter
func ValidateEnvironment(ctx context.Context, python string, argvPrefix []string, env map[string]string, cwd string) error {
	argv := argvPrefix
	if argv == nil {
		argv = []string{python, "-I"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], append(append([]string{}, argv[1:]...), "-c", validationScript())...)
	cmd.Dir = cwd
	cmd.Env = EnvList(env)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("Command '%s' timed out after 60 seconds", pyListRepr(cmd.Args))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		tail := lastRunes(strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "�")), 1000)
		return NewInstallError("venv", "startup validation failed: "+tail, "")
	}
	return err
}

// Restores this installation's recorded dependency set and collects what the repair superseded
func RepairDependencies(ctx context.Context, projectRoot string) error {
	if resolvePath(projectRoot) != resolvePath(RepoRoot()) {
		return NewInstallError("venv", "recovery root does not match this PM installation", "")
	}
	if err := SyncVenv(ctx, SyncVenvOptions{Repair: true, Stdout: os.Stderr}); err != nil {
		return err
	}
	return CollectSupersededGenerations(ctx, projectRoot)
}

// Extras the store's shipped selection carries that the recorded selection lacks
func baselineGaps(ctx context.Context, root string) ([]string, error) {
	facts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, root), true)
	if err != nil {
		return nil, err
	}
	fact, ok := facts.Get("venv")
	if !ok {
		if _, present := facts.packages["venv"]; !present || facts.packages["venv"] == nil {
			return nil, nil
		}
		return nil, &PyError{Class: "ValueError", Message: "invalid recorded dependency state"}
	}
	recordedRaw, isList := fact["extras"].([]any)
	stamp, isStamp := fact["stamp"].(string)
	if !isList || !isStamp || stamp == "" {
		return nil, &PyError{Class: "ValueError", Message: "invalid recorded dependency state"}
	}
	var recorded []string
	for _, extra := range recordedRaw {
		s, ok := extra.(string)
		if !ok {
			return nil, &PyError{Class: "ValueError", Message: "invalid recorded dependency state"}
		}
		recorded = append(recorded, s)
	}
	storeFactsPath, err := FactsPath(ctx)
	if err != nil {
		return nil, err
	}
	storeFacts, err := NewFacts(storeFactsPath, false)
	if err != nil {
		return nil, err
	}
	var shipped []string
	if venv, ok := storeFacts.Get("venv"); ok {
		if list, ok := venv["extras"].([]any); ok {
			for _, e := range list {
				shipped = append(shipped, pyStr(e))
			}
		}
	}
	have := map[string]bool{}
	for _, extra := range recorded {
		have[ExtraKey(extra)] = true
	}
	carried, err := CarriedExtras(ctx, NewVenv(root), shipped)
	if err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	for _, extra := range carried {
		if !have[ExtraKey(extra)] {
			missing[extra] = true
		}
	}
	out := make([]string, 0, len(missing))
	for extra := range missing {
		out = append(out, extra)
	}
	sort.Strings(out)
	return out, nil
}

// Engine currency probe a dependency refresh consults
var engineVenvIsCurrent = venvIsCurrent

// Re-resolves the durable selection after an external update replaced its inputs and reports what happened
func RefreshDependencies(ctx context.Context, projectRoot string) (string, error) {
	root := resolvePath(projectRoot)
	if root != resolvePath(RepoRoot()) {
		return "", NewInstallError("venv", "refresh root does not match this PM installation", "")
	}
	if !isRegularFile(EnvironmentRuntimeFactsPath(ctx, root)) {
		return "base", nil
	}
	missing, err := baselineGaps(ctx, root)
	if err != nil {
		return "", err
	}
	if len(missing) == 0 {
		current, err := engineVenvIsCurrent(ctx, nil, nil, root)
		if err != nil {
			return "", err
		}
		if current {
			return "current", nil
		}
	}
	opts := SyncVenvOptions{Explicit: true, Automatic: missing, Stdout: os.Stderr}
	if len(missing) > 0 {
		opts.Extras = missing
	}
	if err := clientSyncVenv(ctx, opts); err == nil {
		if len(missing) > 0 {
			return "restored " + strings.Join(missing, ", "), nil
		}
		return "rebuilt", nil
	} else {
		fmt.Fprintf(os.Stderr, "dependency refresh failed: %s\n", err)
	}
	_, release, err := RuntimeLock(ctx, root, WaitForever)
	if err != nil {
		return "", err
	}
	defer release()
	facts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, root), true)
	if err != nil {
		return "", err
	}
	fact, _ := facts.Get("venv")
	var recorded []string
	if list, ok := fact["extras"].([]any); ok {
		for _, e := range list {
			recorded = append(recorded, pyStr(e))
		}
	}
	stamp, _ := fact["stamp"].(string)
	if stamp == "" {
		stamp = "stale"
	}
	extras := append([]string{}, recorded...)
	for _, extra := range missing {
		if !slices.Contains(recorded, extra) {
			extras = append(extras, extra)
		}
	}
	if err := facts.RecordState("venv", stamp, extras, "", ""); err != nil {
		return "", err
	}
	return "fallback", nil
}
