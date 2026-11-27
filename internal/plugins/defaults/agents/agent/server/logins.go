package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
)

// How long a retry armed for a login's reset waits again when it finds its session busy
const busyRetry = 30 * time.Second

// Retries sessions armed for their logins to reset, by session
type loginWaits struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

// One retry armed, as the file keeps it so a restart arms it again
type armedWait struct {
	Session  string    `json:"session_id"`
	Provider string    `json:"provider"`
	Login    string    `json:"login"`
	Name     string    `json:"name"`
	At       time.Time `json:"at"`
}

// Settings keys of a session's choice of login of a provider and what it does once that login runs out
func loginKey(provider string) string {
	return providerconfig.Key + "." + provider + "." + providerconfig.Account
}
func exhaustedKey(provider string) string {
	return providerconfig.Key + "." + provider + "." + providerconfig.OnExhausted
}

// Name an account of the provider is shown by, its own name when it has no title
func loginName(instance provider.Instance, account string) string {
	return cmp.Or(instance.AccountFields(account).GetString(providerconfig.Title), account)
}

// Instance of the provider the name picks for the session, its current one when empty
func (a *Agent) sessionInstance(r *run, name string) (provider.Instance, error) {
	if name == "" {
		// The provider the session runs on, even while its login refuses the route
		instance, _, err := a.current(r)
		if instance.ID != "" {
			return instance, nil
		}
		return instance, err
	}
	settings := a.settings(r)
	instance, ok := a.provider(settings, name)
	if !ok {
		return instance, fmt.Errorf("Profile %s has no provider named %s.", settings.Profile(), name)
	}
	return instance, nil
}

// Holds the session to one login of a provider, or its rotation, and sets what it does once that login runs out, or drops the session's
// own choice; sends the last turn again on it when asked
func (a *Agent) SetLogin(ctx context.Context, id, name, login, onExhausted string, retry, clear bool) (*agentv1.SetLoginResponse, error) {
	r, err := a.run(id)
	if err != nil {
		return nil, err
	}
	instance, err := a.sessionInstance(r, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	values := map[string]any{}
	switch login = strings.TrimSpace(login); {
	case clear:
		values[loginKey(instance.ID)], values[exhaustedKey(instance.ID)] = nil, nil
	case strings.EqualFold(login, providerconfig.Rotation) || strings.EqualFold(login, "rotate"):
		values[loginKey(instance.ID)] = providerconfig.Rotation
	case login != "":
		found := ""
		for _, account := range instance.Accounts {
			if account == login || strings.EqualFold(loginName(instance, account), login) {
				found = account
			}
		}
		if found == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%s has no login named %s. Add one with /providers.", instance.Label(), login))
		}
		values[loginKey(instance.ID)] = found
	}
	switch onExhausted = strings.ToLower(strings.TrimSpace(onExhausted)); onExhausted {
	case "":
	case providerconfig.Fail, providerconfig.Rotate:
		if !clear {
			values[exhaustedKey(instance.ID)] = onExhausted
		}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "on_exhausted must be fail or rotate.")
	}
	if len(values) > 0 {
		store, err := a.store()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		merged := r.overrides.Merge(values, nil)
		r.mu.Unlock()
		if err := store.Update(ctx, id, merged.Change()); err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.overrides = merged
		r.exhausted = ""
		r.mu.Unlock()
		a.changed(r)
	}
	settings := a.settings(r)
	current, _, _ := a.current(r)
	out := &agentv1.SetLoginResponse{Login: settings.GetString(loginKey(instance.ID)), OnExhausted: settings.GetString(exhaustedKey(instance.ID)),
		Current: current.ID == instance.ID}
	if retry {
		switch err := a.Retry(id); {
		case err == nil:
			out.Retried = true
		case !strings.Contains(err.Error(), "Nothing to retry"):
			out.Warning = err.Error()
		}
	}
	return out, nil
}

