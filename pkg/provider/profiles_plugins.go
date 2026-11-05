package provider

import "net/url"

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// Mixture of Agents virtual built in row the auth registry carries beside the registry rows
func moaVirtualRow() profileRow {
	return profileRow{name: "moa", display: "Mixture of Agents", base: MoaBaseURL, authType: AuthTypeVirtual, noListing: true, noHealth: true}
}
