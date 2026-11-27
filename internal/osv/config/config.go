package config

const (
	Key = "osv"
	// Where the malware check asks
	Endpoint = Key + ".endpoint"
	// Seconds an answer of the malware check is kept
	CacheTTL = Key + ".cache_ttl"
)