// Holds the session to the login of the name of its current provider, as /model --login asks; what the user is told
func (a *Agent) Login(ctx context.Context, id, name, login string) (string, error) {
	response, err := a.SetLogin(ctx, id, name, login, "", false, false)
	if err != nil {
		return "", err
	}
	if response.GetLogin() == providerconfig.Rotation {
		return "This session takes turns between the logins in the provider's rotation from its next message on.", nil
	}
	return "This session uses the login " + response.GetLogin() + " from its next message on.", nil
}

// Tells the session's windows once that the login it is held to ran out, when the turn failed on it: when it resets, why, and the
// provider's other logins ready now
func (a *Agent) exhaustedLogin(r *run) {
	instance, err := a.sessionInstance(r, "")
	if err != nil || !instance.Pinned {
		return
	}
	account := credentials.Of(instance, instance.Account)
	status := credentials.StatusOf(account)
	if status.State != credentials.Exhausted && status.State != credentials.Dead {
		return
	}
	resets := status.Until(false)
	key := fmt.Sprintf("%s:%s:%d", instance.ID, instance.Account, resets.Unix())
	r.mu.Lock()
	told := r.exhausted == key
	r.exhausted = key
	r.mu.Unlock()
	if told {
		return
	}
	event := &agentv1.LoginExhausted{SessionId: r.sessionID(), Provider: instance.ID, Kind: instance.Spec.Name,
		Login: &agentv1.Login{Id: instance.Account, Name: loginName(instance, instance.Account)}, Reason: cmp.Or(status.Reason, status.Message),
		OnExhausted: cmp.Or(instance.Settings.GetString(providerconfig.OnExhausted), providerconfig.Fail)}
	if !resets.IsZero() {
		event.Resets = timestamppb.New(resets)
	}
	for _, other := range instance.Accounts {
		if other == instance.Account {
			continue
		}
		if state, _ := a.credentials.State(credentials.Of(instance, other)); state == credentials.Ready {
			event.Ready = append(event.Ready, &agentv1.Login{Id: other, Name: loginName(instance, other)})
		}
		event.Rotation = event.Rotation || instance.AccountFields(other).GetBool(providerconfig.InRotation)
	}
	when := ""
	if !resets.IsZero() {
		when = " until " + resets.Local().Format("15:04")
	}
	text := fmt.Sprintf("The %s login %s is unavailable%s.", instance.ID, event.Login.Name, when)
	if len(event.Ready) > 0 {
		text += fmt.Sprintf(" Switch with /model --login %s.", event.Ready[0].Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a.notify(r, "⚠ "+text)
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Exhausted{Exhausted: event}})
}

// File of the retries armed for logins to reset, for every profile
func (a *Agent) waitsPath() string {
	return filepath.Join(a.config.Dir(), "login-waits.json")
}

func (a *Agent) storeWaits(edit func(waits []armedWait) []armedWait) {
	path := a.waitsPath()
	release, err := fsx.Lock(context.Background(), path+".lock", 10*time.Second)
	if err != nil {
		slog.Warn("The armed login retries could not be locked", "error", err)
		return
	}
	defer release()
	var waits []armedWait
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &waits)
	}
	waits = edit(waits)
	data, _ := json.Marshal(waits)
	if err := fsx.Write(path, data, 0o600); err != nil {
		slog.Warn("The armed login retries could not be kept", "error", err)
	}
}

// Drops the retry the session armed, its timer and its record; whether one was armed
func (a *Agent) cancelWait(id string) bool {
	a.waits.mu.Lock()
	timer := a.waits.timers[id]
	delete(a.waits.timers, id)
	a.waits.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	a.storeWaits(func(waits []armedWait) []armedWait {
		return slices.DeleteFunc(waits, func(w armedWait) bool { return w.Session == id })
	})
	return timer != nil
}

