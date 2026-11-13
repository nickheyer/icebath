package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

const (
	logLevelEnv       = "NEMO_RELAY_LOG"
	logStderrEnv      = "NEMO_RELAY_LOG_STDERR"
	logStderrFormatEnv = "NEMO_RELAY_LOG_STDERR_FORMAT"
	logConfigPathEnv  = "NEMO_RELAY_LOG_CONFIG_PATH"
)

const (
	DefaultFileSinkQueueEntries    = 1024
	DefaultFileFlushIntervalMillis = 1000
	MaxFileSinkQueueEntries        = 8192
	MaxFileSinkRetainedFiles       = 9
)

// Invalid logging configuration carrying the message the runtime reports
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return "invalid argument: " + e.Message }

func invalid(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}

// Verbosity of an operational record, ordered from most to least severe
type Level int

const (
	LevelOff Level = iota
	LevelError
	LevelWarn
	LevelInfo
	LevelDebug
	LevelTrace
)

// Lowercase name of the level as configuration spells it
func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	case LevelTrace:
		return "trace"
	}
	return "off"
}

// Level named by configuration text, accepting warning as warn
func ParseLevel(raw string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "error":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	}
	return LevelOff, invalid("invalid logging level '%s'; expected error, warn, info, debug, or trace", raw)
}

// Record layout of one sink
type LogFormat int

const (
	LogFormatHuman LogFormat = iota
	LogFormatJSONL
)

// Format named by configuration text, accepting json as jsonl
func ParseLogFormat(raw string) (LogFormat, error) {
	other := strings.ToLower(strings.TrimSpace(raw))
	switch other {
	case "human":
		return LogFormatHuman, nil
	case "jsonl", "json":
		return LogFormatJSONL, nil
	}
	return LogFormatHuman, invalid("invalid logging format '%s'; expected human or jsonl", other)
}

// What happens when a file sink cannot open
type FileLogSinkFailurePolicy int

const (
	FileLogSinkFailurePolicyRequired FileLogSinkFailurePolicy = iota
	FileLogSinkFailurePolicyOptional
)

// Size based rotation of one file sink
type FileLogRotationConfig struct {
	maxFileSizeBytes uint64
	retainedFiles    int
}

// Rotation settings validated against the per sink backup limit
func NewFileLogRotationConfig(maxFileSizeBytes uint64, retainedFiles int) (FileLogRotationConfig, error) {
	if maxFileSizeBytes == 0 {
		return FileLogRotationConfig{}, invalid("logging sink max_file_size_bytes must be greater than 0")
	}
	if retainedFiles == 0 {
		return FileLogRotationConfig{}, invalid("logging sink retained_files must be greater than 0")
	}
	if retainedFiles > MaxFileSinkRetainedFiles {
		return FileLogRotationConfig{}, invalid("logging sink retained_files %d exceeds maximum %d backup files per sink", retainedFiles, MaxFileSinkRetainedFiles)
	}
	return FileLogRotationConfig{maxFileSizeBytes: maxFileSizeBytes, retainedFiles: retainedFiles}, nil
}

func (r FileLogRotationConfig) MaxFileSizeBytes() uint64 { return r.maxFileSizeBytes }
func (r FileLogRotationConfig) RetainedFiles() int       { return r.retainedFiles }

// One file sink
type FileLogSinkConfig struct {
	Path          string
	Level         Level
	Format        LogFormat
	QueueCapacity int
	Rotation      *FileLogRotationConfig
	FailurePolicy FileLogSinkFailurePolicy
}

// File sink defaults the runtime applies before overrides
func DefaultFileLogSinkConfig() FileLogSinkConfig {
	return FileLogSinkConfig{
		Path: filepath.Join(".nemo-relay", "logs", "relay.log.jsonl"), Level: LevelInfo, Format: LogFormatJSONL,
		QueueCapacity: DefaultFileSinkQueueEntries,
	}
}

// Complete operational logging configuration
type LoggingConfig struct {
	Level               Level
	StderrEnabled       bool
	StderrFormat        LogFormat
	Sinks               []FileLogSinkConfig
	FlushIntervalMillis uint64
}

// Error level records to stderr in the human format
func DefaultLoggingConfig() LoggingConfig {
	return LoggingConfig{Level: LevelError, StderrEnabled: true, StderrFormat: LogFormatHuman, FlushIntervalMillis: DefaultFileFlushIntervalMillis}
}

