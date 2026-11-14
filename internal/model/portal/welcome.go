package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	ModelSwitchHeader = "x-portal-model-switch"

	RefusalModelNotFree    = "model_not_free"
	RefusalFeatureNotFree  = "feature_not_free"
	RefusalAtCapacity      = "at_capacity"
	RefusalAdmissionClosed = "admission_closed"
	RefusalRateLimited     = "rate_limited"

	RouteAnonOnPaidHost     = "anon_on_paid_host"
	RouteNamedOnWelcomeHost = "named_on_welcome_host"
	RouteTierDisabled       = "tier_disabled"
	RouteSessionExpired     = "session_expired"

	FreeTierOutageCopy = "The free model is having trouble responding right now. " +
		"Try sending your message again in a minute."

	signinChat        = "To sign in: /login."
	signinTerminal    = "To sign in: `icebath auth upgrade`."
	modelHintChat     = "Run /model and pick the Icebath Portal row again."
	modelHintTerminal = "Run `icebath model` and pick the Icebath Portal row again."
)

var (
	welcomeHosts          = []string{"welcome-api.icebath.app"}
	welcomeRefusalReasons = []string{RefusalModelNotFree, RefusalFeatureNotFree, RefusalAtCapacity, RefusalAdmissionClosed, RefusalRateLimited}
	welcomeRouteRefusals  = [][2]string{
		{"anonymous accounts must use", RouteAnonOnPaidHost},
		{"serves anonymous icebath accounts only", RouteNamedOnWelcomeHost},
		{"anonymous accounts are not accepted", RouteTierDisabled},
		{"invalid jwt", RouteSessionExpired},
	}
)

func WelcomeHosts() []string {
	hosts := slices.Clone(welcomeHosts)
	for _, part := range strings.Split(os.Getenv(EnvExtraWelcomeHosts), ",") {
		if host := strings.ToLower(strings.TrimSpace(part)); host != "" && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func RouteIsWelcomeHost(baseURL string) bool {
	return slices.Contains(WelcomeHosts(), hostOf(baseURL))
}

func PinModelForRoute(provider, baseURL, model string) string {
	if provider == Provider && RouteIsWelcomeHost(baseURL) {
		if model != "" && model != GuestModel {
			slog.Info(fmt.Sprintf("Icebath Portal free tier: using %s instead of configured model %s", GuestModel, model))
		}
		return GuestModel
	}
	return model
}

func RouteCanServeModel(provider, baseURL, model string) bool {
	if provider != Provider || !RouteIsWelcomeHost(baseURL) {
		return true
	}
	return model == "" || model == GuestModel
}

func OnFreeModel(provider string, apiKey auth.Secret, baseURL string) bool {
	return RouteIsWelcomeHost(baseURL) && IsAnonymousRequest(provider, apiKey)
}

func WelcomeTierGate(reason string) bool {
	return reason == RefusalModelNotFree || reason == RefusalFeatureNotFree
}

type Refusal struct {
	Reason     string   `json:"reason"`
	RetryAfter int      `json:"retry_after"`
	Alternates []string `json:"alternates"`
	UpgradeURL string   `json:"upgrade_url"`
}

func floatOf(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case json.Number:
		return numeric(t)
	case string:
		return parseFloat(t)
	}
	return 0, false
}

func ParseWelcomeRefusal(body json.RawMessage) (Refusal, bool) {
	payload, ok := parseObject(body)
	if !ok {
		return Refusal{}, false
	}
	reason, isString := payload.get("reason").(string)
	if !isString || !slices.Contains(welcomeRefusalReasons, reason) {
		return Refusal{}, false
	}
	out := Refusal{Reason: reason, Alternates: []string{}}
	if raw := payload.get("retry_after"); present(raw) {
		if f, ok := floatOf(raw); ok {
			out.RetryAfter = max(0, int(math.Trunc(f)))
		}
	}
	for _, item := range payload.list("alternates") {
		if s, ok := item.(string); ok && s != "" {
			out.Alternates = append(out.Alternates, s)
		}
	}
	out.UpgradeURL = payload.str("upgrade_url")
	return out, true
}

func signinDoor(inChat, door bool) string {
	switch {
	case !door:
		return ""
	case inChat:
		return signinChat
	}
	return signinTerminal
}

func WelcomeRefusalCopy(r Refusal, model string, inChat, door bool) string {
	signin := signinDoor(inChat, door)
	serves := GuestModel
	if len(r.Alternates) > 0 {
		serves = r.Alternates[0]
	}
	wait := "a little while"
	if r.RetryAfter > 0 {
		wait = FriendlyWait(time.Duration(r.RetryAfter) * time.Second)
	}
	text := ""
	switch r.Reason {
	case RefusalModelNotFree:
		what := "That model isn't"
		if model != "" {
			what = model + " isn't"
		}
		text = fmt.Sprintf("%s available without signing in, so icebath uses %s instead. Sign in for more models. %s", what, serves, signin)
	case RefusalFeatureNotFree:
		text = "That isn't available without signing in. Sign in to use it, it's free. " + signin
	case RefusalAtCapacity:
		text = fmt.Sprintf("Chatting without signing in is really busy right now. Sign in to skip the queue, it's free, or try again in %s. %s", wait, signin)
	case RefusalAdmissionClosed:
		text = fmt.Sprintf("Chatting without signing in is full right now. Sign in to keep going, it's free, or try again in %s. %s", wait, signin)
	case RefusalRateLimited:
		text = fmt.Sprintf("You've used up the allowance for chatting without signing in. It refreshes in %s. "+
			"Sign in for a bigger allowance, it's free. %s", wait, signin)
	default:
		text = "icebath couldn't send that without signing in. Signing in is free. " + signin
	}
	return strings.TrimRight(text, " \t\r\n")
}

func WelcomeRouteRefusal(status int, message, baseURL string) string {
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return ""
	}
	text := strings.ToLower(message)
	for _, pair := range welcomeRouteRefusals {
		if strings.Contains(text, pair[0]) {
			return pair[1]
		}
	}
	if status == http.StatusForbidden && RouteIsWelcomeHost(baseURL) {
		return RouteTierDisabled
	}
	return ""
}