// Starts the timer of a retry a second past the reset, in place of one the session had
func (a *Agent) arm(w armedWait) {
	delay := max(time.Until(w.At.Add(time.Second)), 0)
	a.waits.mu.Lock()
	defer a.waits.mu.Unlock()
	if a.waits.timers == nil {
		a.waits.timers = map[string]*time.Timer{}
	}
	if prev := a.waits.timers[w.Session]; prev != nil {
		prev.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() { a.fire(w, timer) })
	a.waits.timers[w.Session] = timer
}

// Sends the session's last turn again once its login reset, later again while the session is busy
func (a *Agent) fire(w armedWait, timer *time.Timer) {
	a.waits.mu.Lock()
	if a.waits.timers[w.Session] != timer {
		a.waits.mu.Unlock()
		return
	}
	delete(a.waits.timers, w.Session)
	a.waits.mu.Unlock()
	if a.ctx.Err() != nil {
		return
	}
	if r, err := a.run(w.Session); err == nil {
		r.mu.Lock()
		busy := r.busy
		r.mu.Unlock()
		if busy {
			w.At = time.Now().Add(busyRetry)
			a.arm(w)
			return
		}
		if err := a.Retry(w.Session); err != nil {
			slog.Warn("A login wait could not retry the turn", "session", w.Session, "error", err)
		}
	} else {
		slog.Info("A login wait was dropped: the session is gone", "session", w.Session)
	}
	a.storeWaits(func(waits []armedWait) []armedWait {
		return slices.DeleteFunc(waits, func(kept armedWait) bool { return kept.Session == w.Session })
	})
}

// Sends the session's last turn again once the login it is held to resets, or at once, or drops the retry armed
func (a *Agent) WaitLogin(ctx context.Context, id, name string, now, cancel bool) (*agentv1.WaitLoginResponse, error) {
	r, err := a.run(id)
	if err != nil {
		return nil, err
	}
	out := &agentv1.WaitLoginResponse{State: "idle"}
	if cancel {
		if a.cancelWait(id) {
			out.State = "cancelled"
		}
		return out, nil
	}
	if now {
		a.cancelWait(id)
		if err := a.Retry(id); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		out.State = "retried"
		return out, nil
	}
	instance, err := a.sessionInstance(r, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if !instance.Pinned {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("the session uses no single %s login to wait for", instance.ID))
	}
	out.Login = &agentv1.Login{Id: instance.Account, Name: loginName(instance, instance.Account)}
	_, until := a.credentials.State(credentials.Of(instance, instance.Account))
	if until.IsZero() {
		return out, nil
	}
	w := armedWait{Session: id, Provider: instance.ID, Login: instance.Account, Name: out.Login.Name, At: until}
	a.storeWaits(func(waits []armedWait) []armedWait {
		return append(slices.DeleteFunc(waits, func(kept armedWait) bool { return kept.Session == id }), w)
	})
	a.arm(w)
	out.State, out.RetryAt = "armed", timestamppb.New(until)
	return out, nil
}

// Arms again the retries sessions armed before this process started
func (a *Agent) rearmWaits() {
	data, err := os.ReadFile(a.waitsPath())
	if err != nil {
		return
	}
	var waits []armedWait
	if json.Unmarshal(data, &waits) != nil {
		return
	}
	for _, w := range waits {
		a.arm(w)
	}
}

func (s *service) SetLogin(ctx context.Context, request *agentv1.SetLoginRequest) (*agentv1.SetLoginResponse, error) {
	return s.agent.SetLogin(ctx, request.GetSessionId(), request.GetProvider(), request.GetLogin(), request.GetOnExhausted(), request.GetRetry(),
		request.GetClear())
}

func (s *service) WaitLogin(ctx context.Context, request *agentv1.WaitLoginRequest) (*agentv1.WaitLoginResponse, error) {
	return s.agent.WaitLogin(ctx, request.GetSessionId(), request.GetProvider(), request.GetNow(), request.GetCancel())
}
