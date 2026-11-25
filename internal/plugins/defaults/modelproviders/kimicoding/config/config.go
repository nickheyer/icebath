package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	GlobalName         = "kimi-coding"
	GlobalKey          = pluginsconfig.Key + "." + GlobalName
	GlobalBaseURL      = GlobalKey + ".base_url"
	GlobalAPIKey       = GlobalKey + ".api_key"
	GlobalDefaultModel = GlobalKey + ".default_model"
)

const (
	CNName         = "kimi-coding-cn"
	CNKey          = pluginsconfig.Key + "." + CNName
	CNBaseURL      = CNKey + ".base_url"
	CNAPIKey       = CNKey + ".api_key"
	CNDefaultModel = CNKey + ".default_model"
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