func WelcomeRouteRefusalCopy(kind string, inChat, door bool) string {
	signin := signinDoor(inChat, door)
	modelHint := modelHintTerminal
	if inChat {
		modelHint = modelHintChat
	}
	text := "icebath couldn't reach the free model on this route."
	switch kind {
	case RouteAnonOnPaidHost:
		text = "This install is set to use a different Icebath Portal server (" + EnvInferenceBaseURL + "). " +
			"Unset it to use the free model, or sign in. " + signin
	case RouteNamedOnWelcomeHost:
		text = "This Icebath Portal account needs to reconnect. " + modelHint
	case RouteTierDisabled:
		text = "Using icebath without signing in is switched off right now. " +
			"Sign in to keep chatting, it's free. " + signin
	case RouteSessionExpired:
		text = "icebath couldn't renew its connection to the free model. " +
			"Send your message again, or sign in to keep chatting, it's free. " + signin
	}
	return strings.TrimRight(text, " \t\r\n")
}

type ModelSwitch struct {
	Requested string
	Backing   string
}

func NoteModelSwitch(requested string, header http.Header) (ModelSwitch, bool) {
	backing := strings.TrimSpace(header.Get(ModelSwitchHeader))
	if backing == "" || backing == requested {
		return ModelSwitch{}, false
	}
	return ModelSwitch{Requested: requested, Backing: backing}, true
}

func (a *Account) ApplyModelSwitch(ctx context.Context, pending ModelSwitch, currentModel, baseURL string) (notice string, applied bool) {
	ctx = a.bind(ctx)
	if pending.Backing == "" || currentModel != pending.Requested {
		return "", false
	}
	slog.Info(fmt.Sprintf("Icebath Portal gateway asked to switch %s -> %s; applied for this session", pending.Requested, pending.Backing))
	cfg, err := a.cfg.LoadReadonly(ctx)
	if err == nil && strings.TrimSpace(cfg.Model.Default.ID) == pending.Requested {
		if _, err = a.update(ctx, ConfigUpdate{Provider: Provider, BaseURL: baseURL, DefaultModel: pending.Backing}); err == nil {
			slog.Info(fmt.Sprintf("Config default model moved %s -> %s", pending.Requested, pending.Backing))
		}
	}
	if err != nil {
		slog.Debug("model switch: config default left as is", "error", err)
	}
	return fmt.Sprintf("Model is now %s (your account's model; %s is the free tier's).", pending.Backing, pending.Requested), true
}
