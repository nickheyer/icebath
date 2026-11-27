package config

const (
	Key = "vault"
	// Where the key of a new vault is kept: file or keychain
	KeyStorage = Key + ".key_storage"
)

// Settings every password manager has under its own section, vault.<name>
const (
	Enabled = "enabled"
)

// Setting of the password manager with the name
func Of(source, setting string) string {
	return Key + "." + source + "." + setting
}
