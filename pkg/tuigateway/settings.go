package tuigateway

// One group of the settings screen in the order it is shown
type SettingsGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Short string `json:"short"`
	Order int64  `json:"order"`
	// Shown only once the advanced settings are opened
	AdvancedOnly bool `json:"advanced_only,omitzero"`
}

// One allowed value of a choice setting with what it means
type SettingsChoice struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Meaning string `json:"meaning,omitzero"`
}

// One setting as the settings screen shows it, its description, kind, value in effect and where that value comes from
type SettingsRow struct {
	Key     string           `json:"key"`
	Label   string           `json:"label"`
	Short   string           `json:"short"`
	Help    string           `json:"help,omitzero"`
	Group   string           `json:"group"`
	Order   int64            `json:"order"`
	Kind    string           `json:"kind"`
	Default any              `json:"default"`
	Null    string           `json:"null,omitzero"`
	Choices []SettingsChoice `json:"choices,omitzero"`
	// Whether values outside the choices are taken too
	Open bool     `json:"open,omitzero"`
	Min  *float64 `json:"min,omitzero"`
	Max  *float64 `json:"max,omitzero"`
	Unit string   `json:"unit,omitzero"`
	// Where the setting may be written: global, workspace or session
	Scopes []string `json:"scopes"`
	// When a change takes effect: live, next_turn, new_session or restart
	Apply    string `json:"apply"`
	Advanced bool   `json:"advanced,omitzero"`
	// Slash shortcut that changes the setting, without the slash
	Slash string `json:"slash,omitzero"`
	// Environment variable that overrides the setting
	Env string `json:"env,omitzero"`
	// Old names the setting was migrated from
	Was   []string `json:"was,omitzero"`
	Value any      `json:"value"`
	// Layer the value comes from: default, global, session, env or managed
	Source string `json:"source"`
	// Whether a managed install locks the setting
	Locked bool `json:"locked,omitzero"`
	// Value the session layer holds when the rows were read for a session
	Session any `json:"session,omitzero"`
}

// Groups of the settings screen
type SettingsGroupsParams struct {
	SessionID *string `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

type SettingsGroupsResult struct {
	Groups []SettingsGroup `json:"groups"`
}

// Settings a query selects, with the session's own values when a session is named
type SettingsListParams struct {
	SessionID *string  `json:"session_id,omitzero"`
	Profile   *string  `json:"profile,omitzero"`
	Group     *string  `json:"group,omitzero"`
	Prefix    *string  `json:"prefix,omitzero"`
	Search    *string  `json:"search,omitzero"`
	Keys      []string `json:"keys,omitzero"`
	Advanced  *bool    `json:"advanced,omitzero"`
}

type SettingsListResult struct {
	Rows []SettingsRow `json:"rows"`
}

// Writes one setting at a scope, the profile when none is named
type SettingsSetParams struct {
	SessionID *string `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
	Key       string  `json:"key"`
	Value     any     `json:"value"`
	Scope     *string `json:"scope,omitzero"`
}

// Takes away the value one scope holds so the next layer applies
type SettingsResetParams struct {
	SessionID *string `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
	Key       string  `json:"key"`
	Scope     *string `json:"scope,omitzero"`
}

// Setting as it stands after a write and when the change takes effect
type SettingsWriteResult struct {
	Row   SettingsRow `json:"row"`
	Apply string      `json:"apply"`
}

var (
	SettingsGroups = NewMethod[SettingsGroupsParams, SettingsGroupsResult]("settings.groups")
	SettingsList   = NewMethod[SettingsListParams, SettingsListResult]("settings.list")
	SettingsSet    = NewMethod[SettingsSetParams, SettingsWriteResult]("settings.set")
	SettingsReset  = NewMethod[SettingsResetParams, SettingsWriteResult]("settings.reset")
)
