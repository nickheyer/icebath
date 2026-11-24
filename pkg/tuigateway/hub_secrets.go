package tuigateway

// How the Bitwarden secret store is set up
type SecretsBitwarden struct {
	Enabled bool `json:"enabled,omitzero"`
	// Variable holding the access token, and whether it holds one
	TokenEnv  string `json:"token_env"`
	TokenSet  bool   `json:"token_set,omitzero"`
	ProjectID string `json:"project_id,omitzero"`
	// Address of another region or of a server of the user's own, empty for the default cloud
	ServerURL string `json:"server_url,omitzero"`
	// Helper program and its version, empty when it is not installed
	Binary  string `json:"binary,omitzero"`
	Version string `json:"version,omitzero"`
}

// One environment variable and the 1Password reference it resolves from
type SecretReference struct {
	Env       string `json:"env"`
	Reference string `json:"reference"`
}

// How the 1Password secret store is set up
type SecretsOnePassword struct {
	Enabled bool `json:"enabled,omitzero"`
	// Variable holding a service account token, and whether it holds one
	TokenEnv string `json:"token_env"`
	TokenSet bool   `json:"token_set,omitzero"`
	Account  string `json:"account,omitzero"`
	// The op program and its version, empty when it is not installed
	Binary     string            `json:"binary,omitzero"`
	Version    string            `json:"version,omitzero"`
	References []SecretReference `json:"references"`
}

type SecretsStateParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Both secret stores of the profile
type SecretsStateResult struct {
	Bitwarden   SecretsBitwarden   `json:"bitwarden"`
	OnePassword SecretsOnePassword `json:"onepassword"`
}

// Access token and server whose Bitwarden projects to list, the stored token when none is given
type SecretsProjectsParams struct {
	Profile   *string `json:"profile,omitzero"`
	Token     string  `json:"token,omitzero"`
	ServerURL string  `json:"server_url,omitzero"`
}

// One Bitwarden project an access token sees
type SecretsProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type SecretsProjectsResult struct {
	Projects []SecretsProject `json:"projects"`
}

// One change to a secret store, bitwarden or onepassword: setup, token, off, check, and for 1Password map and unmap
type SecretsChangeParams struct {
	Profile *string `json:"profile,omitzero"`
	Source  string  `json:"source"`
	Verb    string  `json:"verb"`
	// New token of a setup or a token change, the stored one kept when empty on a setup
	Token string `json:"token,omitzero"`
	// Server and project of a Bitwarden setup
	ServerURL string `json:"server_url,omitzero"`
	ProjectID string `json:"project_id,omitzero"`
	// Account, token variable and path of the op program of a 1Password setup, each kept as it is when empty
	Account    string `json:"account,omitzero"`
	TokenEnv   string `json:"token_env,omitzero"`
	BinaryPath string `json:"binary_path,omitzero"`
	// Environment variable a map binds to the reference, or an unmap frees
	Env       string `json:"env,omitzero"`
	Reference string `json:"reference,omitzero"`
}

// What the next start would do with one name a store hands over: export, override, skip_set, skip_token or unresolved
type SecretsPreviewRow struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// The stores after a change, with what a setup or a check found
type SecretsChangeResult struct {
	State SecretsStateResult `json:"state"`
	// What the next start would do with each secret, from a setup or a check
	Rows     []SecretsPreviewRow `json:"rows,omitzero"`
	Warnings []string            `json:"warnings,omitzero"`
	// Who 1Password signs in as after a setup or a token change, empty when it is not signed in
	Identity string `json:"identity,omitzero"`
	// How many projects a new Bitwarden token sees
	Projects int64 `json:"projects,omitzero"`
}

// Both secret stores of the profile for the secret stores screen
var SecretsState = NewMethod[SecretsStateParams, SecretsStateResult]("secrets.state")

// Bitwarden projects an access token sees
var SecretsProjects = NewMethod[SecretsProjectsParams, SecretsProjectsResult]("secrets.projects")

// One change to a secret store, answered with the stores after it
var SecretsChange = NewMethod[SecretsChangeParams, SecretsChangeResult]("secrets.change")
