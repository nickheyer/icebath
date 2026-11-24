package provider

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	consoleutil "github.com/nickheyer/icebath/pkg/utils/console"
)

// SecretSourceRefusal is a secret store change refused for what was asked or for what the store answered
type SecretSourceRefusal struct {
	Message string
	// What a refused Bitwarden token usually means: region for a token of another Bitwarden region, token for a wrong or revoked one, empty when not known
	Cause string
}

func (e *SecretSourceRefusal) Error() string { return e.Message }

// BitwardenState is how the Bitwarden secret store is set up for a home
type BitwardenState struct {
	Enabled bool
	// Variable holding the access token, and whether it holds one
	TokenEnv  string
	TokenSet  bool
	ProjectID string
	// Address of another region or of a server of the user's own, empty for the default cloud
	ServerURL string
	// Helper program and its version, empty when it is not installed
	Binary, Version string
}

// OnePasswordState is how the 1Password secret store is set up for a home
type OnePasswordState struct {
	Enabled bool
	// Variable holding a service account token, and whether it holds one
	TokenEnv string
	TokenSet bool
	Account  string
	// The op program and its version, empty when it is not installed
	Binary, Version string
	// Environment variable names with the reference each resolves from, by name
	References [][2]string
}

// BitwardenProject is one project an access token sees
type BitwardenProject struct{ ID, Name string }

// SecretPreviewRow is what the next start would do with one name: export, override, skip_set, skip_token or unresolved
type SecretPreviewRow struct{ Name, Action string }

// SecretPreview is what a secret store would hand over at the next start
type SecretPreview struct {
	Rows     []SecretPreviewRow
	Warnings []string
}

// Value of a variable of the home: the environment of this process first, then the env file of the home
func secretEnvValue(h, name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	if v, ok := config.NewSecrets(h).Lookup()(name); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// What a start would do with a name a store hands over: skip the token variable itself, skip a variable that is set unless overriding, else export it
func secretSyncAction(name, tokenEnv string, override bool) string {
	already := os.Getenv(name) != ""
	switch {
	case name == tokenEnv:
		return "skip_token"
	case already && !override:
		return "skip_set"
	case already:
		return "override"
	}
	return "export"
}

// Turns the Bitwarden section on for a project, the settings it does not hold yet taking their defaults
func enableBitwarden(section map[string]any, tokenEnv, projectID, serverURL string) {
	section["enabled"], section["project_id"], section["server_url"] = true, projectID, serverURL
	for _, entry := range []struct {
		key   string
		value any
	}{{"access_token_env", tokenEnv}, {"cache_ttl_seconds", 300}, {"override_existing", true}, {"auto_install", true}} {
		if _, ok := section[entry.key]; !ok {
			section[entry.key] = entry.value
		}
	}
}

// Turns the 1Password section on, the settings it does not hold yet taking their defaults
func enableOnePassword(section map[string]any) {
	section["enabled"] = true
	for _, entry := range []struct {
		key   string
		value any
	}{{"env", map[string]any{}}, {"cache_ttl_seconds", 300}, {"override_existing", true}} {
		if _, ok := section[entry.key]; !ok {
			section[entry.key] = entry.value
		}
	}
}

// Turns a secret store off in the config of the home
func setSecretSourceDisabled(h, section string) error {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return err
	}
	secretsSetdefaultMapping(secretsSetdefaultMapping(cfg, "secrets"), section)["enabled"] = false
	return secretsSaveConfig(h, cfg)
}

// The Bitwarden helper program, installed when it is missing
func bitwardenBinary() (string, error) {
	backend := loadBw()
	if binary := backend.find(false); binary != "" {
		return binary, nil
	}
	binary, err := backend.install()
	if err != nil {
		return "", &SecretSourceRefusal{Message: "could not install the Bitwarden helper: " + err.Error()}
	}
	return binary, nil
}

// Secrets CLI context of a home that prints nowhere, for the probes that take one
func quietSecretsCLI(ctx context.Context, h string) *secretsCLI {
	return &secretsCLI{ctx: ctx, home: h, console: consoleutil.NewConsole(io.Discard)}
}

