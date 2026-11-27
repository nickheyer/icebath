package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
)

// How the app is launched from the command line: on a new chat, or on a session to resume or continue
type launch struct {
	resume     string
	continuing bool
	name       string
	keepCwd    bool
}

// Reads a launch of the app from the arguments: none, -r/--resume SESSION, -c/--continue [NAME], with --no-restore-cwd; false for any
// other command line
func parseLaunch(args []string) (launch, bool, error) {
	var l launch
	for i := 0; i < len(args); i++ {
		arg := args[i]
		flag, value, inline := strings.Cut(arg, "=")
		switch flag {
		case "-r", "--resume":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return l, true, errors.New("--resume needs the id or title of a session")
				}
				i++
				value = args[i]
			}
			l.resume = strings.TrimSpace(value)
		case "-c", "--continue":
			l.continuing = true
			if inline {
				l.name = strings.TrimSpace(value)
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				l.name = strings.TrimSpace(args[i])
			}
		case "--no-restore-cwd":
			l.keepCwd = true
		default:
			return l, false, nil
		}
	}
	if l.resume != "" && l.continuing {
		return l, true, errors.New("--resume and --continue can't be used together")
	}
	return l, true, nil
}

// Opens the app for the command line when it is a launch of it: a new chat, or the session it resumes or continues, saying on the way out
// how to come back to the session shown last. False for any other command line
func Launch(ctx context.Context, conn *connect.Client, dir string, args []string, app App) (bool, error) {
	l, ok, err := parseLaunch(args)
	if !ok || err != nil {
		return ok, err
	}
	service := sessionsv1connect.NewSessionServiceClient(conn)
	out := os.Stdout
	started := time.Now()
	id := ""
	switch {
	case l.resume != "":
		response, err := service.Resolve(ctx, &sessionsv1.ResolveRequest{Ref: l.resume, Tip: true})
		if err != nil {
			return true, fmt.Errorf("No session found matching '%s'. Use 'icebath sessions list' to see available sessions.", l.resume)
		}
		id = response.GetSession().GetId()
		restore(out, response.GetSession(), l.keepCwd)
	case l.continuing:
		session, err := continued(ctx, conn, service, dir, l.name)
		if err != nil {
			return true, err
		}
		id = session.GetId()
		restore(out, session, l.keepCwd)
	}
	return true, openApp(ctx, conn, app, id, started)
}

// Runs the app on the session, a new chat when none, then says how to come back to the one it showed last
func openApp(ctx context.Context, conn *connect.Client, app App, id string, started time.Time) error {
	if id != "" {
		ctx = sessions.WithResume(ctx, id)
	}
	ctx, shown := sessions.WithShown(ctx)
	if err := app(ctx, conn); err != nil {
		return err
	}
	goodbye(context.WithoutCancel(ctx), os.Stdout, sessionsv1connect.NewSessionServiceClient(conn), shown.ID(), started)
	return nil
}

// Session -c continues: the one titled with the name, else the one this terminal showed last, else the latest one made here or anywhere
func continued(ctx context.Context, conn *connect.Client, service sessionsv1connect.SessionServiceClient, dir, name string) (*sessionsv1.Session, error) {
	if name != "" {
		response, err := service.Resolve(ctx, &sessionsv1.ResolveRequest{Ref: name, Tip: true})
		if err != nil {
			return nil, fmt.Errorf("No session found matching '%s'. Use 'icebath sessions list' to see available sessions.", name)
		}
		return response.GetSession(), nil
	}
	settings := configv1connect.NewConfigServiceClient(conn)
	on := true
	if setting, err := settings.Get(ctx, &configv1.GetRequest{Key: sessionsconfig.TerminalContinue}); err == nil {
		yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &on)
	}
	if crumb, ok := sessions.Remembered(dir); on && ok {
		if response, err := service.Resolve(ctx, &sessionsv1.ResolveRequest{Ref: crumb.SessionID, Tip: true}); err == nil {
			return response.GetSession(), nil
		}
	}
	cwd, _ := os.Getwd()
	for _, directory := range []string{cwd, ""} {
		listed, err := service.List(ctx, &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{Limit: 1, Directory: directory,
			ExcludeSources: []string{"tool", "cron", "subagent", "kanban"}}})
		if err != nil {
			return nil, err
		}
		if found := listed.GetSessions(); len(found) > 0 {
			response, err := service.Resolve(ctx, &sessionsv1.ResolveRequest{Ref: found[0].GetId(), Tip: true})
			if err != nil {
				return found[0], nil
			}
			return response.GetSession(), nil
		}
	}
	return nil, errors.New("No previous TUI session to continue. Start a new one with `icebath`, or list sessions with `icebath sessions list`.")
}

// Moves into the session's working directory, as resuming it puts the person back where it ran, unless asked not to
func restore(out io.Writer, session *sessionsv1.Session, keep bool) {
	directory := session.GetDirectory()
	current, _ := os.Getwd()
	if keep || directory == "" || filepath.Clean(directory) == filepath.Clean(current) {
		return
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		fmt.Fprintf(out, "⚠ Session's working directory is gone: %s — staying in %s\n", directory, current)
		return
	}
	if err := os.Chdir(directory); err != nil {
		fmt.Fprintf(out, "⚠ Could not enter session's working directory %s: %v\n", directory, err)
		return
	}
	fmt.Fprintf(out, "↻ Working directory: %s\n", directory)
}

// Says how to come back to the session the app showed last
func goodbye(ctx context.Context, out io.Writer, service sessionsv1connect.SessionServiceClient, id string, started time.Time) {
	if id == "" {
		fmt.Fprintln(out, "Goodbye! 󰆧")
		return
	}
	response, err := service.Get(ctx, &sessionsv1.GetRequest{Id: id})
	if err != nil || len(response.GetMessages()) == 0 {
		fmt.Fprintln(out, "Goodbye! 󰆧")
		return
	}
	session := response.GetSession()
	users, calls := 0, 0
	for _, m := range response.GetMessages() {
		if m.GetRole() == sessionsv1.Role_ROLE_USER {
			users++
		}
		calls += len(m.GetToolCalls())
	}
	fmt.Fprintln(out, "Resume this session with:")
	fmt.Fprintf(out, "  icebath --resume %s\n", id)
	if title := session.GetTitle(); title != "" {
		fmt.Fprintf(out, "  icebath -c %q\n", title)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%-16s%s\n", "Session:", id)
	if title := session.GetTitle(); title != "" {
		fmt.Fprintf(out, "%-16s%s\n", "Title:", title)
	}
	elapsed := time.Since(started).Round(time.Second)
	fmt.Fprintf(out, "%-16s%dh %dm %ds\n", "Duration:", int(elapsed.Hours()), int(elapsed.Minutes())%60, int(elapsed.Seconds())%60)
	fmt.Fprintf(out, "%-16s%d (%d user, %d tool calls)\n", "Messages:", len(response.GetMessages()), users, calls)
}
