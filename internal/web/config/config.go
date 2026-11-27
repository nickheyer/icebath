package config

const (
	Key            = "web"
	Backend        = Key + ".backend"
	SearchBackend  = Key + ".search_backend"
	ExtractBackend = Key + ".extract_backend"
	FreeFallback   = Key + ".keyless_fallback"
	FreeRescue     = Key + ".keyless_rescue"
	ProviderTier   = Key + ".provider_tier"
	PrivateURLs    = Key + ".allow_private_urls"
	FakeIPRanges   = Key + ".fake_ip_ranges"
	HostedParsing  = Key + ".hosted_ocr"

	Blocklist            = Key + ".blocklist"
	BlocklistEnabled     = Blocklist + ".enabled"
	BlocklistDomains     = Blocklist + ".domains"
	BlocklistSharedFiles = Blocklist + ".shared_files"
)

// Plans web.provider_tier pins a backend to
const (
	Free = "free"
	Paid = "paid"
	Auto = "auto"
)