// SecretSourcesState is how both secret stores are set up for the home
func SecretSourcesState(h string) (BitwardenState, OnePasswordState, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return BitwardenState{}, OnePasswordState{}, err
	}
	bw := bwCfg(cfg)
	bitwarden := BitwardenState{Enabled: pyTruthy(bw["enabled"]), TokenEnv: cfgText(bw, "access_token_env", bwDefaultTokenEnv),
		ProjectID: cfgStr(bw, "project_id"), ServerURL: cfgStr(bw, "server_url")}
	bitwarden.TokenSet = secretEnvValue(h, bitwarden.TokenEnv) != ""
	if bitwarden.Binary = loadBw().find(false); bitwarden.Binary != "" {
		bitwarden.Version = bwsVersion(bitwarden.Binary)
	}
	op := sectionCfg(cfg, "onepassword")
	onePassword := OnePasswordState{Enabled: pyTruthy(op["enabled"]), TokenEnv: cfgText(op, "service_account_token_env", opDefaultTokenEnv), Account: cfgStr(op, "account")}
	onePassword.TokenSet = secretEnvValue(h, onePassword.TokenEnv) != ""
	if onePassword.Binary = findOp(cfgStr(op, "binary_path")); onePassword.Binary != "" {
		onePassword.Version = opVersion(onePassword.Binary)
	}
	refs := references(op)
	for _, name := range sortedAnyKeys(refs) {
		onePassword.References = append(onePassword.References, [2]string{name, pyStr(refs[name])})
	}
	return bitwarden, onePassword, nil
}

// BitwardenProjectsFor is the projects an access token sees on a server, the stored token when none is given
func BitwardenProjectsFor(ctx context.Context, h, token, serverURL string) ([]BitwardenProject, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return nil, err
	}
	if token = strings.TrimSpace(token); token == "" {
		token = secretEnvValue(h, cfgText(bwCfg(cfg), "access_token_env", bwDefaultTokenEnv))
	}
	if token == "" {
		return nil, &SecretSourceRefusal{Message: "Bitwarden needs an access token"}
	}
	binary, err := bitwardenBinary()
	if err != nil {
		return nil, err
	}
	projects, failure := bitwardenProjects(ctx, binary, token, strings.TrimSpace(serverURL))
	if failure != nil {
		return nil, &SecretSourceRefusal{Message: failure.Text, Cause: failure.Cause}
	}
	out := make([]BitwardenProject, 0, len(projects))
	for _, p := range projects {
		out = append(out, BitwardenProject{ID: pyStr(p["id"]), Name: projectField(p, "name")})
	}
	return out, nil
}

// SetUpBitwarden turns the Bitwarden store on for a project after one pull of its secrets worked, keeping a new token only then, and answers what the next start would do with each secret
func SetUpBitwarden(h, token, serverURL, projectID string) (*SecretPreview, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return nil, err
	}
	section := secretsSetdefaultMapping(secretsSetdefaultMapping(cfg, "secrets"), "bitwarden")
	tokenEnv := cfgText(section, "access_token_env", bwDefaultTokenEnv)
	stored := secretEnvValue(h, tokenEnv)
	token, serverURL, projectID = strings.TrimSpace(token), strings.TrimSpace(serverURL), strings.TrimSpace(projectID)
	if token == "" {
		token = stored
	}
	switch {
	case token == "":
		return nil, &SecretSourceRefusal{Message: "Bitwarden needs an access token"}
	case projectID == "":
		return nil, &SecretSourceRefusal{Message: "Bitwarden needs a project"}
	}
	binary, err := bitwardenBinary()
	if err != nil {
		return nil, err
	}
	secrets, warnings, err := loadBw().fetch(BitwardenFetchOptions{AccessToken: token, ProjectID: projectID, Binary: binary,
		CacheTTL: 300, UseCache: false, ServerURL: serverURL, HomePath: h})
	if err != nil {
		return nil, &SecretSourceRefusal{Message: err.Error()}
	}
	if token != stored {
		if err := secretsSaveEnvValue(h, tokenEnv, token); err != nil {
			return nil, err
		}
		os.Setenv(tokenEnv, token)
	}
	enableBitwarden(section, tokenEnv, projectID, serverURL)
	if err := secretsSaveConfig(h, cfg); err != nil {
		return nil, err
	}
	preview := &SecretPreview{Warnings: warnings}
	for _, name := range sortedStringKeys(secrets) {
		preview.Rows = append(preview.Rows, SecretPreviewRow{Name: name, Action: secretSyncAction(name, tokenEnv, cfgBool(section, "override_existing", false))})
	}
	return preview, nil
}

