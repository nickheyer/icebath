package tuigateway

// Informational one line notice for the session
type NoticePayload struct {
	Message string `json:"message"`
}

// Keyed out of band notice, a toast for ttl_ms or sticky in the status bar until cleared
type NotificationShowPayload struct {
	Text   string `json:"text"`
	Level  string `json:"level"`
	Kind   string `json:"kind"`
	TTLMs  *int64 `json:"ttl_ms,omitzero"`
	Key    string `json:"key,omitzero"`
	ID     string `json:"id,omitzero"`
	Detail string `json:"detail,omitzero"`
}

type NotificationClearPayload struct {
	Key string `json:"key"`
}

type BatteryCategory string

func (BatteryCategory) Values() []string { return []string{"good", "warn", "bad", "critical", "dim"} }

// Host battery for the status bar, available false on a host without one
type SystemBatteryResult struct {
	Available bool            `json:"available"`
	Percent   *int64          `json:"percent,omitzero"`
	Plugged   *bool           `json:"plugged,omitzero"`
	Category  BatteryCategory `json:"category"`
}

type LocaleSurface string

func (LocaleSurface) Values() []string { return []string{"core", "tui"} }

type I18nCatalogParams struct {
	Profile *string       `json:"profile,omitzero"`
	Lang    string        `json:"lang"`
	Surface LocaleSurface `json:"surface,omitzero"`
}

// Pack and overlay messages of one language and surface, merged by the UI over its bundled catalog
type I18nCatalogResult struct {
	Lang     string            `json:"lang"`
	Surface  LocaleSurface     `json:"surface"`
	Messages map[string]string `json:"messages"`
}

// Language the UI can be shown in: its own name for itself, whether it reads right to left, the layer that supplies it and the surfaces it covers
type LanguageOption struct {
	ID       string   `json:"id"`
	Endonym  string   `json:"endonym"`
	RTL      bool     `json:"rtl"`
	Source   string   `json:"source"`
	Surfaces []string `json:"surfaces"`
}

// Asks for the languages of a profile, and for the id a code, alias or regional tag stands for
type I18nLanguagesParams struct {
	Profile *string `json:"profile,omitzero"`
	Resolve string  `json:"resolve,omitzero"`
}

// Languages of a profile, the language in effect, and the id the value to resolve stands for, empty when no language answers to it
type I18nLanguagesResult struct {
	Languages []LanguageOption `json:"languages"`
	Current   string           `json:"current"`
	Resolved  string           `json:"resolved,omitzero"`
}

// Informational one liner for the session
var NoticeEvent = NewEvent[NoticePayload]("notice")

// Shows or replaces a keyed notice
var NotificationShowEvent = NewEvent[NotificationShowPayload]("notification.show")

// Withdraws the notice with this key
var NotificationClearEvent = NewEvent[NotificationClearPayload]("notification.clear")

// Battery of the host the gateway runs on
var SystemBattery = NewMethod[ProfileParams, SystemBatteryResult]("system.battery")

// Languages the UI can be shown in
var I18nLanguages = NewMethod[I18nLanguagesParams, I18nLanguagesResult]("i18n.languages")

// Messages of one language pack and surface
var I18nCatalog = NewMethod[I18nCatalogParams, I18nCatalogResult]("i18n.catalog")