// Configuration the environment selects, false when no logging variable is set
func ConfigFromEnvironment() (LoggingConfig, bool, error) {
	level, err := environmentValue(logLevelEnv)
	if err != nil {
		return LoggingConfig{}, false, err
	}
	stderrEnabled, err := environmentValue(logStderrEnv)
	if err != nil {
		return LoggingConfig{}, false, err
	}
	stderrFormat, err := environmentValue(logStderrFormatEnv)
	if err != nil {
		return LoggingConfig{}, false, err
	}
	configPath, err := environmentValue(logConfigPathEnv)
	if err != nil {
		return LoggingConfig{}, false, err
	}
	if configPath != nil && (level != nil || stderrEnabled != nil || stderrFormat != nil) {
		return LoggingConfig{}, false, invalid("%s cannot be combined with %s or %s or %s", logConfigPathEnv, logLevelEnv, logStderrEnv, logStderrFormatEnv)
	}
	if configPath != nil {
		config, err := ConfigFromFilePath(*configPath)
		return config, err == nil, err
	}
	if level == nil && stderrEnabled == nil && stderrFormat == nil {
		return LoggingConfig{}, false, nil
	}
	config := DefaultLoggingConfig()
	if level != nil {
		if config.Level, err = ParseLevel(*level); err != nil {
			return LoggingConfig{}, false, err
		}
	}
	if stderrEnabled != nil {
		if config.StderrEnabled, err = parseBool(logStderrEnv, *stderrEnabled); err != nil {
			return LoggingConfig{}, false, err
		}
	}
	if stderrFormat != nil {
		if config.StderrFormat, err = ParseLogFormat(*stderrFormat); err != nil {
			return LoggingConfig{}, false, err
		}
	}
	return config, true, nil
}

// Configuration read from an absolute toml file with a logging table
func ConfigFromFilePath(path string) (LoggingConfig, error) {
	if !filepath.IsAbs(path) {
		return LoggingConfig{}, invalid("logging configuration path must be absolute: %s", path)
	}
	if filepath.Ext(path) != ".toml" {
		return LoggingConfig{}, invalid("logging configuration path must identify a .toml file: %s", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return LoggingConfig{}, invalid("failed to read logging configuration %s: %s", path, ioErrorText(err))
	}
	if !utf8.Valid(contents) {
		return LoggingConfig{}, invalid("failed to read logging configuration %s: stream did not contain valid UTF-8", path)
	}
	config, err := ConfigFromTOMLDocument(string(contents))
	if err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			return LoggingConfig{}, invalid("invalid logging configuration in %s: %s", path, err.Error())
		}
		return LoggingConfig{}, err
	}
	return config, nil
}

var (
	loggingFields = []string{"level", "stderr_enabled", "stderr_format", "flush_interval_millis", "sinks"}
	sinkFields    = []string{"failure_policy", "path", "level", "format", "queue_capacity", "max_file_size_bytes", "retained_files"}
)

// Configuration of a toml document's logging table
func ConfigFromTOMLDocument(contents string) (LoggingConfig, error) {
	var document map[string]any
	if err := toml.Unmarshal([]byte(contents), &document); err != nil {
		return LoggingConfig{}, invalid("invalid logging TOML: %s", err.Error())
	}
	raw, ok := document["logging"]
	if !ok {
		return LoggingConfig{}, invalid("logging configuration requires a [logging] section")
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return LoggingConfig{}, invalid("invalid logging TOML: invalid type: %s, expected struct RawLoggingConfig", tomlTypeName(raw))
	}
	if err := denyUnknown(table, loggingFields); err != nil {
		return LoggingConfig{}, err
	}
	config := DefaultLoggingConfig()
	if v, ok := table["level"]; ok {
		s, err := tomlString(v, "a string")
		if err != nil {
			return LoggingConfig{}, err
		}
		if config.Level, err = ParseLevel(s); err != nil {
			return LoggingConfig{}, err
		}
	}
	if v, ok := table["stderr_enabled"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return LoggingConfig{}, invalid("invalid logging TOML: invalid type: %s, expected a boolean", tomlTypeName(v))
		}
		config.StderrEnabled = b
	}
	if v, ok := table["stderr_format"]; ok {
		s, err := tomlString(v, "a string")
		if err != nil {
			return LoggingConfig{}, err
		}
		if config.StderrFormat, err = ParseLogFormat(s); err != nil {
			return LoggingConfig{}, err
		}
	}
	if v, ok := table["flush_interval_millis"]; ok {
		n, err := tomlUnsigned(v)
		if err != nil {
			return LoggingConfig{}, err
		}
		config.FlushIntervalMillis = n
	}
	if v, ok := table["sinks"]; ok {
		list, isList := v.([]any)
		if !isList {
			return LoggingConfig{}, invalid("invalid logging TOML: invalid type: %s, expected a sequence", tomlTypeName(v))
		}
		for _, item := range list {
			sinkTable, isTable := item.(map[string]any)
			if !isTable {
				return LoggingConfig{}, invalid("invalid logging TOML: invalid type: %s, expected struct RawFileLogSinkConfig", tomlTypeName(item))
			}
			sink, err := resolveSink(sinkTable, config.Level)
			if err != nil {
				return LoggingConfig{}, err
			}
			config.Sinks = append(config.Sinks, sink)
		}
	}
	return config, nil
}

