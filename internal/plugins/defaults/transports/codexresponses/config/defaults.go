package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: transportconfig.Key,
		Title:  "OpenAI responses API",
		About:  "Requests to providers that use the OpenAI Responses API",
		Order:  10,
		Settings: []config.Setting{
			{Key: TextVerbosity, Title: "Answer length", About: "How long the answers of the model are.", Default: "",
				Options: config.Options(config.Option{Title: "The model's default"}, config.Option{Value: "low", Title: "Short"},
					config.Option{Value: "medium", Title: "Medium"}, config.Option{Value: "high", Title: "Long"})},
			{Key: FirstEventTimeout, Title: "First output wait", About: "How long a stream may wait for its first output before it is sent again.",
				Default: 2 * time.Minute, Zero: "No limit", Env: []string{FirstEventTimeoutEnv}, Advanced: true},
			{Key: FirstEventLargeAbove, Title: "Large request size", About: "The request size from which the first output wait grows to the silence limit.",
				Default: 10000, Zero: "Never", Counts: "tokens", Env: []string{FirstEventLargeAboveEnv}, Advanced: true},
			{Key: FirstEventStrict, Title: "Fixed first output wait", About: "Keep the first output wait as it is for large requests.", Default: false,
				Env: []string{FirstEventStrictEnv}, Advanced: true},
			{Key: FirstEventMax, Title: "First output wait cap", About: "The longest that the first output wait may grow to.", Default: time.Duration(0),
				Zero: "No limit", Env: []string{FirstEventMaxEnv}, Advanced: true},
			{Key: EventTimeout, Title: "Silence limit", About: "How long a stream may go silent after its first output before it is dropped.",
				Default: time.Duration(0), Null: "automatic", Zero: "No limit", Env: []string{EventTimeoutEnv}, Advanced: true},
			{Key: HardTimeout, Title: "Silence limit cap", About: "The longest that the silence limit of a large request may grow to.",
				Default: 1500 * time.Second, Zero: "No limit", Env: []string{HardTimeoutEnv}, Advanced: true},
		},
	}})
}
