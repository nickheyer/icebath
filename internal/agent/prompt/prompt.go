// System prompt assembly, project instruction files, turn notes, reference expansion and subdirectory hints
package prompt

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const (
	TierStable   = "stable"
	TierContext  = "context"
	TierVolatile = "volatile"

	defaultProfile   = "default"
	alibabaProvider  = "alibaba"
	envIgnoreRules   = "ICEBATH_IGNORE_RULES"
	promptDateLayout = "Monday, January 02, 2006"
	sessionStampForm = "20060102_150405"
	dayStampForm     = "20060102"
)

var sessionStamp = regexp.MustCompile(`^([0-9]{8})_([0-9]{6})`)

type PlatformHints interface {
	PlatformHint(ctx context.Context, platform string) string
}

type Options struct {
	Config        *config.Store
	Homes         *home.Resolver
	Host          *hostinfo.Host
	Env           *env.Manager
	Providers     *env.Providers
	Probe         BackendProber
	BotScreen     BotScreen
	Distributions Distributions
	PlatformHints PlatformHints
	SafeWorker    bool
}

type Builder struct {
	cfg        *config.Store
	homes      *home.Resolver
	host       *hostinfo.Host
	env        *env.Manager
	providers  *env.Providers
	probe      BackendProber
	screen     BotScreen
	dists      Distributions
	hints      PlatformHints
	safeWorker bool
	git        *gitRunner

	mu     sync.Mutex
	probes map[[3]string]string
}

type MemoryBlocks struct {
	MemoryEnabled  bool
	ProfileEnabled bool
	Memory         string
	User           string
	External       string
}

type SkillIndex struct {
	Text string
}

type SkillQuery struct {
	ToolNames         tool.Names
	Toolsets          []string
	CompactCategories []string
	Platform          string
}

type Tier struct {
	Name string
	Text string
}

type Prompt struct {
	Text      string
	Static    string
	Tiers     []Tier
	Warnings  []string
	Workspace *WorkspacePin
	Timeless  bool
}

type Inputs struct {
	Config           *config.Config
	Model            string
	Provider         string
	Mode             wire.Mode
	Platform         string
	SessionID        string
	ConversationRoot string
	SessionStart     time.Time
	PassSessionID    bool
	ContextLength    int
	ToolNames        tool.Names
	System           string
	SkipContextFiles bool
	LoadSoulIdentity bool
	LaunchArtifact   bool
	Memory           MemoryBlocks
	Skills           SkillIndex
	AutoLoad         string
	PluginSections   []Section
	Workspace        *WorkspacePin
	StoredPrompt     string
	Oneshot          bool
	KanbanWorker     bool
	CronDelivery     string
	BotMode          []string
	Timeless         bool
}

func New(o Options) (*Builder, error) {
	switch {
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "prompt.Options.Config"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "prompt.Options.Homes"}
	case o.Host == nil:
		return nil, &tool.UnfilledError{Name: "prompt.Options.Host"}
	case o.Env == nil:
		return nil, &tool.UnfilledError{Name: "prompt.Options.Env"}
	}
	return &Builder{cfg: o.Config, homes: o.Homes, host: o.Host, env: o.Env, providers: o.Providers, probe: o.Probe,
		screen: o.BotScreen, dists: o.Distributions, hints: o.PlatformHints, safeWorker: o.SafeWorker,
		git: newGitRunner(), probes: map[[3]string]string{}}, nil
}

func AutoLoad(ctx context.Context, load func(ctx context.Context) (text string, missing []string, err error)) string {
	if truthy(os.Getenv(envIgnoreRules)) {
		return ""
	}
	text, missing, err := load(ctx)
	if err != nil {
		slog.Debug("skills.auto_load: injection skipped", "error", err)
		return ""
	}
	if len(missing) > 0 {
		slog.Warn("skills.auto_load: skill(s) not found or disabled, skipped: " + strings.Join(missing, ", "))
	}
	return text
}

func (b *Builder) ProfileName(ctx context.Context) string {
	h, err := b.homes.Home(ctx)
	if err != nil {
		return defaultProfile
	}
	if name, ok := home.ProfileName(h); ok && name != "" {
		return name
	}
	return defaultProfile
}