// RotateBitwardenToken keeps a new access token once Bitwarden accepts it and answers how many projects it sees
func RotateBitwardenToken(ctx context.Context, h, token string) (int, error) {
	if token = strings.TrimSpace(token); token == "" {
		return 0, &SecretSourceRefusal{Message: "Bitwarden needs an access token"}
	}
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return 0, err
	}
	bw := bwCfg(cfg)
	binary, err := bitwardenBinary()
	if err != nil {
		return 0, err
	}
	projects, failure := bitwardenProjects(ctx, binary, token, cfgStr(bw, "server_url"))
	if failure != nil {
		return 0, &SecretSourceRefusal{Message: failure.Text, Cause: failure.Cause}
	}
	tokenEnv := cfgText(bw, "access_token_env", bwDefaultTokenEnv)
	if err := secretsSaveEnvValue(h, tokenEnv, token); err != nil {
		return 0, err
	}
	os.Setenv(tokenEnv, token)
	loadBw().clearCaches(h)
	return len(projects), nil
}

// PreviewBitwarden pulls the secrets of the project now and answers what the next start would do with each
func PreviewBitwarden(h string) (*SecretPreview, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return nil, err
	}
	bw := bwCfg(cfg)
	tokenEnv := cfgText(bw, "access_token_env", bwDefaultTokenEnv)
	token, projectID := secretEnvValue(h, tokenEnv), cfgStr(bw, "project_id")
	switch {
	case !pyTruthy(bw["enabled"]):
		return nil, &SecretSourceRefusal{Message: "Bitwarden is turned off"}
	case token == "":
		return nil, &SecretSourceRefusal{Message: "Bitwarden has no access token"}
	case projectID == "":
		return nil, &SecretSourceRefusal{Message: "Bitwarden has no project"}
	}
	secrets, warnings, err := loadBw().fetch(BitwardenFetchOptions{AccessToken: token, ProjectID: projectID,
		CacheTTL: 300, UseCache: false, ServerURL: cfgStr(bw, "server_url"), HomePath: h})
	if err != nil {
		return nil, &SecretSourceRefusal{Message: err.Error()}
	}
	preview := &SecretPreview{Warnings: warnings}
	for _, name := range sortedStringKeys(secrets) {
		preview.Rows = append(preview.Rows, SecretPreviewRow{Name: name, Action: secretSyncAction(name, tokenEnv, cfgBool(bw, "override_existing", false))})
	}
	return preview, nil
}

// SetUpOnePassword turns the 1Password store on: the op program is found, the account, the token variable and a new token are kept when given, and the identity op signs in as is answered, empty when it is not signed in
func SetUpOnePassword(ctx context.Context, h, account, tokenEnv, token, binaryPath string) (string, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return "", err
	}
	op := opCfgForWrite(cfg)
	if binaryPath = strings.TrimSpace(binaryPath); binaryPath == "" {
		binaryPath = cfgStr(op, "binary_path")
	}
	binary := findOp(binaryPath)
	if binary == "" {
		return "", &SecretSourceRefusal{Message: "the 1Password op program was not found; install it from " + opDocsURL}
	}
	if binaryPath != "" {
		op["binary_path"] = binaryPath
	}
	if account = strings.TrimSpace(account); account != "" {
		op["account"] = account
	}
	if tokenEnv = strings.TrimSpace(tokenEnv); tokenEnv == "" {
		tokenEnv = cfgText(op, "service_account_token_env", opDefaultTokenEnv)
	}
	op["service_account_token_env"] = tokenEnv
	if token = strings.TrimSpace(token); token != "" {
		if err := secretsSaveEnvValue(h, tokenEnv, token); err != nil {
			return "", err
		}
		os.Setenv(tokenEnv, token)
	}
	enableOnePassword(op)
	if err := secretsSaveConfig(h, cfg); err != nil {
		return "", err
	}
	identity, _ := opWhoami(quietSecretsCLI(ctx, h), binary, cfgStr(op, "account"), secretEnvValue(h, tokenEnv))
	return identity, nil
}

