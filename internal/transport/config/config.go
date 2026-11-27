package config

const Key = "transports"

// How long requests to a provider may take and stay silent, the same for every API
const (
	// Seconds one request may take before it is given up
	APITimeout = Key + ".api_timeout"
	// Seconds a request that isn't streamed may go without an answer; empty picks it from the model and the request's size
	CallStaleTimeout = Key + ".api_call_stale_timeout"
	// Seconds a stream may stay silent before it is dropped and asked again, and the same for a server on this network
	StreamStaleTimeout = Key + ".stream_stale_timeout"
	LocalStaleTimeout  = Key + ".local_stream_stale_timeout"
	// Seconds one reply may keep streaming, 0 for no limit
	StreamMaxLifetime = Key + ".stream_max_lifetime"
	// Seconds the socket of a stream may stay quiet before the read gives up
	StreamReadTimeout = Key + ".stream_read_timeout"
	// Times a stream that broke before it delivered anything is asked again
	StreamRetries = Key + ".stream_retries"
	// Stalled streams in a row after which a session's requests fail at once, 0 never
	StaleGiveup = Key + ".stream_stale_giveup"
	// Headers every request of the OpenAI APIs carries, over the built in ones
	DefaultHeaders = Key + ".default_headers"
	// Seconds a stream that already gave its final answer may stay open before it is shut
	StreamDrainTimeout = Key + ".stream_drain_timeout"
	// How long a provider keeps the prompt cached: 5m, 1h, auto, or off
	PromptCacheTTL = Key + ".prompt_cache_ttl"
)

// Values of PromptCacheTTL
const (
	CacheShort = "5m"
	CacheLong  = "1h"
	CacheAuto  = "auto"
	CacheOff   = "off"
)

// Variables that set them
const (
	StreamReadTimeoutEnv = "ICEBATH_STREAM_READ_TIMEOUT"
	StreamRetriesEnv     = "ICEBATH_STREAM_RETRIES"
	StaleGiveupEnv       = "ICEBATH_STREAM_STALE_GIVEUP"
)