func (b *Builder) SkillsQuery(ctx context.Context, in Inputs, toolsets []string) (SkillQuery, bool) {
	if !skillTools(in.ToolNames) {
		return SkillQuery{}, false
	}
	q := SkillQuery{ToolNames: in.ToolNames, Toolsets: toolsets, Platform: in.Platform}
	cwd, _, err := env.ResolveContextCwd(ctx, b.env.Settings(), true)
	if err != nil {
		return q, true
	}
	if mode, err := b.Mode(ctx, in.Platform, cwd, ""); err == nil {
		q.CompactCategories = mode.CompactSkillCategories()
	}
	return q, true
}

func (b *Builder) activeProfileLine(ctx context.Context) (string, error) {
	h, err := b.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	root, err := home.DefaultRoot()
	if err != nil {
		return "", err
	}
	name := b.ProfileName(ctx)
	if name == defaultProfile {
		return "Active icebath profile: default. Other profiles (if any) live " +
			"under " + root + "/profiles/<name>/. Each profile has its own " +
			"skills/, plugins/, cron/, and memories/ that affect a different " +
			"session than this one. Do not modify another profile's " +
			"skills/plugins/cron/memories unless the user explicitly directs " +
			"you to.", nil
	}
	return "Active icebath profile: " + name + ". This session reads " +
		"and writes " + h + "/. The default " +
		"profile's data lives at " + root + "/skills/, " + root + "/plugins/, " +
		root + "/cron/, " + root + "/memories/ — those belong to a " +
		"different session run from a different shell. Do NOT modify " +
		"another profile's skills/plugins/cron/memories unless the user " +
		"explicitly directs you to.", nil
}

func sessionStartLike(in Inputs, now time.Time) time.Time {
	for _, candidate := range []string{in.ConversationRoot, in.SessionID} {
		m := sessionStamp.FindStringSubmatch(candidate)
		if m == nil {
			continue
		}
		if t, err := time.ParseInLocation(sessionStampForm, m[1]+"_"+m[2], time.Local); err == nil {
			return t.In(now.Location())
		}
	}
	if !in.SessionStart.IsZero() {
		return in.SessionStart.In(now.Location())
	}
	return now
}

func (b *Builder) zoneBits(ctx context.Context, now time.Time) []string {
	iana := ""
	if name, loc := b.cfg.TimeZone(ctx); loc != nil {
		iana = name
	}
	var bits []string
	if iana != "" {
		bits = append(bits, iana)
	}
	if abbrev := now.Format("MST"); abbrev != "" && abbrev != iana {
		bits = append(bits, abbrev)
	}
	if offset := now.Format("-0700"); len(offset) >= 3 {
		bits = append(bits, "UTC"+offset[:3]+":"+offset[3:])
	}
	return bits
}

func (b *Builder) timestampLine(ctx context.Context, in Inputs, timeless bool) string {
	now := b.cfg.Now(ctx)
	bits := b.zoneBits(ctx, now)
	suffix := ""
	if len(bits) > 0 {
		suffix = " (" + strings.Join(bits, ", ") + ")"
	}
	start := sessionStartLike(in, now)
	line := "Conversation started: " + start.Format(promptDateLayout) + suffix
	if now.Format(dayStampForm) != start.Format(dayStampForm) {
		line += "\nToday's date (as of the last context rebuild): " + now.Format(promptDateLayout) + " " +
			"— trust this over the start date for what day it is now; query tools for exact time."
	}
	if timeless {
		line = ""
		if len(bits) > 0 {
			line = "Timezone: " + strings.Join(bits, ", ")
		}
	}
	sessionID := ""
	if in.PassSessionID {
		sessionID = in.SessionID
	}
	for _, f := range [][2]string{{"Session ID", sessionID}, {"Model", in.Model}, {"Provider", in.Provider}, {"Platform", in.Platform}} {
		if f[1] != "" {
			line += "\n" + f[0] + ": " + f[1]
		}
	}
	return line
}

func memoryParts(m MemoryBlocks) []string {
	var parts []string
	if m.MemoryEnabled && m.Memory != "" {
		parts = append(parts, m.Memory)
	}
	if m.ProfileEnabled && m.User != "" {
		parts = append(parts, m.User)
	}
	if m.External != "" {
		parts = append(parts, m.External)
	}
	return parts
}

func alibabaIdentity(in Inputs) []string {
	if in.Provider != alibabaProvider {
		return nil
	}
	short := in.Model
	if i := strings.LastIndex(short, "/"); i >= 0 {
		short = short[i+1:]
	}
	return []string{"You are powered by the model named " + short + ". " +
		"The exact model ID is " + in.Model + ". " +
		"When asked what model you are, always answer based on this information, " +
		"not on any model name returned by the API."}
}

