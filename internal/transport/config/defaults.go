package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Connections",
		About: "Time limits and retries for requests to providers",
		Group: config.GroupModels,
		Order: 50,
		Settings: []config.Setting{
			{Key: APITimeout, Title: "Request time limit", About: "How long one model request may take before it is given up.",
				Help: "A time limit set on a provider wins over this one.", Default: 1800 * time.Second, Min: config.Bound(time.Second), Advanced: true},
			{Key: CallStaleTimeout, Title: "Silent request limit", About: "How long a request that isn't streamed may wait for an answer.",
				Help: "A silence limit set on a provider wins over this one.", Default: time.Duration(0), Null: "automatic", Zero: "No limit",
				Min: config.Bound(0), Advanced: true},
			{Key: StreamStaleTimeout, Title: "Model silence limit", About: "How long a model may stay silent before the request is sent again.",
				Default: 3 * time.Minute, Zero: "No limit", Min: config.Bound(0), Advanced: true},
			{Key: LocalStaleTimeout, Title: "Local silence limit", About: "How long a model on your own network may stay silent before the request is dropped.",
				Default: 15 * time.Minute, Zero: "No limit", Min: config.Bound(0), Advanced: true},
			{Key: StreamMaxLifetime, Title: "Stream time limit", About: "The longest one model reply may keep streaming.", Default: time.Duration(0),
				Zero: "No limit", Min: config.Bound(0), Advanced: true},
			{Key: StreamReadTimeout, Title: "Stream read limit", About: "How long the connection of a stream may stay quiet before the read gives up.",
				Default: 2 * time.Minute, Env: []string{StreamReadTimeoutEnv}, Advanced: true},
			{Key: StreamDrainTimeout, Title: "Stream close wait", About: "How long a stream that already gave its final answer may stay open.",
				Default: 2 * time.Second, Min: config.Bound(0), Advanced: true},
			{Key: StreamRetries, Title: "Stream retries", About: "How many times a stream that broke before it sent anything is tried again.",
				Default: 2, Counts: "retries", Least: config.Num(0), Env: []string{StreamRetriesEnv}, Advanced: true},
			{Key: StaleGiveup, Title: "Stalls before failing", About: "How many stalled streams in a row make the requests of a session fail at once.",
				Default: 5, Zero: "Never", Counts: "stalls", Env: []string{StaleGiveupEnv}, Advanced: true},
			{Key: DefaultHeaders, Title: "Extra headers", About: "Headers sent with every request to an OpenAI-style API.",
				Help: "A header set here replaces the built-in one of the same name.", Default: map[string]any{}, Advanced: true,
				Entry: &config.Entry{Noun: "header", Name: "Header name", Fields: []config.Setting{
					{Title: "Value", About: "The text the header carries.", Default: ""}}}},
			{Key: PromptCacheTTL, Title: "Prompt cache time", About: "How long the provider keeps the text of your requests cached.", Default: CacheShort, Advanced: true,
				Options: config.Options(config.Option{Value: CacheShort, Title: "5 minutes"}, config.Option{Value: CacheLong, Title: "1 hour"},
					config.Option{Value: CacheAuto, Title: "Automatic", About: "1 hour in chats, 5 minutes in scheduled and scripted runs"},
					config.Option{Value: CacheOff, Title: "Off", About: "Nothing is cached"})},
		},
	}})
}
