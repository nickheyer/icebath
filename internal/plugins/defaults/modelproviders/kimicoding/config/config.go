package config

const (
	GlobalName         = "kimi-coding"
	GlobalTitle        = "Kimi / Moonshot"
	GlobalBaseURL      = GlobalEndpoint
	GlobalDefaultModel = "kimi-k2-turbo-preview"
)

const (
	CNName         = "kimi-coding-cn"
	CNTitle        = "Kimi / Moonshot (China)"
	CNBaseURL      = CNEndpoint
	CNDefaultModel = "kimi-k2-turbo-preview"
)

const (
	GlobalAPIKeyEnv       = "KIMI_API_KEY"
	GlobalCodingAPIKeyEnv = "KIMI_CODING_API_KEY"
	GlobalBaseURLEnv      = "KIMI_BASE_URL"
	CNAPIKeyEnv           = "KIMI_CN_API_KEY"
)

const (
	GlobalEndpoint  = "https://api.moonshot.ai/v1"
	GlobalModelsURL = GlobalEndpoint + "/models"
	CNEndpoint      = "https://api.moonshot.cn/v1"
	CNModelsURL     = CNEndpoint + "/models"
)
