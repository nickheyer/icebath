package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Seconds a server being added gets to answer before it is kept as it is
const probeWait = 15

// What adding a server kept on the way, before the server itself is: the values set for it and whether it was signed in to. A server that
// is then not kept takes them with it, so nothing is kept of one that was not added
type made struct {
	values []string
	signed bool
}

// Adds a server in steps: one of the catalog or a server of your own; for one of your own its name, how it connects, then what runs it or
// its address, and only when the server refuses without one its token; for one of the catalog only the values it can't run without.
// Nothing is kept until the last step succeeds, and a server that signs in is kept once you signed in
func (s *servers) add() tea.Cmd {
	s.hub.Busy("Reading the catalog…")
	return tui.Call(s.hub, func() (*mcpv1.CatalogResponse, error) {
		return s.service.Catalog(s.kit.Env.Ctx, &mcpv1.CatalogRequest{Profile: s.profile})
	}, func(response *mcpv1.CatalogResponse) tea.Cmd {
		s.hub.Quiet()
		entries := map[string]*mcpv1.Entry{}
		choices := []tui.Choice{{ID: "", Title: "A server of your own", Detail: "A command on this computer, or an address"}}
		for _, entry := range response.GetEntries() {
			if entry.GetInstalled() || entry.GetProblem() != "" {
				continue
			}
			entries[entry.GetName()] = entry
			choices = append(choices, tui.Choice{ID: entry.GetName(), Title: entry.GetName(), Detail: oneLine(entry.GetDescription())})
		}
		f := s.kit.Flow(s.hub, "Add a server")
		return f.Pick("Which server to add.", choices, "", func(choice tui.Choice) tea.Cmd {
			if choice.ID == "" {
				return s.own(f)
			}
			return s.fromCatalog(f, entries[choice.ID])
		})
	})
}

// Steps of a server of your own: its name, how it connects, then what that way needs
func (s *servers) own(f *tui.Flow) tea.Cmd {
	name := cards.Field{ID: "name", Label: "Name", Help: "A name for the server, such as github. Use lower case letters, digits, - and _.",
		Check: s.free}
	return f.Ask("", []cards.Field{name}, "", func(named map[string]string) tea.Cmd {
		server := strings.TrimSpace(named["name"])
		return f.Pick("How it connects.", ways, "", func(choice tui.Choice) tea.Cmd {
			switch choice.ID {
			case byCommand:
				return f.Ask("What runs the server on this computer.", commandFields(), "Add", func(values map[string]string) tea.Cmd {
					settings := map[string]any{"enabled": true, "command": strings.TrimSpace(values["command"])}
					if args := strings.TrimSpace(values["args"]); args != "" {
						settings["args"] = args
					}
					return s.added(f, server, settings, "", "", made{})
				})
			case byAddress:
				return f.Ask("", []cards.Field{addressField()}, "Add", func(values map[string]string) tea.Cmd {
					return s.reach(f, server, strings.TrimSpace(values["url"]))
				})
			}
			return f.Ask("", []cards.Field{addressField()}, "Sign in", func(values map[string]string) tea.Cmd {
				settings := map[string]any{"enabled": true, "url": strings.TrimSpace(values["url"]), "auth": "oauth"}
				return s.signFirst(f, server, settings, nil, "")
			})
		})
	})
}

// What is wrong with a name typed for a new server, empty when it can be one
func (s *servers) free(typed string) string {
	typed = strings.TrimSpace(typed)
	switch {
	case typed == "":
		return "Give the server a name."
	case !config.Named(typed):
		return "Use only lower case letters, digits, - and _."
	case s.find(typed) != nil:
		return "There is a server called " + typed + " already. Give this one another name."
	}
	return ""
}

