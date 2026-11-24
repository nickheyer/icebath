package tuigateway

// One model of the fallback chain, tried when the ones before it fail
type FallbackRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Address of an endpoint of the user's own, empty for a known provider
	BaseURL string `json:"base_url,omitzero"`
}

type FallbackListParams struct {
	Profile *string `json:"profile,omitzero"`
}

// The fallback chain in the order it is tried, and the model it backs up
type FallbackListResult struct {
	// Model the chain backs up, worded as model (via provider), empty when none is set
	Primary string        `json:"primary,omitzero"`
	Entries []FallbackRow `json:"entries"`
}

// One change to the fallback chain: add, remove, move or clear
type FallbackChangeParams struct {
	Profile *string `json:"profile,omitzero"`
	Verb    string  `json:"verb"`
	// Provider, model and address of the entry an add puts at the end of the chain
	Provider string `json:"provider,omitzero"`
	Model    string `json:"model,omitzero"`
	BaseURL  string `json:"base_url,omitzero"`
	// Position of the entry a remove or a move is for, counted from zero
	Index int64 `json:"index,omitzero"`
	// Position a move puts the entry at
	To int64 `json:"to,omitzero"`
}

// Whether one entry of the chain can be used, and why not
type FallbackCheckRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Ready    bool   `json:"ready,omitzero"`
	// Ready, ready without a request sent, or the kind of failure
	Status string `json:"status"`
	Error  string `json:"error,omitzero"`
	// Milliseconds the model took to answer the request sent to it, zero when none was sent
	LatencyMs int64 `json:"latency_ms,omitzero"`
}

// Check of every entry of the chain, with one small request sent to each when Live
type FallbackCheckParams struct {
	Profile *string `json:"profile,omitzero"`
	Live    bool    `json:"live,omitzero"`
}

// Results in chain order
type FallbackCheckResult struct {
	Results []FallbackCheckRow `json:"results"`
}

// Fallback chain of the profile for the fallback models screen
var FallbackList = NewMethod[FallbackListParams, FallbackListResult]("fallback.list")

// One change to the fallback chain, answered with the chain after it
var FallbackChange = NewMethod[FallbackChangeParams, FallbackListResult]("fallback.change")

// Whether each entry of the fallback chain can be used
var FallbackCheck = NewMethod[FallbackCheckParams, FallbackCheckResult]("fallback.check")
