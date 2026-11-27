package launch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/launch"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
)

// Notices of a reply's end the agent's server sends and the command line reads: a reply stopped, and the mark before why a reply failed,
// which no other notice of a reply starts with
const (
	Stopped = "The reply was stopped."
	Failed  = "✗ "
)

// What the launch of the app asked of the sessions its windows start or resume: the flags given, by their first spelling; and the sessions
// the launch sent messages to, which end as their hooks know it once the launch does
type Asked struct {
	mu    sync.Mutex
	given launch.Given
	held  []string
}

// Records a session the launch sent a message to
func (a *Asked) Hold(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Contains(a.held, id) {
		a.held = append(a.held, id)
	}
}

// Ends the sessions the launch held as their hooks know it, printing what the hooks have to tell the person
func (a *Asked) finalize(ctx context.Context, conn *connect.Client) {
	a.mu.Lock()
	held := a.held
	a.held = nil
	a.mu.Unlock()
	if len(held) == 0 {
		return
	}
	response, err := agentv1connect.NewAgentServiceClient(conn).Finalize(context.WithoutCancel(ctx), &agentv1.FinalizeRequest{SessionIds: held,
		Reason: "shutdown"})
	if err != nil {
		slog.Debug("Could not end the sessions of the launch as their hooks know it", "error", err)
		return
	}
	for _, message := range response.GetMessages() {
		fmt.Println(message)
	}
}

type flags struct {
	asked *Asked
}

// Registers the flags; what they asked, which the app sends with the first message of each session
func Register(registry *launch.Registry) *Asked {
	asked := &Asked{}
	registry.Register(flags{asked: asked})
	return asked
}

func (flags) Flags() []launch.Flag {
	return []launch.Flag{
		{Names: []string{"--pass-session-id"}, Help: "Put the session id in the agent's system prompt"},
		{Names: []string{"--ignore-user-config"}, Help: "Use the default settings instead of the settings file, keys file still read"},
		{Names: []string{"--ignore-rules"}, Help: "Leave out rule files such as AGENTS.md, memory and preloaded skills"},
		{Names: []string{"--safe-mode"}, Help: "Leave out every customization, to find out whether your setup causes a problem"},
		{Names: []string{"-s", "--skills"}, Value: "names", Help: "Skills to load at the start, repeatable or separated by commas"},
	}
}

func (f flags) Around(ctx context.Context, conn *connect.Client, given launch.Given, run launch.Run) error {
	f.asked.mu.Lock()
	f.asked.given = given
	f.asked.mu.Unlock()
	if given.Has("--safe-mode") {
		// What this process serves itself leaves out every customization, MCP servers it would start included
		os.Setenv("ICEBATH_SAFE_MODE", "1")
		os.Setenv("ICEBATH_IGNORE_RULES", "1")
	}
	err := run(ctx)
	f.asked.finalize(ctx, conn)
	return err
}

// What a session is asked from the launch, its flags or their variables in the environment; nil when nothing was asked
func (a *Asked) Launch() *agentv1.Launch {
	a.mu.Lock()
	given := a.given
	a.mu.Unlock()
	l := &agentv1.Launch{Skills: given.Values("-s")}
	asked := len(l.Skills) > 0
	for _, flag := range []struct {
		on  bool
		set **bool
	}{
		{given.Has("--pass-session-id"), &l.PassSessionId},
		{given.Has("--ignore-rules"), &l.IgnoreRules},
		{given.Has("--ignore-user-config") || config.Yes(os.Getenv("ICEBATH_IGNORE_USER_CONFIG")), &l.IgnoreUserConfig},
		{given.Has("--safe-mode") || config.Yes(os.Getenv("ICEBATH_SAFE_MODE")), &l.SafeMode},
	} {
		if flag.on {
			*flag.set, asked = new(true), true
		}
	}
	if !asked {
		return nil
	}
	return l
}