func (b *Builder) Reconstruct(ctx context.Context, in Inputs, stored, label string) (Prompt, bool) {
	p, err := b.Build(ctx, in)
	if err != nil {
		slog.Debug("static system-prefix reconstruction failed on "+label, "error", err)
		return Prompt{}, false
	}
	return p, p.Static != "" && strings.HasPrefix(stored, p.Static)
}

func (b *Builder) Build(ctx context.Context, in Inputs) (Prompt, error) {
	if in.Config == nil {
		return Prompt{}, &tool.UnfilledError{Name: "prompt.Inputs.Config"}
	}
	var warnings []string
	t := truncation{maxChars: b.maxChars(ctx, in.ContextLength), warnings: &warnings}

	soul := ""
	if in.LoadSoulIdentity || !in.SkipContextFiles {
		var err error
		if soul, err = b.loadSoul(ctx, t); err != nil {
			return Prompt{}, err
		}
	}
	soulLoaded := soul != ""
	stable := []string{defaultIdentity}
	if soulLoaded {
		stable[0] = soul
	}
	helpSlot := len(stable)
	stable = append(stable, helpGuidanceNoSkills)
	stable = append(stable, guidanceParts(in)...)
	skillsPrompt := ""
	if skillTools(in.ToolNames) {
		skillsPrompt = in.Skills.Text
	}
	if has(in.ToolNames, "skill_view") && strings.Contains(skillsPrompt, helpSkillRow) {
		stable[helpSlot] = helpGuidance
	}
	stable = append(stable, alibabaIdentity(in)...)
	if !in.SkipContextFiles && skillTools(in.ToolNames) && in.AutoLoad != "" {
		stable = append(stable, in.AutoLoad)
	}
	environmentHints, err := b.environmentHints(ctx)
	if err != nil {
		return Prompt{}, err
	}
	coding, pin, err := b.codingParts(ctx, in)
	if err != nil {
		return Prompt{}, err
	}
	stable = append(stable, coding.prefix...)

	var post []string
	if in.Config.Agent.EnvironmentProbe {
		post = append(post, b.env.ProbeLine(ctx))
	}
	timeless := in.Timeless
	if in.Config.Agent.BotModeProtocol && joinTier(in.BotMode) != "" {
		post = append(post, in.BotMode...)
		timeless = true
	}
	platformHint, err := b.PlatformHint(ctx, in)
	if err != nil {
		return Prompt{}, err
	}
	post = append(post, platformHint)

	contextParts := []string{in.System}
	if !in.SkipContextFiles {
		q, err := b.agentContextQuery(ctx, in, soulLoaded)
		if err != nil {
			return Prompt{}, err
		}
		files, err := b.contextFiles(ctx, q, t)
		if err != nil {
			return Prompt{}, err
		}
		contextParts = append(contextParts, files)
	}
	if len(coding.workspace) > 0 {
		contextParts = append(contextParts, coding.workspace...)
		contextParts = append(contextParts, coding.trailing...)
		contextParts = append(contextParts, post...)
	} else {
		stable = append(stable, coding.trailing...)
		stable = append(stable, post...)
	}

	volatile := []string{skillsPrompt}
	volatile = append(volatile, memoryParts(in.Memory)...)
	volatile = append(volatile, sectionBlock(in.PluginSections, SectionAfterMemory))
	profileLine, err := b.activeProfileLine(ctx)
	if err != nil {
		return Prompt{}, err
	}
	volatile = append(volatile, profileLine, b.timestampLine(ctx, in, timeless))
	if environmentHints != "" {
		environmentHints = strings.ReplaceAll(environmentHints, runtimeEnvironmentHeading, "> "+runtimeEnvironmentHeading)
		volatile = append(volatile, runtimeEnvironmentHeading+"\n\n"+environmentHints+"\n\n"+runtimeEnvironmentEnd)
	}

	tiers := []Tier{{TierStable, joinTier(stable)}, {TierContext, joinTier(contextParts)}, {TierVolatile, joinTier(volatile)}}
	return Prompt{
		Text:      joinNonEmpty([]string{tiers[0].Text, tiers[1].Text, tiers[2].Text}, "\n\n"),
		Static:    tiers[0].Text,
		Tiers:     tiers,
		Warnings:  warnings,
		Workspace: pin,
		Timeless:  timeless,
	}, nil
}
