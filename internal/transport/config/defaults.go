package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Transports",
		About: "How requests reach each kind of provider API",
		Settings: []config.Setting{
			{Key: APITimeout, Title: "Request time limit", About: "Seconds one model request may take before it is given up; a provider's own limit wins over this",
				Default: 1800 * time.Second, Min: config.Bound(time.Second)},
			{Key: CallStaleTimeout, Title: "Silent request limit", About: "Seconds a request that isn't streamed may go without an answer; empty picks it " +
				"from the model and the request size, 0 for no limit. A provider's own silence limit wins over this", Default: time.Duration(0),
				Null: "picked from the model and request size", Min: config.Bound(0)},
			{Key: StreamStaleTimeout, Title: "Model silence limit", About: "Seconds a model may stay silent before the request is dropped and asked again; 0 for no limit",
				Default: 3 * time.Minute, Min: config.Bound(0)},
			{Key: LocalStaleTimeout, Title: "Local silence limit", About: "Seconds a model on this machine or network may stay silent before the request is dropped; 0 for no limit",
				Default: 15 * time.Minute, Min: config.Bound(0)},
			{Key: StreamMaxLifetime, Title: "Stream time limit", About: "Longest one model reply may keep streaming, in seconds; 0 for no limit", Default: time.Duration(0),
				Min: config.Bound(0)},
			{Key: StreamReadTimeout, Title: "Stream read limit", About: "Seconds the connection of a stream may stay quiet before the read gives up", Default: 2 * time.Minute,
				Env: []string{StreamReadTimeoutEnv}},
			{Key: StreamRetries, Title: "Stream retries", About: "Times a stream that broke before it delivered anything is asked again", Default: 2,
				Env: []string{StreamRetriesEnv}},
			{Key: StaleGiveup, Title: "Stalls before giving up", About: "Stalled streams in a row after which a session's requests fail at once until one answers; 0 never",
				Default: 5, Env: []string{StaleGiveupEnv}},
			{Key: DefaultHeaders, Title: "Extra headers", About: "Headers every request to an OpenAI style API carries, header names mapped to values; one set here " +
				"replaces the built-in one", Default: map[string]any{}},
			{Key: StreamDrainTimeout, Title: "Stream close wait", About: "Seconds a model stream that already gave its final answer may stay open before it is shut",
				Default: 2 * time.Second, Min: config.Bound(0)},
			{Key: PromptCacheTTL, Title: "Prompt cache time", About: "How long the provider keeps your prompt cached", Default: CacheShort,
				Options: config.Options(config.Option{Value: CacheShort, Title: "5 minutes"}, config.Option{Value: CacheLong, Title: "1 hour"},
					config.Option{Value: CacheAuto, Title: "Auto", About: "1 hour in chats, 5 minutes in scheduled and scripted runs"},
					config.Option{Value: CacheOff, Title: "Off", About: "No prompt caching"})},
		},
	}})
}
