package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/cua"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
)

//go:embed schema.json
var schemaJSON []byte

// Schema of the tool, as every function calling model is sent it
var schema = func() map[string]any {
	var s map[string]any
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		panic("computer_use schema: " + err.Error())
	}
	delete(s, "name")
	return s
}()

type registrant struct {
	driver    *cua.Driver
	approvals *approvals.Approvals

	mu sync.Mutex
	// Backend each session drives, the one call at a time it takes, and the permission mode and display it was made for
	held map[string]*held
	// Sessions told that yolo widened the driver's permissions
	warned map[string]bool
	// Last screenshot each session was shown, so an unchanged one is left out
	shots map[string]*shot
}

type held struct {
	backend *cua.Backend
	call    sync.Mutex
	mode    string
	display string
}

// Screenshot a session was shown last, of which window, and how many times in a row it was left out since
type shot struct {
	digest string
	target [2]string
	streak int
}

// Times in a row an unchanged screenshot is left out before it is shown again anyway
const omittedMax = 2

func Register(registry *tools.Registry, stores *sessions.Registry, driver *cua.Driver, checks *approvals.Approvals) {
	r := &registrant{driver: driver, approvals: checks, held: map[string]*held{}, warned: map[string]bool{}, shots: map[string]*shot{}}
	registry.Register(r)
	stores.OnRelease(func(_ context.Context, session string) { r.release(session) })
	checks.OnYolo(r.release)
}

// Kinds of action the tool asks approval for, by action and delivery
func risks() []string {
	var out []string
	for _, action := range append(slices.Sorted(func(yield func(string) bool) {
		for name, spec := range actions {
			if spec.destructive && !yield(name) {
				return
			}
		}
	}), "bring_to_front") {
		out = append(out, "cua:"+action+":background", "cua:"+action+":foreground")
	}
	return out
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:    "computer_use",
		Toolset: "computer_use",
		Title:   "Computer",
		About:   "See a screen and use its mouse and keyboard",
		Group:   "Computer use",
		Subject: "action",
		Risks:   risks(),
		Schema:  schema,
		Handler: r.handle,
		Check:   r.available,
		Lacks:   r.lacks,
		Effect:  tools.MayChange,
		// Desktop control has nothing to do with code
		Unfocused: true,
		Fallible:  true,
	}}
}

// Whether the profile's provider can run here: the driver on a desktop platform, installed, or the screen in the terminal backend's sandbox;
// any provider icebath has none of is offered so the call can say why
func (r *registrant) available(env *tools.Env) bool {
	if cua.Unknown(env.Config) != nil {
		return true
	}
	if !cua.Supported() {
		return false
	}
	installed, err := r.driver.Installed()
	if err != nil {
		slog.Debug("computer_use: driver lookup failed", "error", err)
	}
	// A screen in the terminal backend's sandbox is driven by the driver inside it, so none is needed here
	return installed || r.driver.Desktop.InSandbox(env.Config)
}

// Permission mode the session's driver runs in: the profile's, unrestricted while its approvals are bypassed, said once a session
func (r *registrant) mode(cfg *config.Config, session string) string {
	configured := cuconfig.Standard
	if strings.ToLower(strings.TrimSpace(cfg.GetString(cuconfig.PermissionMode))) == cuconfig.Bounded {
		configured = cuconfig.Bounded
	}
	if !r.approvals.Launched() && !r.approvals.Yolo(session) {
		return configured
	}
	r.mu.Lock()
	warn := !r.warned[session]
	r.warned[session] = true
	r.mu.Unlock()
	if warn {
		slog.Warn(fmt.Sprintf("computer_use: approval bypass (--yolo / -z) escalated the cua-driver permission mode from the configured '%s' to "+
			"'unrestricted' for this session. Runtime approval prompts are disabled and the driver's residual ceilings no longer apply. Drop the "+
			"bypass flag to keep '%s', or declare a version-3 computer_use.capability_manifest to keep a ceiling on bypassed runs.", configured, configured))
	}
	return cuconfig.Unrestricted
}

// The session's backend with its call held, made when there is none and made again when the permission mode or the display changed
func (r *registrant) backend(ctx context.Context, cfg *config.Config, session string) (*held, error) {
	for {
		mode, display := r.mode(cfg, session), r.driver.Display(cfg)
		r.mu.Lock()
		h := r.held[session]
		if h == nil {
			backend, err := r.driver.Backend(cfg, mode)
			if err == nil {
				err = backend.Start(ctx)
			}
			if err != nil {
				r.mu.Unlock()
				return nil, err
			}
			h = &held{backend: backend, mode: mode, display: display}
			r.held[session] = h
			r.mu.Unlock()
		} else if h.mode != mode || h.display != display {
			delete(r.held, session)
			r.mu.Unlock()
			stop(h, "computer_use backend replacement failed for session "+session)
			continue
		} else {
			r.mu.Unlock()
		}
		h.call.Lock()
		r.mu.Lock()
		current := r.held[session] == h && h.mode == r.mode(cfg, session) && h.display == r.driver.Display(cfg)
		r.mu.Unlock()
		if current {
			return h, nil
		}
		h.call.Unlock()
	}
}

// Stops a backend once its call in flight finished, a failure only logged
func stop(h *held, what string) {
	h.call.Lock()
	defer h.call.Unlock()
	if err := h.backend.Stop(); err != nil {
		slog.Debug(fmt.Sprintf("%s: %v", what, err))
	}
}

// Lets the session's backend go, as the session ends or its yolo turns, so the next call makes one in the mode that holds then
func (r *registrant) release(session string) {
	r.mu.Lock()
	h := r.held[session]
	delete(r.held, session)
	delete(r.shots, session)
	if !r.approvals.Yolo(session) {
		delete(r.warned, session)
	}
	r.mu.Unlock()
	if h != nil {
		stop(h, "computer_use backend release failed for session "+session)
	}
}

// Stops every backend so no driver outlives icebath
func (r *registrant) Close() error {
	r.mu.Lock()
	all := r.held
	r.held, r.warned, r.shots = map[string]*held{}, map[string]bool{}, map[string]*shot{}
	r.mu.Unlock()
	for _, h := range all {
		stop(h, "cua-driver teardown at exit failed")
	}
	return nil
}

// Forgets the session's last screenshot once its context was compacted, so the next capture shows the screen again
func (r *registrant) Compacted(_ *config.Config, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.shots, session)
}

// Whether the frame repeats the session's last one of the same window, within the streak it may be left out for; any other frame kept as
// the last
func (r *registrant) unchanged(session, digest string, target [2]string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.shots[session]; s != nil && s.digest == digest && s.target == target && s.streak < omittedMax {
		s.streak++
		return true
	}
	r.shots[session] = &shot{digest: digest, target: target}
	return false
}

// What computer use lacks to run: a computer it can drive, or the program that drives it
func (r *registrant) lacks(*tools.Env) string {
	if !cua.Supported() {
		return "icebath can't drive the screen of this computer."
	}
	return "It needs the program that drives the screen, which is not installed."
}

// The group of computer use, whose options are the computer use section
func (r *registrant) Groups() []tools.Group {
	return []tools.Group{{Title: "Computer use", Sections: []string{cuconfig.Key}, Order: 70}}
}
