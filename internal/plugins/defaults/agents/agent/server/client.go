package server

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Key of the model settings of a session holding the shell of its client, as the session froze it
const clientKey = "_client"

// Source of the sessions a one-shot run of the command line makes, whose tools wait longer and which wait for their background work
// before they end
const oneshotSource = "oneshot"

// Rounds of background work a one-shot session answers before it ends
const lingerRounds = 8

// Told the session when its client's toolchain and host paths can't apply, the client being on another machine
const remoteClientNote = "⚠ This session uses the PATH, programs and example messages file of the computer icebath runs on, not those of " +
	"your shell. Your shell's are used only when you connect from that computer."

// Shell of the session's client as it was stored, nil when it has none
func storedClient(session *sessionsv1.Session) *tools.Client {
	var recorded struct {
		Client *tools.Client `json:"_client"`
	}
	if session.GetModelConfig() == "" || json.Unmarshal([]byte(session.GetModelConfig()), &recorded) != nil {
		return nil
	}
	return recorded.Client
}

// Shell of the session's client
func (r *run) shell() *tools.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

// Freezes the shell of the client with the session as the first message it is sent comes with it: its variables kept by the rules of a
// client on this machine when the connection and the machine's name prove it is one, else by those of a remote one. A session that
// already holds messages or a client keeps what it has
func (a *Agent) freeze(ctx context.Context, id string, variables map[string]string, host string) error {
	if len(variables) == 0 {
		return nil
	}
	r, err := a.run(id)
	if err != nil {
		return err
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	if session.GetMessages() > 0 || r.shell() != nil {
		return nil
	}
	local := onThisMachine(ctx, host)
	client, err := tools.FreezeClient(variables, local, a.settings(r).GetStringSlice(terminalconfig.ClientEnvPassthrough))
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := store.Update(ctx, id, sessions.Change{ModelConfig: map[string]any{clientKey: client}}); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return nil
	}
	r.client = client
	if !local {
		a.notify(r, remoteClientNote)
	} else if note := projectPlugins(client, session.GetDirectory()); note != "" {
		a.notify(r, note)
	}
	return nil
}

// Whether the client runs on this machine: in this process, or connected from this machine's loopback and naming this machine
func onThisMachine(ctx context.Context, host string) bool {
	info, ok := connect.CallInfoForServerContext(ctx)
	if !ok || info.PeerAddr == "" {
		return true
	}
	address, _, err := net.SplitHostPort(info.PeerAddr)
	if err != nil {
		address = info.PeerAddr
	}
	if ip := net.ParseIP(address); ip == nil || !ip.IsLoopback() {
		return false
	}
	name, err := os.Hostname()
	return err == nil && host != "" && strings.EqualFold(host, name)
}

// Notice for a session whose client asked for the project's own plugins in a folder that has some, which the server never loads; empty
// otherwise
func projectPlugins(client *tools.Client, directory string) string {
	value, _ := client.Lookup("ICEBATH_ENABLE_PROJECT_PLUGINS")
	if !config.Yes(value) {
		return ""
	}
	folder := filepath.Join(directory, ".icebath", "plugins")
	if entries, err := os.ReadDir(folder); err != nil || len(entries) == 0 {
		return ""
	}
	return "⚠ The plugins in " + folder + " are not loaded, because icebath never loads plugins from a project folder. " +
		"ICEBATH_ENABLE_PROJECT_PLUGINS has no effect here."
}

// Wait of a one-shot session for its background work at its end: one budget for every round, the rounds taken, and whether it is over
type lingering struct {
	deadline time.Time
	rounds   int
	done     bool
}

// Whether the session waits for its background work once more before it ends
func (w *lingering) due(r *run) bool {
	return r.oneshot && !w.done
}

// Waits for the background work of the one-shot session that tells it when it ends, within what is left of its budget; whether a notice of
// it arrived for the session to answer. The wait is over once some work outlived the budget, nothing arrived, or the rounds ran out
func (a *Agent) linger(r *run, w *lingering) bool {
	settings := a.settings(r)
	if w.deadline.IsZero() {
		w.deadline = time.Now().Add(time.Duration(max(settings.GetInt(terminalconfig.OneshotWait), 0)) * time.Second)
	}
	w.rounds++
	ctx, stop := context.WithCancel(a.ctx)
	defer stop()
	r.mu.Lock()
	r.stop = stop
	r.mu.Unlock()
	_, running := a.tools.Linger(ctx, &tools.Env{Config: settings, OneShot: true, Client: r.shell()}, r.sessionID(), w.deadline)
	r.mu.Lock()
	delivered := len(r.queue) > 0
	r.mu.Unlock()
	w.done = running || !delivered || w.rounds >= lingerRounds || ctx.Err() != nil
	return delivered
}
