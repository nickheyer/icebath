package config

const (
	Key = "vault"
	// Where the key of the vault is kept, file or keychain, which moving the key changes; where the first key is made before there is one
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
