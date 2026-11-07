package logging

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Logger name prefixes of each component, gating component logs and logs --component
var ComponentPrefixes = map[string][]string{
	"gateway": {"gateway", "hermes_plugins", "plugins.platforms", "agent.memory_caller_scope"},
	"agent":   {"agent", "run_agent", "model_tools", "batch_runner"},
	"tools":   {"tools"},
	"cli":     {"hermes_cli", "cli"},
	"cron":    {"cron"},
	"gui":     {"hermes_cli.web_server", "hermes_cli.pty_bridge", "hermes_cli.desktop", "tui_gateway", "uvicorn"},
}

// Component names in declaration order
var ComponentNames = []string{"gateway", "agent", "tools", "cli", "cron", "gui"}

var noisyLoggers = []string{
	"openai", "openai._base_client", "httpx", "httpcore", "asyncio", "hpack", "hpack.hpack",
	"grpc", "modal", "urllib3", "urllib3.connectionpool", "websockets", "charset_normalizer",
	"markdown_it",
}

// Pins the noisy third party loggers at WARNING
func quietNoisyLoggers() {
	for _, name := range noisyLoggers {
		GetLogger(name).SetLevel(LevelWarning)
	}
}

// Passes only records whose logger name starts with one of the prefixes
type componentFilter struct{ prefixes []string }

func (f componentFilter) Filter(rec *Record) bool {
	for _, p := range f.prefixes {
		if strings.HasPrefix(rec.Name, p) {
			return true
		}
	}
	return false
}

// Options of SetupLogging, zero values deferring to config.yaml logging and the built in defaults
type Options struct {
	Home        string
	LogLevel    string
	MaxSizeMB   int
	BackupCount int
	Mode        string
	Force       bool
}

var (
	setupMu            sync.Mutex
	loggingInitialized bool
)

// Configures the log files of a home and returns its logs directory, a second call a no op unless forced
func SetupLogging(ctx context.Context, o Options) (string, error) {
	h := o.Home
	if h == "" {
		h = home.Dir(ctx)
	}
	logDir := filepath.Join(h, "logs")
	if _, err := home.Mkdir(logDir); err != nil {
		return "", err
	}
	lineBufferPipedStdout()
	adopted, err := adoptSecondaryHome(h)
	if err != nil {
		return "", err
	}
	if adopted {
		return logDir, nil
	}
	cfgLevel, cfgMaxSize, cfgBackup := readLoggingConfig(ctx)
	var rawLevel any = "INFO"
	if o.LogLevel != "" {
		rawLevel = o.LogLevel
	} else if pyrepr.Truthy(cfgLevel) {
		rawLevel = cfgLevel
	}
	levelName, ok := rawLevel.(string)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'upper'", pyrepr.TypeName(rawLevel))
	}
	level, known := levelByName[pystr.Upper(levelName)]
	if !known {
		level = LevelInfo
	}
	var rawSize any = 5
	if o.MaxSizeMB != 0 {
		rawSize = o.MaxSizeMB
	} else if pyrepr.Truthy(cfgMaxSize) {
		rawSize = cfgMaxSize
	}
	sizeMB, ok := configNumber(rawSize)
	if !ok {
		return "", fmt.Errorf("TypeError: logging.max_size_mb must be a number, not %s", pyrepr.TypeName(rawSize))
	}
	var rawBackups any = 3
	if o.BackupCount != 0 {
		rawBackups = o.BackupCount
	} else if pyrepr.Truthy(cfgBackup) {
		rawBackups = cfgBackup
	}
	backups, ok := configNumber(rawBackups)
	if !ok || backups != float64(int(backups)) {
		return "", fmt.Errorf("TypeError: logging.backup_count must be an integer, not %s", pyrepr.TypeName(rawBackups))
	}
	maxBytes := sizeMB * 1024 * 1024
	formatter, err := newRedactingFormatter(logFormat, "")
	if err != nil {
		return "", err
	}
	specs := []struct {
		filename  string
		level     int
		maxBytes  float64
		backups   int
		component string
	}{
		{"agent.log", level, maxBytes, int(backups), ""},
		{"errors.log", LevelWarning, 2 * 1024 * 1024, 2, ""},
		{"gateway.log", LevelInfo, 5 * 1024 * 1024, 3, "gateway"},
		{"gui.log", LevelInfo, 10 * 1024 * 1024, 5, "gui"},
	}
	for _, s := range specs {
		if s.component != "" && o.Mode != s.component {
			continue
		}
		var filter Filter
		if s.component != "" {
			filter = componentFilter{prefixes: ComponentPrefixes[s.component]}
		}
		if err := addRotatingHandler(ctx, filepath.Join(logDir, s.filename), s.level, s.maxBytes, s.backups, formatter, filter); err != nil {
			return "", err
		}
	}
	clhMu.Lock()
	warn := windowsCLHFallback && !fallbackWarned
	reason := windowsCLHReason
	if warn {
		fallbackWarned = true
	}
	clhMu.Unlock()
	if warn {
		if reason == "" {
			reason = "portalocker probe failed"
		}
		GetLogger("hermes_logging").Warning(fmt.Sprintf("concurrent-log-handler unavailable on this Windows install (%s); "+
			"file logging fell back to stdlib rotation without rollover.", reason))
	}
	setupMu.Lock()
	defer setupMu.Unlock()
	if loggingInitialized && !o.Force {
		return logDir, nil
	}
	if rl := root.Level(); rl == LevelNotSet || rl > level {
		root.SetLevel(level)
	}
	quietNoisyLoggers()
	loggingInitialized = true
	return logDir, nil
}

