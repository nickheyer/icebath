package tools

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

// One tools configuration run, its questions going through Ask and Flow when set else the console
type ToolsSetup struct {
	Ctx  context.Context
	Env  *Env
	Con  *cliui.Console
	Ask  setupflow.Asker
	Flow ToolsFlowHooks
}

// PickerProvider is one plugin registered provider as the tools picker lists it
type PickerProvider interface {
	Name() string
	DisplayName() string
	GetSetupSchema() map[string]any
}

// PickerRegistry is one plugin provider registry the tools picker reads, nil Get results meaning unregistered
type PickerRegistry struct {
	List         func(ctx context.Context, env *Env) []PickerProvider
	Get          func(ctx context.Context, env *Env, name string) PickerProvider
	BuiltinNames map[string]bool
}

// ToolsConfigHooks are the auth, media, browser and computer use facts the tools picker reads, each installed by its package
var ToolsConfigHooks struct {
	AuxPickerRows          func(ctx context.Context, currentProvider, currentModel, currentBaseURL string, maxModels int) ([]map[string]any, error)
	AuxPickerLabels        func(rows []map[string]any, currentProvider, currentBaseURL string) []string
	XAIOAuthStatus         func(ctx context.Context, h string) map[string]any
	CodexAuthStatus        func(ctx context.Context, h string) map[string]any
	CodexDeviceCodeLogin   func(ctx context.Context, h string, out io.Writer) (map[string]any, error)
	SaveCodexTokens        func(ctx context.Context, h string, tokens map[string]any, lastRefresh string) error
	XAIOAuthLoginFromSetup func(ctx context.Context, h string) bool
	SetupPromptChoice      func(question string, choices []string, def int) int
	SetupPrompt            func(question, def string, password bool) string
	SetupIsNoninteractive  func() bool

	STTModelCatalog      func(provider string) ([]string, bool)
	STTModelConfigKey    func(provider string) (string, bool)
	STTDefaultLocalModel func() string
	// Voices of the local Piper engine by name, and the one used when none is configured
	PiperVoices       func() (voices []string, defaultVoice string)
	DeepInfraModelIDs func(ctx context.Context, env *Env, tag string) []string
	FALModels         func() (ids []string, catalog map[string]map[string]any, defaultModel string)
	ImageGenRegistry  *PickerRegistry
	VideoGenRegistry  *PickerRegistry
	TTSRegistry       *PickerRegistry

	BrowserRegistry       *PickerRegistry
	LightpandaBinary      func(env *Env) string
	LightpandaInstallHint func() string
	ChromiumExecutable    func(env *Env) string
	ChromiumMissingHint   func(env *Env) string
	BakedChromiumFile     func() string

	ComputerUseProviders      func(env *Env) ([][2]string, error)
	ComputerUseDefaultBackend func() string
	CuaDriverInstallReady     func(env *Env) (bool, error)
	InstallCuaDriver          func(ctx context.Context, env *Env, con *cliui.Console) (bool, error)
}

// Panics naming a hook its package never installed, the import failure of the module behind it
func requireToolsConfigHook(name string, installed bool) {
	if !installed {
		panic("tools.ToolsConfigHooks." + name + " is not installed")
	}
}

// Missing setup helpers, the import failure reported when the setup module cannot load
var errSetupHelpersUnwired = errors.New("setup helpers are not installed")

// Home whose env file and config the run reads and writes, the env home else the context home
func (s *ToolsSetup) home() string {
	if s.Env != nil && s.Env.Home != "" {
		return s.Env.Home
	}
	return home.Dir(s.Ctx)
}

// Context carrying the run home so profile scoped lookups resolve against it
func (s *ToolsSetup) ctx() context.Context {
	ctx := s.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return home.WithOverride(ctx, s.home())
}

// Tool env the registries and media lookups read, one bound to the run home when the run carries none
func (s *ToolsSetup) toolEnv() *Env {
	if s.Env != nil {
		return s.Env
	}
	return &Env{Home: s.home()}
}

func (s *ToolsSetup) println(text string) { fmt.Fprintln(s.Con.Out, text) }

func (s *ToolsSetup) color(text string, codes ...string) string { return s.Con.Color(text, codes...) }

// Value of a key in the run home env file or process env, empty when unset
func (s *ToolsSetup) getEnvValue(key string) (string, error) {
	v, _, err := config.GetEnvValue(s.ctx(), s.home(), key)
	return v, err
}

func (s *ToolsSetup) saveEnvValue(key, value string) error {
	return config.SaveEnvValueContext(s.ctx(), s.home(), key, value)
}

// Fresh mutable effective config of the run home
func (s *ToolsSetup) loadConfig() (config.Tree, error) {
	return config.LoadContext(s.ctx(), s.home(), configLookup(s.Env))
}

func (s *ToolsSetup) saveConfig(cfg config.Tree) error {
	return config.SaveConfig(s.ctx(), s.home(), map[string]any(cfg), config.SaveOptions{})
}