// Tries a server reached at an address before it is kept: one that answers is kept, one that refuses without a token asks for its token as
// the last step, and one that can't be reached now is kept as it is, the note saying why
func (s *servers) reach(f *tui.Flow, name, url string) tea.Cmd {
	settings := map[string]any{"enabled": true, "url": url}
	data, _ := json.Marshal(settings)
	s.hub.Busy("Trying " + url + "…")
	return tui.Call(s.hub, func() (*mcpv1.ProbeResponse, error) {
		return s.service.Probe(s.kit.Env.Ctx, &mcpv1.ProbeRequest{Profile: s.profile, Name: name, Settings: string(data), ConnectTimeout: probeWait})
	}, func(r *mcpv1.ProbeResponse) tea.Cmd {
		s.hub.Quiet()
		switch {
		case r.GetOk():
			return s.added(f, name, settings, "", "", made{})
		case r.GetClass() == "auth_required":
			token := cards.Field{ID: "token", Label: "Token", Kind: cards.FieldSecret,
				Help: "The API key or token that the server asks for. It is kept with your MCP values."}
			return f.Ask("The server turned icebath down without a token.", []cards.Field{token}, "Add", func(values map[string]string) tea.Cmd {
				return s.bearer(f, name, settings, strings.TrimSpace(values["token"]))
			})
		}
		return s.added(f, name, settings, "It couldn't be reached yet: "+strs.Full(r.GetError()), "", made{})
	})
}

// Keeps a server's token among the values and the server with the header that sends it
func (s *servers) bearer(f *tui.Flow, name string, settings map[string]any, token string) tea.Cmd {
	return tui.Call(s.hub, func() (*mcpv1.SaveBearerResponse, error) {
		return s.service.SaveBearer(s.kit.Env.Ctx, &mcpv1.SaveBearerRequest{Profile: s.profile, Name: name, Token: token})
	}, func(r *mcpv1.SaveBearerResponse) tea.Cmd {
		var headers map[string]any
		json.Unmarshal([]byte(r.GetHeaders()), &headers)
		settings["headers"] = headers
		return s.added(f, name, settings, "", "", made{values: []string{r.GetKey()}})
	})
}

// Signs in to a server not kept yet, then keeps it once the sign-in let icebath in, setting the values given first; the notice of the
// catalog is said once it is kept
func (s *servers) signFirst(f *tui.Flow, name string, settings map[string]any, values map[string]string, notice string) tea.Cmd {
	data, _ := json.Marshal(settings)
	request := &mcpv1.LoginRequest{Profile: s.profile, Name: name, Settings: string(data)}
	// What a sign-in that did not let icebath in left behind, such as the client it signed up as, goes with the server that is not added
	forget := func(said string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			s.undo(name, made{signed: true})
			return func() tea.Cmd { return s.hub.Fail(said) }
		})
	}
	return signin(s.kit, s.hub, s.service, request, func(event *mcpv1.LoginEvent) tea.Cmd {
		switch {
		case event.GetError() != "":
			return forget("Couldn't sign in to " + name + ", so it was not added. " + strs.Full(event.GetError()))
		case event.GetNoToken():
			return forget("The sign-in to " + name + " ended without letting icebath in, so it was not added. Try again.")
		}
		return s.keepValues(f, name, settings, values, notice, made{signed: true})
	}, func() tea.Cmd {
		// Closed before it ended: the server is not added, and what the sign-in began goes
		return tui.Async(func() func() tea.Cmd {
			s.undo(name, made{signed: true})
			return func() tea.Cmd { return nil }
		})
	})
}

// Steps of a server of the catalog: the values it can't run without, when it lacks any
func (s *servers) fromCatalog(f *tui.Flow, entry *mcpv1.Entry) tea.Cmd {
	var fields []cards.Field
	for _, spec := range entry.GetMissing() {
		if !spec.GetRequired() {
			continue
		}
		kind := cards.FieldText
		if spec.GetSecret() {
			kind = cards.FieldSecret
		}
		help := cmp.Or(strs.Full(spec.GetPrompt()), "The value "+entry.GetName()+" asks for.")
		fields = append(fields, cards.Field{ID: spec.GetName(), Label: spec.GetName(), Kind: kind, Value: spec.GetDefault(), Help: help,
			Check: func(typed string) string {
				if strings.TrimSpace(typed) == "" {
					return "It can't run without this value."
				}
				return ""
			}})
	}
	if len(fields) == 0 {
		return s.install(f, entry, nil)
	}
	return f.Ask(entry.GetName()+" can't run without these values.", fields, "Add", func(values map[string]string) tea.Cmd {
		return s.install(f, entry, values)
	})
}

