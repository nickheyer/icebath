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
		About:  "Requests to providers that use the OpenAI Responses API, ChatGPT's Codex backend among them",
		Settings: []config.Setting{
			{Key: FirstEventTimeout, Title: "First event limit", About: "Seconds a stream may go without its first event before it is dropped and asked again; " +
				"0 for no limit", Default: 2 * time.Minute, Env: []string{FirstEventTimeoutEnv}},
			{Key: EventTimeout, Title: "Event silence limit", About: "Seconds a stream may go silent after its first event before it is dropped; empty " +
				"picks it from the request's size", Default: time.Duration(0), Null: "picked from the request's size", Env: []string{EventTimeoutEnv}},
			{Key: FirstEventLargeAbove, Title: "Large request size", About: "Tokens of context from which the first event limit is raised to the event " +
				"limit; 0 never", Default: 10000, Env: []string{FirstEventLargeAboveEnv}},
			{Key: FirstEventStrict, Title: "Strict first event limit", About: "Keeps the first event limit as it is for large requests", Default: false,
				Env: []string{FirstEventStrictEnv}},
			{Key: FirstEventMax, Title: "First event limit cap", About: "Most seconds the first event limit is raised to; 0 for no cap", Default: time.Duration(0),
				Env: []string{FirstEventMaxEnv}},
			{Key: HardTimeout, Title: "Silence ceiling", About: "Most seconds a request's silence limit is raised to for a large context; 0 for no cap",
				Default: 1500 * time.Second, Env: []string{HardTimeoutEnv}},
			{Key: TextVerbosity, Title: "Answer length", About: "How long the model's answers are; empty for the model's own", Default: "",
				Options: config.Options(config.Option{Title: "The model's"}, config.Option{Value: "low", Title: "Low"},
					config.Option{Value: "medium", Title: "Medium"}, config.Option{Value: "high", Title: "High"})},
		},
	}})
}
