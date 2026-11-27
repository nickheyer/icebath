package approvals

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
)

// Channel that shows approval prompts to the user in place of the chat, such as an app on their phone
type Transport struct {
	// Value of approvals.transport that picks it
	Name  string
	Title string
	About string
	// Shows the request and gives the user's answer, which must be bound to the request and be one of its choices
	Present func(ctx context.Context, request Request) (Decision, error)
}

type Registrant interface {
	Transports() []Transport
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Approvals
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Transports() []Transport {
	var transports []Transport
	for _, registrant := range r.registrants {
		transports = append(transports, registrant.Transports()...)
	}
	return transports
}

// Host the registry is nested in, through which plugins reach the approvals and the command guards
func (r *Registry) Host() *Approvals {
	return r.host
}

// What decides whether the agent's risky commands and actions go ahead: the rules, the modes, the answers remembered for each session and
// for good, and the prompts waiting for the user
type Approvals struct {
	Registry
	config *config.Config
	// Every session runs without asking, as ICEBATH_YOLO_MODE said when icebath started, so nothing run later can turn it on
	launch bool
	// This process serves sessions as icebath server, where commands that would stop it are refused
	serving atomic.Bool
	// Transports presenting a prompt now, at most so many
	slots chan struct{}

	mu       sync.Mutex
	sessions map[string]*session
	// Smart approval denials in a row by session, the least recent dropped past the cap
	denials map[string]int
	order   []string
	// Writes of the allowlist, one at a time
	allow sync.Mutex
	// Told of every change of the prompts of any session, by the key Observe gave them
	observers map[int]func(session string, prompts []Prompt)
	observed  int
	// Told when yolo turns on or off for a session, such as a driver whose permissions follow it
	yoloed []func(session string)
	// Hook points that hear of each approval asked and answered and of each wait for the user
	hooks *hooks.Hooks
}

// What one session's approvals keep
type session struct {
	// Keys allowed for the rest of the session
	approved map[string]bool
	yolo     bool
	// Prompts waiting for an answer, oldest first
	queue []*prompt
	// Tells the windows of the session that its prompts changed, nil while nothing listens
	notify func()
}

// Sessions whose denials are counted at once
const maxDenialSessions = 256

// Transports presenting prompts at once
const maxTransports = 8

func New(cfg *config.Config, helpers *auxiliary.Auxiliary, points *hooks.Hooks) *Approvals {
	a := &Approvals{
		config:   cfg,
		launch:   config.Yes(os.Getenv("ICEBATH_YOLO_MODE")),
		slots:    make(chan struct{}, maxTransports),
		sessions: map[string]*session{},
		denials:  map[string]int{},
		hooks:    points,
	}
	approvalsconfig.Register(cfg, func() []config.Option {
		var options []config.Option
		for _, transport := range a.Transports() {
			options = append(options, config.Option{Value: transport.Name, Title: transport.Title, About: transport.About})
		}
		return options
	})
	helpers.Register(tasks{})
	return a
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (a *Approvals) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = a
	}
	a.Registry.Register(registrant)
}

// Marks this process as the server that hosts sessions, where commands that would stop it are refused
func (a *Approvals) Serve() {
	a.serving.Store(true)
}

// Helper task the smart reviewer runs on
type tasks struct{}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: approvalsconfig.Task, Title: "Smart approvals", About: "Model that checks flagged commands when approvals are smart",
		Timeout: 30, Critical: true}}
}

// State of the session, made on first use; a.mu held
func (a *Approvals) session(id string) *session {
	s := a.sessions[id]
	if s == nil {
		s = &session{approved: map[string]bool{}}
		a.sessions[id] = s
	}
	return s
}

// Whether the session runs without asking, from the launch or for the session
func (a *Approvals) skips(id string) bool {
	return a.launch || a.Yolo(id)
}

// Whether yolo was turned on for the session
func (a *Approvals) Yolo(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id] != nil && a.sessions[id].yolo
}

// Whether every session runs without asking since icebath started with yolo on
func (a *Approvals) Launched() bool {
	return a.launch
}

// Turns asking off or on for the session, telling what follows it when that changed it
func (a *Approvals) SetYolo(id string, on bool) {
	a.mu.Lock()
	s := a.session(id)
	changed := s.yolo != on
	s.yolo = on
	hooks := a.yoloed
	a.mu.Unlock()
	if changed {
		for _, hook := range hooks {
			hook(id)
		}
	}
}