// Installs a server of the catalog: its repository cloned when it runs from source, then its settings kept with the values given, after a
// sign-in for one that signs in
func (s *servers) install(f *tui.Flow, entry *mcpv1.Entry, values map[string]string) tea.Cmd {
	name := entry.GetName()
	s.hub.Busy("Adding " + name + "…")
	return tui.Call(s.hub, func() (map[string]any, error) {
		ctx := s.kit.Env.Ctx
		dir := ""
		if entry.GetFromSource() {
			cloned, err := s.service.GitInstall(ctx, &mcpv1.GitInstallRequest{Profile: s.profile, Entry: name})
			if err != nil {
				return nil, err
			}
			if cloned.GetError() != "" {
				return nil, fmt.Errorf("Couldn't download %s. %s", name, strs.Full(cloned.GetError()))
			}
			dir = cloned.GetDir()
		}
		installed, err := s.service.InstallConfig(ctx, &mcpv1.InstallConfigRequest{Profile: s.profile, Entry: name, InstallDir: dir, Values: values})
		if err != nil {
			return nil, err
		}
		settings := map[string]any{}
		return settings, json.Unmarshal([]byte(installed.GetSettings()), &settings)
	}, func(settings map[string]any) tea.Cmd {
		s.hub.Quiet()
		secret := map[string]bool{}
		for _, spec := range entry.GetRequires() {
			secret[spec.GetName()] = spec.GetSecret()
		}
		kept := map[string]string{}
		for key, value := range values {
			if value = strings.TrimSpace(value); value != "" && secret[key] {
				kept[key] = value
			}
		}
		notice := strings.TrimSpace(entry.GetPostInstall())
		if entry.GetAuth() == "oauth" && entry.GetProvider() == "" {
			return s.signFirst(f, name, settings, kept, notice)
		}
		return s.keepValues(f, name, settings, kept, notice, made{})
	})
}

// Sets the values a new server reads, then keeps the server; a value that can't be set ends the adding, with what was kept on the way
// taken back
func (s *servers) keepValues(f *tui.Flow, name string, settings map[string]any, values map[string]string, notice string, kept made) tea.Cmd {
	if len(values) == 0 {
		return s.added(f, name, settings, "", notice, kept)
	}
	ctx := s.kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		var failed error
		for _, key := range slices.Sorted(maps.Keys(values)) {
			if _, failed = s.service.SetValue(ctx, &mcpv1.SetValueRequest{Profile: s.profile, Name: key, Value: values[key]}); failed != nil {
				break
			}
			kept.values = append(kept.values, key)
		}
		if failed != nil {
			s.undo(name, kept)
		}
		return func() tea.Cmd {
			if failed != nil {
				return s.hub.Fail(name + " was not added. " + strs.Full(tui.Reason(failed)))
			}
			return s.added(f, name, settings, "", notice, kept)
		}
	})
}

// Takes back what adding a server kept on the way, once the server is not kept: the values set for it, and its sign-in
func (s *servers) undo(name string, kept made) {
	ctx := s.kit.Env.Ctx
	for _, key := range kept.values {
		s.service.SetValue(ctx, &mcpv1.SetValueRequest{Profile: s.profile, Name: key})
	}
	if kept.signed {
		s.service.Remove(ctx, &mcpv1.RemoveRequest{Profile: s.profile, Name: name})
	}
}

// Keeps a new server through the one function that checks its name and its shape, then ends the flow with the server in hand and a note
// of what was made with what goes with it; a notice of the catalog for the person who installs it is said apart. A server that is refused
// keeps nothing: what was kept for it on the way is taken back
func (s *servers) added(f *tui.Flow, name string, settings map[string]any, after, notice string, kept made) tea.Cmd {
	data, err := json.Marshal(settings)
	if err != nil {
		return s.hub.Fail(err.Error())
	}
	s.hub.Busy("Adding " + name + "…")
	ctx := s.kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		saved, err := s.service.Save(ctx, &mcpv1.SaveRequest{Profile: s.profile, Name: name, Settings: string(data)})
		refused := ""
		switch issues := saved.GetIssues(); {
		case err != nil:
			refused = name + " was not added. " + strs.Full(tui.Reason(err))
		case len(issues) > 0:
			refused = name + " was not added. " + sentences(issues)
		}
		if refused != "" {
			s.undo(name, kept)
		}
		return func() tea.Cmd {
			if refused != "" {
				return s.hub.Fail(refused)
			}
			cmds := []tea.Cmd{f.End(), s.hub.Done(strings.TrimSpace("Added " + name + ". " + after)), s.load(name), s.options.Load(nil)}
			if notice != "" {
				cmds = append(cmds, s.kit.WM.Toast(notice, wm.Info, 2*wm.DefaultToastTTL))
			}
			return tea.Batch(cmds...)
		}
	})
}