// Configures the log files of home for mode, the setup_logging call a worker process makes at startup
func SetupHomeLogging(h, mode string) error {
	_, err := SetupLogging(context.Background(), Options{Home: h, Mode: mode})
	return err
}

// Numeric value of a config scalar, booleans counting as 0 and 1
func configNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Adds the DEBUG console handler of verbose mode once
func SetupVerboseLogging() error {
	formatter, err := newRedactingFormatter(logFormatVerbose, "%H:%M:%S")
	if err != nil {
		return err
	}
	for _, h := range root.Handlers() {
		if sh, ok := h.(*streamHandler); ok && sh.verbose {
			return nil
		}
	}
	h := newStreamHandler(safeStderr())
	h.SetLevel(LevelDebug)
	h.SetFormatter(formatter)
	h.verbose = true
	root.AddHandler(h)
	if root.Level() > LevelDebug {
		root.SetLevel(LevelDebug)
	}
	quietNoisyLoggers()
	GetLogger("rex-deploy").SetLevel(LevelInfo)
	installSlogDefault()
	return nil
}

// Routes a further home's records to its own files when this process already logs for another home
func adoptSecondaryHome(h string) (bool, error) {
	resolved, err := resolveHome(h)
	if err != nil {
		return false, nil
	}
	queueState.mu.Lock()
	known := knownLogHomes()
	queueState.mu.Unlock()
	if len(known) == 0 || known[resolved] {
		return false, nil
	}
	homes := make([]string, 0, len(known)+1)
	for k := range known {
		homes = append(homes, k)
	}
	slices.Sort(homes)
	return EnableProfileLogRouting(append(homes, resolved))
}

// Creates the logs directory and a configured rotating handler for path
func newFileHandler(ctx context.Context, path string, level int, maxBytes float64, backupCount int, formatter *Formatter) (*managedRotatingFileHandler, error) {
	if _, err := home.Mkdir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	clhMu.Lock()
	if windowsCLHFallback {
		maxBytes, backupCount = 0, 0
	}
	clhMu.Unlock()
	h, err := newManagedRotatingFileHandler(ctx, path, maxBytes, backupCount)
	if err != nil {
		return nil, err
	}
	h.SetLevel(level)
	h.SetFormatter(formatter)
	return h, nil
}

// Registers a queued rotating handler for path once per resolved path, routed when routing is on
func addRotatingHandler(ctx context.Context, path string, level int, maxBytes float64, backupCount int, formatter *Formatter, filter Filter) error {
	resolved := resolvePath(path)
	queueState.mu.Lock()
	existingHandlers := slices.Clone(queueState.handlers)
	queueState.mu.Unlock()
	for _, existing := range existingHandlers {
		switch e := existing.(type) {
		case *profileRoutingFileHandler:
			if e.routedLogPath == resolved {
				return nil
			}
			owner := filepath.Dir(filepath.Dir(resolved))
			if e.filename == filepath.Base(resolved) && (owner == e.defaultHome || slices.Contains(e.routedHomes(), owner)) {
				return nil
			}
		case *managedRotatingFileHandler:
			if resolvePath(e.baseFilename) == resolved {
				return nil
			}
		}
	}
	handler, err := newFileHandlerFn(ctx, path, level, maxBytes, backupCount, formatter)
	if err != nil {
		return err
	}
	if filter != nil {
		handler.AddFilter(filter)
	}
	var queued Handler = handler
	homes := map[string]bool{}
	routed := false
	for _, existing := range existingHandlers {
		if r, ok := existing.(*profileRoutingFileHandler); ok {
			routed = true
			homes[r.defaultHome] = true
			for _, p := range r.routedHomes() {
				homes[p] = true
			}
		}
	}
	if routed {
		sorted := make([]string, 0, len(homes))
		for k := range homes {
			sorted = append(sorted, k)
		}
		slices.Sort(sorted)
		queued = newProfileRoutingFileHandler(handler, sorted)
		quietly(handler.Close)
	}
	registerQueuedHandler(queued)
	return nil
}

// Level, max size and backup count from config.yaml logging, nil for each one absent or unreadable
func readLoggingConfig(ctx context.Context) (level, maxSizeMB, backupCount any) {
	defer func() {
		if recover() != nil {
			level, maxSizeMB, backupCount = nil, nil, nil
		}
	}()
	cfgPath := home.ConfigPath(home.Dir(ctx))
	var cfg map[string]any
	effective, err := config.LoadUserEffective(cfgPath, os.LookupEnv)
	if err == nil {
		cfg = effective
	} else {
		exists, err := config.ConfigExists(ctx, cfgPath)
		if err != nil {
			return nil, nil, nil
		}
		if exists {
			doc, err := config.ReadConfigDoc(ctx, cfgPath)
			if err != nil {
				return nil, nil, nil
			}
			m, ok := doc.(map[string]any)
			if !ok {
				return nil, nil, nil
			}
			cfg = m
		}
	}
	if len(cfg) == 0 {
		return nil, nil, nil
	}
	logCfg, ok := cfg["logging"].(map[string]any)
	if !ok {
		return nil, nil, nil
	}
	return logCfg["level"], logCfg["max_size_mb"], logCfg["backup_count"]
}
