package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key             = webconfig.Key + ".xai"
	Model           = Key + ".model"
	Timeout         = Key + ".timeout"
	AllowedDomains  = Key + ".allowed_domains"
	ExcludedDomains = Key + ".excluded_domains"
)

// Model of a search when web.xai.model is blank
const DefaultModel = "grok-build-0.1"