// RotateOnePasswordToken keeps a new service account token once op accepts it and answers the identity it signs in as
func RotateOnePasswordToken(ctx context.Context, h, token string) (string, error) {
	if token = strings.TrimSpace(token); token == "" {
		return "", &SecretSourceRefusal{Message: "1Password needs a service account token"}
	}
	op, err := opCfg(h)
	if err != nil {
		return "", err
	}
	binary := findOp(cfgStr(op, "binary_path"))
	if binary == "" {
		return "", &SecretSourceRefusal{Message: "the 1Password op program was not found; install it from " + opDocsURL}
	}
	identity, accepted := opWhoami(quietSecretsCLI(ctx, h), binary, cfgStr(op, "account"), token)
	if !accepted {
		return "", &SecretSourceRefusal{Message: "1Password did not accept the token; nothing was changed"}
	}
	tokenEnv := cfgText(op, "service_account_token_env", opDefaultTokenEnv)
	if err := secretsSaveEnvValue(h, tokenEnv, token); err != nil {
		return "", err
	}
	os.Setenv(tokenEnv, token)
	clearOnePasswordCaches(h)
	return identity, nil
}

// Binds an environment variable to a 1Password reference in the config of the home and answers the reference as kept, the warnings of a pair that is not valid, and whether the store is on
func mapOnePasswordReference(h, envVar string, reference any) (string, []string, bool, error) {
	valid, warnings := validateOpReferences(map[string]any{envVar: reference})
	value, ok := valid[envVar]
	if !ok {
		return "", warnings, false, nil
	}
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return "", nil, false, err
	}
	op := opCfgForWrite(cfg)
	env, isMap := op["env"].(map[string]any)
	if !isMap {
		env = map[string]any{}
		op["env"] = env
	}
	env[envVar] = value
	if err := secretsSaveConfig(h, cfg); err != nil {
		return "", nil, false, err
	}
	return value, nil, pyTruthy(op["enabled"]), nil
}

// MapOnePasswordReference binds an environment variable to the 1Password reference it resolves from
func MapOnePasswordReference(h, envVar, reference string) error {
	_, warnings, _, err := mapOnePasswordReference(h, strings.TrimSpace(envVar), reference)
	if err == nil && len(warnings) > 0 {
		return &SecretSourceRefusal{Message: strings.Join(warnings, "; ")}
	}
	return err
}

// Takes the reference of an environment variable out of the config of the home and reports whether it had one
func unmapOnePasswordReference(h, envVar string) (bool, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return false, err
	}
	envMap, isMap := opCfgForWrite(cfg)["env"].(map[string]any)
	if _, mapped := envMap[envVar]; !isMap || !mapped {
		return false, nil
	}
	delete(envMap, envVar)
	return true, secretsSaveConfig(h, cfg)
}

// UnmapOnePasswordReference takes the 1Password reference of an environment variable away
func UnmapOnePasswordReference(h, envVar string) error {
	mapped, err := unmapOnePasswordReference(h, envVar)
	if err == nil && !mapped {
		return &SecretSourceRefusal{Message: envVar + " has no reference"}
	}
	return err
}

// PreviewOnePassword resolves every reference now and answers what the next start would do with each
func PreviewOnePassword(h string) (*SecretPreview, error) {
	op, err := opCfg(h)
	if err != nil {
		return nil, err
	}
	if !pyTruthy(op["enabled"]) {
		return nil, &SecretSourceRefusal{Message: "1Password is turned off"}
	}
	refs := references(op)
	if len(refs) == 0 {
		return &SecretPreview{}, nil
	}
	tokenEnv := cfgText(op, "service_account_token_env", opDefaultTokenEnv)
	secrets, warnings, err := FetchOnePasswordSecrets(OnePasswordFetchOptions{References: refs, Account: cfgStr(op, "account"),
		TokenEnv: tokenEnv, BinaryPath: cfgStr(op, "binary_path"), UseCache: false, CacheTTL: 300, HomePath: h})
	if err != nil {
		return nil, &SecretSourceRefusal{Message: err.Error()}
	}
	preview := &SecretPreview{Warnings: warnings}
	for _, name := range sortedAnyKeys(refs) {
		action := secretSyncAction(name, tokenEnv, cfgBool(op, "override_existing", true))
		if _, resolved := secrets[name]; !resolved && name != tokenEnv {
			action = "unresolved"
		}
		preview.Rows = append(preview.Rows, SecretPreviewRow{Name: name, Action: action})
	}
	return preview, nil
}

// DisableSecretSource turns the Bitwarden or the 1Password store off, its token and references staying where they are
func DisableSecretSource(h, source string) error {
	if source != "bitwarden" && source != "onepassword" {
		return &SecretSourceRefusal{Message: "unknown secret store: " + source}
	}
	return setSecretSourceDisabled(h, source)
}
