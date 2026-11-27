package config

import webconfig "github.com/nickheyer/icebath/internal/web/config"

const (
	Key        = webconfig.Key + ".ddgs"
	Region     = Key + ".region"
	SafeSearch = Key + ".safesearch"
	TimeLimit  = Key + ".timelimit"
	Proxy      = Key + ".proxy"
)
