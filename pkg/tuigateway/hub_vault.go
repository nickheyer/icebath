package tuigateway

// One item the agent may fill in for the user, never its secret
type VaultItemRow struct {
	Handle string `json:"handle"`
	// Where the item is kept: the local vault or a password manager
	Source string `json:"source"`
	// login, payment or address
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Identifier string `json:"identifier,omitzero"`
	// Site the item may be filled in on
	Origin string `json:"origin,omitzero"`
	// Kept in the vault of this profile, so it can be removed here
	Local bool `json:"local,omitzero"`
}

// One password manager and whether the agent uses it: on, off, or missing when it is not installed
type VaultSourceRow struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
}

// Where the key of the local vault lives
type VaultKeyInfo struct {
	// file, keychain, or none while no item was saved yet
	Storage string `json:"storage"`
	Path    string `json:"path,omitzero"`
	Service string `json:"service,omitzero"`
	Account string `json:"account,omitzero"`
	// Where the key of a new vault goes, file or keychain
	Configured string `json:"configured"`
	// Keychain of this computer
	Keychain string `json:"keychain,omitzero"`
	// Keychain copy that a move back to the file could not remove yet
	KeychainLeft string `json:"keychain_left,omitzero"`
}

// One field of an item of a kind besides its label and site
type VaultField struct {
	Key      string `json:"key"`
	Required bool   `json:"required,omitzero"`
	Secret   bool   `json:"secret,omitzero"`
	// Only values the field takes, empty for free text
	Choices []string `json:"choices,omitzero"`
}

// One kind of item with its fields in the order they are asked
type VaultKind struct {
	Kind   string       `json:"kind"`
	Fields []VaultField `json:"fields"`
}

type VaultListParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Everything the autofill vault screen shows
type VaultListResult struct {
	Items []VaultItemRow `json:"items"`
	// Password managers that are on but locked, so their items are not listed
	Locked  []string         `json:"locked"`
	Sources []VaultSourceRow `json:"sources"`
	Key     VaultKeyInfo     `json:"key"`
	Kinds   []VaultKind      `json:"kinds"`
}

// One change to the autofill vault: add, remove, source or key
type VaultChangeParams struct {
	Profile *string `json:"profile,omitzero"`
	Verb    string  `json:"verb"`
	// Kind, label, site and values by field of an item an add stores
	Kind   string            `json:"kind,omitzero"`
	Label  string            `json:"label,omitzero"`
	Origin string            `json:"origin,omitzero"`
	Values map[string]string `json:"values,omitzero"`
	// Handle of the item a remove takes out
	Handle string `json:"handle,omitzero"`
	// Password manager a source change turns on or off
	Source string `json:"source,omitzero"`
	On     bool   `json:"on,omitzero"`
	// Where a key change moves the key, keychain or file
	To string `json:"to,omitzero"`
}

// Items, password managers and key of the autofill vault for its screen
var VaultList = NewMethod[VaultListParams, VaultListResult]("vault.list")

// One change to the autofill vault, answered with the vault after it
var VaultChange = NewMethod[VaultChangeParams, VaultListResult]("vault.change")
