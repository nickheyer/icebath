package config

const (
	Key = "secrets"
	// Order the secret stores are asked in before the rest in the order they are registered
	Sources = Key + ".sources"
	// Variables no secret store may overwrite once they are set
	Preserve = Key + ".preserve_existing"
	// A secret named for the profile, OPENAI_API_KEY_WORK in profile work, is also set under its plain name
	ProfileAlias = Key + ".profile_alias"
)

// Names of the settings every secret store has under its own section, secrets.<name>
const (
	Enabled  = "enabled"
	Override = "override_existing"
	Timeout  = "timeout_seconds"
	// Variables a store that fills named ones fills, each with the reference in the store its value comes from
	Env = "env"
)

// Section of the secret store with the name
func Section(name string) string {
	return Key + "." + name
}

// Setting of the secret store with the name
func Of(source, setting string) string {
	return Section(source) + "." + setting
}
