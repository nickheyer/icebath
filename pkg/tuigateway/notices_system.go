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

// Informational one liner for the session
var NoticeEvent = NewEvent[NoticePayload]("notice")

// Shows or replaces a keyed notice
var NotificationShowEvent = NewEvent[NotificationShowPayload]("notification.show")

// Withdraws the notice with this key
var NotificationClearEvent = NewEvent[NotificationClearPayload]("notification.clear")

// Battery of the host the gateway runs on
var SystemBattery = NewMethod[ProfileParams, SystemBatteryResult]("system.battery")

// Messages of one language pack and surface
var I18nCatalog = NewMethod[I18nCatalogParams, I18nCatalogResult]("i18n.catalog")
