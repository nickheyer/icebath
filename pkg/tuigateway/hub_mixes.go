package tuigateway

// One model of a mix
type MixSlot struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitzero"`
	// An advising model kept in the mix without being asked
	Off bool `json:"off,omitzero"`
}

// One saved mix: the models that advise and the one that acts on their advice
type MixRow struct {
	Name string `json:"name"`
	// The mix a run uses when it names none
	Default bool `json:"default,omitzero"`
	// The mix the settings have switched on
	Active   bool      `json:"active,omitzero"`
	Advisers []MixSlot `json:"advisers"`
	Acting   MixSlot   `json:"acting"`
	// Billing note when the acting model runs on another provider than the main model
	Notice string `json:"notice,omitzero"`
}

type MixesListParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Every saved mix of the profile
type MixesListResult struct {
	Mixes []MixRow `json:"mixes"`
}

// One change to the saved mixes: save, default or remove
type MixesChangeParams struct {
	Profile *string `json:"profile,omitzero"`
	Verb    string  `json:"verb"`
	Name    string  `json:"name"`
	// Advising models and acting model a save stores, a new name making a new mix
	Advisers []MixSlot `json:"advisers,omitzero"`
	Acting   *MixSlot  `json:"acting,omitzero"`
}

// Saved mixes of the profile for the model mixes screen
var MixesList = NewMethod[MixesListParams, MixesListResult]("mixes.list")

// One change to the saved mixes, answered with the mixes after it
var MixesChange = NewMethod[MixesChangeParams, MixesListResult]("mixes.change")