// File sink of one sinks entry, its level defaulting to the logging level
func resolveSink(table map[string]any, defaultLevel Level) (FileLogSinkConfig, error) {
	if err := denyUnknown(table, sinkFields); err != nil {
		return FileLogSinkConfig{}, err
	}
	sink := FileLogSinkConfig{Level: defaultLevel, Format: LogFormatJSONL, QueueCapacity: DefaultFileSinkQueueEntries}
	if v, ok := table["failure_policy"]; ok {
		s, err := tomlString(v, "a string")
		if err != nil {
			return sink, err
		}
		switch s {
		case "required":
			sink.FailurePolicy = FileLogSinkFailurePolicyRequired
		case "optional":
			sink.FailurePolicy = FileLogSinkFailurePolicyOptional
		default:
			return sink, invalid("invalid logging TOML: unknown variant `%s`, expected `required` or `optional`", s)
		}
	}
	path, ok := table["path"]
	if !ok {
		return sink, invalid("logging sink requires path")
	}
	p, err := tomlString(path, "path string")
	if err != nil {
		return sink, err
	}
	if p == "" {
		return sink, invalid("logging sink path must not be empty")
	}
	sink.Path = p
	if v, ok := table["level"]; ok {
		s, err := tomlString(v, "a string")
		if err != nil {
			return sink, err
		}
		if sink.Level, err = ParseLevel(s); err != nil {
			return sink, err
		}
	}
	if v, ok := table["format"]; ok {
		s, err := tomlString(v, "a string")
		if err != nil {
			return sink, err
		}
		if sink.Format, err = ParseLogFormat(s); err != nil {
			return sink, err
		}
	}
	if v, ok := table["queue_capacity"]; ok {
		n, err := tomlUnsigned(v)
		if err != nil {
			return sink, err
		}
		switch {
		case n == 0:
			return sink, invalid("logging sink queue_capacity must be greater than 0")
		case n > MaxFileSinkQueueEntries:
			return sink, invalid("logging sink queue_capacity %d exceeds maximum %d entries per file sink", n, MaxFileSinkQueueEntries)
		}
		sink.QueueCapacity = int(n)
	}
	maxSize, hasMax := table["max_file_size_bytes"]
	retained, hasRetained := table["retained_files"]
	switch {
	case hasMax && hasRetained:
		size, err := tomlUnsigned(maxSize)
		if err != nil {
			return sink, err
		}
		count, err := tomlUnsigned(retained)
		if err != nil {
			return sink, err
		}
		rotation, err := NewFileLogRotationConfig(size, int(min(count, uint64(MaxFileSinkRetainedFiles+1))))
		if err != nil {
			if count > MaxFileSinkRetainedFiles {
				return sink, invalid("logging sink retained_files %d exceeds maximum %d backup files per sink", count, MaxFileSinkRetainedFiles)
			}
			return sink, err
		}
		sink.Rotation = &rotation
	case hasMax || hasRetained:
		return sink, invalid("logging sink max_file_size_bytes and retained_files must be configured together")
	}
	return sink, nil
}

// Rejects keys outside the allowed set with the deserializer's unknown field text
func denyUnknown(table map[string]any, allowed []string) error {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		known := false
		for _, a := range allowed {
			known = known || a == k
		}
		if known {
			continue
		}
		quoted := make([]string, len(allowed))
		for i, a := range allowed {
			quoted[i] = "`" + a + "`"
		}
		return invalid("invalid logging TOML: unknown field `%s`, expected one of %s", k, strings.Join(quoted, ", "))
	}
	return nil
}

func tomlString(v any, expected string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", invalid("invalid logging TOML: invalid type: %s, expected %s", tomlTypeName(v), expected)
	}
	return s, nil
}

func tomlUnsigned(v any) (uint64, error) {
	switch n := v.(type) {
	case int64:
		if n < 0 {
			return 0, invalid("invalid logging TOML: invalid value: integer `%d`, expected u64", n)
		}
		return uint64(n), nil
	case uint64:
		return n, nil
	}
	return 0, invalid("invalid logging TOML: invalid type: %s, expected u64", tomlTypeName(v))
}

// Deserializer description of a toml value's type
func tomlTypeName(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("string %q", t)
	case bool:
		return fmt.Sprintf("boolean `%t`", t)
	case int64:
		return fmt.Sprintf("integer `%d`", t)
	case float64:
		return fmt.Sprintf("floating point `%v`", t)
	case []any:
		return "sequence"
	case map[string]any:
		return "map"
	}
	return "value"
}

func parseBool(name, raw string) (bool, error) {
	other := strings.ToLower(strings.TrimSpace(raw))
	switch other {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, invalid("invalid %s value '%s'; expected true or false", name, other)
}

// Value of a set environment variable, an error for an empty or non Unicode value
func environmentValue(name string) (*string, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil, nil
	}
	if value == "" {
		return nil, invalid("%s must not be empty when set", name)
	}
	if !utf8.ValidString(value) {
		return nil, invalid("%s must contain valid Unicode", name)
	}
	return &value, nil
}

// Operating system error text with the path prefix removed
func ioErrorText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return osErrorText(pe.Err)
	}
	return err.Error()
}
