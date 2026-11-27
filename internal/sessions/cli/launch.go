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
	"github.com/nickheyer/icebath/pkg/strs"
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
					return l, true, errors.New("--resume needs the id or title of a session, such as icebath -r 3f2a9c.")
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
		return l, true, errors.New("--resume and --continue can't go together. Use one of them.")
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
			return true, fmt.Errorf("No session matches “%s”. Run icebath sessions list to see your sessions.", l.resume)
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
			return nil, fmt.Errorf("No session matches “%s”. Run icebath sessions list to see your sessions.", name)
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
	return nil, errors.New("There is no session to continue yet. Run icebath to start one, or icebath sessions list to see your sessions.")
}

// Moves into the session's working directory, as resuming it puts the person back where it ran, unless asked not to
func restore(out io.Writer, session *sessionsv1.Session, keep bool) {
	directory := session.GetDirectory()
	current, _ := os.Getwd()
	if keep || directory == "" || filepath.Clean(directory) == filepath.Clean(current) {
		return
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		fmt.Fprintf(out, "⚠ The folder of the session, %s, is gone, so icebath stays in %s.\n", directory, current)
		return
	}
	if err := os.Chdir(directory); err != nil {
		fmt.Fprintf(out, "⚠ icebath could not go into the folder of the session, %s: %v.\n", directory, err)
		return
	}
	fmt.Fprintf(out, "Working in the folder of the session, %s.\n", directory)
}

// Length of time in hours, minutes and seconds, each said when it is not zero: 1 hour, 2 minutes and 5 seconds
func lasted(d time.Duration) string {
	var parts []string
	for _, unit := range []struct {
		length time.Duration
		noun   string
	}{{time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if n := int(d / unit.length); n > 0 {
			parts = append(parts, strs.Count(n, unit.noun))
			d -= time.Duration(n) * unit.length
		}
	}
	switch len(parts) {
	case 0:
		return "Less than a second"
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// Says how to come back to the session the app showed last
func goodbye(ctx context.Context, out io.Writer, service sessionsv1connect.SessionServiceClient, id string, started time.Time) {
	if id == "" {
		fmt.Fprintln(out, "Goodbye.")
		return
	}
	response, err := service.Get(ctx, &sessionsv1.GetRequest{Id: id})
	if err != nil || len(response.GetMessages()) == 0 {
		fmt.Fprintln(out, "Goodbye.")
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
	fmt.Fprintln(out, "To come back to this session, run:")
	fmt.Fprintf(out, "  icebath --resume %s\n", id)
	if title := session.GetTitle(); title != "" {
		fmt.Fprintf(out, "  icebath -c %q\n", title)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%-12s%s\n", "Session:", id)
	if title := session.GetTitle(); title != "" {
		fmt.Fprintf(out, "%-12s%s\n", "Title:", title)
	}
	fmt.Fprintf(out, "%-12s%s\n", "Time:", lasted(time.Since(started).Round(time.Second)))
	fmt.Fprintf(out, "%-12s%s\n", "Messages:", strs.Number(float64(len(response.GetMessages()))))
	fmt.Fprintf(out, "%-12s%s\n", "From you:", strs.Number(float64(users)))
	fmt.Fprintf(out, "%-12s%s\n", "Tool calls:", strs.Number(float64(calls)))
}