// Has fn told of each session whose yolo turns on or off, such as a driver that runs with wider permissions under it and must be made
// again
func (a *Approvals) OnYolo(fn func(session string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.yoloed = append(a.yoloed, fn)
}

// Turns asking off for a session whose record kept it off, true when this call did; a launch in yolo leaves it alone
func (a *Approvals) Restore(id string, persisted bool) bool {
	if !persisted || a.launch {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.session(id)
	if s.yolo {
		return false
	}
	s.yolo = true
	return true
}

// Mode the profile's approvals run in: manual, smart or off; a YAML false reads as off, anything unknown as manual
func Mode(cfg *config.Config) string {
	raw := cfg.GetString(approvalsconfig.Mode)
	switch mode := strings.ToLower(strings.TrimSpace(raw)); mode {
	case approvalsconfig.Manual, approvalsconfig.Smart, approvalsconfig.Off:
		return mode
	case "false":
		return approvalsconfig.Off
	case "", "true":
	default:
		slog.Warn(fmt.Sprintf("Unknown approvals.mode %q — defaulting to 'manual'. Valid values: manual, smart, off", raw))
	}
	return approvalsconfig.Manual
}

// Whether slash commands that delete or reset things ask first; ICEBATH_TUI_NO_CONFIRM set to true says they don't, whatever the setting
func Confirms(cfg *config.Config) bool {
	if value, set := cfg.LookupEnv(approvalsconfig.NoConfirmEnv); set {
		return !config.Yes(value)
	}
	return cfg.GetBool(approvalsconfig.DestructiveConfirm)
}

// Whether the key is allowed for the session or always
func (a *Approvals) approved(cfg *config.Config, id, key string) bool {
	if slices.Contains(cfg.GetStringSlice(approvalsconfig.Allowlist), key) {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id] != nil && a.sessions[id].approved[key]
}

// Records an answer that lasts: session allows the keys for the rest of the session, always also adds them to the allowlist
func (a *Approvals) remember(cfg *config.Config, id, choice string, keys []string) error {
	if choice != Session && choice != Always {
		return nil
	}
	a.mu.Lock()
	s := a.session(id)
	for _, key := range keys {
		s.approved[key] = true
	}
	a.mu.Unlock()
	if choice != Always {
		return nil
	}
	_, _, err := a.Allow(cfg, keys)
	return err
}

// Adds the entries to the profile's allowlist, giving those that were new and how many it holds now
func (a *Approvals) Allow(cfg *config.Config, entries []string) ([]string, int, error) {
	a.allow.Lock()
	defer a.allow.Unlock()
	list := cfg.GetStringSlice(approvalsconfig.Allowlist)
	var added []string
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" && !slices.Contains(list, entry) {
			list = append(list, entry)
			added = append(added, entry)
		}
	}
	if len(added) == 0 {
		return nil, len(list), nil
	}
	slices.Sort(list)
	if err := cfg.Set(approvalsconfig.Allowlist, list); err != nil {
		return nil, 0, err
	}
	return added, len(list), nil
}

// Takes entries out of the profile's allowlist
func (a *Approvals) Disallow(cfg *config.Config, entries []string) error {
	a.allow.Lock()
	defer a.allow.Unlock()
	list := slices.DeleteFunc(cfg.GetStringSlice(approvalsconfig.Allowlist), func(entry string) bool { return slices.Contains(entries, entry) })
	return cfg.Set(approvalsconfig.Allowlist, list)
}

// Counts one more smart denial in a row for the session, giving the count
func (a *Approvals) deny(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.denials[id]++
	a.order = append(slices.DeleteFunc(a.order, func(key string) bool { return key == id }), id)
	for len(a.denials) > maxDenialSessions {
		delete(a.denials, a.order[0])
		a.order = a.order[1:]
	}
	return a.denials[id]
}

// Clears the session's denials after something was approved
func (a *Approvals) reset(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.denials, id)
	a.order = slices.DeleteFunc(a.order, func(key string) bool { return key == id })
}

// Hard stop the agent reads once the session's denials reached the limit, empty before
func (a *Approvals) breaker(cfg *config.Config, id string) string {
	a.mu.Lock()
	count := a.denials[id]
	a.mu.Unlock()
	limit := cfg.GetInt(approvalsconfig.DenialBreaker)
	if limit <= 0 || count < limit {
		return ""
	}
	slog.Warn("Smart-approval circuit breaker tripped", "session", id, "denials", count, "threshold", limit)
	return " CIRCUIT BREAKER: " + strconv.Itoa(count) + " consecutive commands were blocked by the security reviewer. STOP attempting variations of this " +
		"operation. Report the blocked operation to the user and either ask them to run it manually or use /approve."
}

// Seconds a prompt waits, clamped to what every platform can wait
func timeout(cfg *config.Config) time.Duration {
	seconds := cfg.GetInt(approvalsconfig.Timeout)
	if seconds > maxWaitSeconds {
		slog.Warn(fmt.Sprintf("the approval wait of %ds exceeds the platform-safe maximum; clamping to %ds", seconds, maxWaitSeconds))
	}
	return time.Duration(min(max(seconds, 0), maxWaitSeconds)) * time.Second
}

// Longest wait every platform can express: a DWORD of milliseconds on Windows, a year elsewhere
var maxWaitSeconds = func() int {
	if runtime.GOOS == "windows" {
		return (0xFFFFFFFF - 1000) / 1000
	}
	return 31536000
}()

// Wait spelled in the largest unit it is a whole number of
func window(d time.Duration) string {
	seconds := max(int(d/time.Second), 0)
	count, unit := seconds, "second"
	switch {
	case seconds != 0 && seconds%3600 == 0:
		count, unit = seconds/3600, "hour"
	case seconds != 0 && seconds%60 == 0:
		count, unit = seconds/60, "minute"
	}
	if count != 1 {
		unit += "s"
	}
	return strconv.Itoa(count) + " " + unit
}
