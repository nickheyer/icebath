package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

func hostedMembersLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.session_hosted_members")
}

// Ignored roster entries already logged, keyed by profile, field, bounded value repr and detail
var hostedMembersReported = struct {
	sync.Mutex
	seen map[[4]string]bool
}{seen: map[[4]string]bool{}}

// One local bot this process serves under its profile name, home and owning authority
type LocalBot struct {
	Name      string
	Home      string
	Authority *SessionAuthority
	Problem   *string
	Retiring  bool
}

// Local bots this gateway serves, one per built authority, flagged when clients cannot address one by name
func LocalBots(runner any) []LocalBot {
	type pair struct {
		name      string
		authority *SessionAuthority
	}
	var pairs []pair
	if registry := registryOf(runner); registry != nil {
		for _, authority := range registry.Authorities() {
			name, _ := registry.ProfileName(authority)
			if name == "" {
				name = servedProfileName(authority.ProfileID)
			}
			pairs = append(pairs, pair{name, authority})
		}
	} else if bare, ok := runner.(launchAuthorityRunner); ok {
		if authority := bare.SessionAuthority(); authority != nil {
			pairs = append(pairs, pair{servedProfileName(authority.ProfileID), authority})
		}
	}
	counts := map[string]int{}
	for _, p := range pairs {
		counts[p.name]++
	}
	bots := make([]LocalBot, 0, len(pairs))
	for _, p := range pairs {
		var problem *string
		if counts[p.name] > 1 {
			text := "two served homes answer to that name"
			problem = &text
		} else if p.name != servedProfileName(p.authority.ProfileID) {
			text := "a linked profile home is not supported yet"
			problem = &text
		}
		bots = append(bots, LocalBot{Name: p.name, Home: pypath.PurePathStr(p.authority.ProfileID), Authority: p.authority,
			Problem: problem, Retiring: p.authority.Retiring})
	}
	return bots
}

// Why a new member may not take a profile name, the refusal reason, field and detail
type HostedMemberRefusal struct {
	Reason string
	Field  string
	Detail string
}

// Seatable profile homes by name for a group on the authority, and the names a new member may not take
func MemberRoster(ctx context.Context, authority *SessionAuthority, siblings bool) (*pydict.Dict[string, string], map[string]HostedMemberRefusal, error) {
	own, served, refused := hostedServedBots(authority)
	blocked := map[string]bool{}
	for name := range refused {
		blocked[name] = true
	}
	homes := pydict.New[string, string]()
	for name, profileHome := range served.All() {
		homes.Set(name, profileHome)
	}
	if !siblings {
		for _, name := range homes.Keys() {
			if name != own {
				homes.Delete(name)
				refused[name] = HostedMemberRefusal{"permission_denied", "members", "only a host-scoped connection seats a served profile without an entry"}
			}
		}
	}
	seated := map[string]string{}
	for name, profileHome := range served.All() {
		seated[home.Key(profileHome)] = name
	}
	configured := hostedConfiguredProfiles(ctx, authority, own)
	for _, name := range config.MappingKeys(configured) {
		if blocked[name] {
			continue
		}
		value := configured[name]
		field := "hosted_rooms.profiles." + name
		profileHome, reason, detail, err := hostedConfigEntry(name, value, served, seated)
		if err != nil {
			return nil, nil, err
		}
		_, isServed := served.Get(name)
		switch {
		case siblings && isServed:
			if reason != "" {
				hostedMembersReport(own, field, value, detail)
			}
		case reason == "":
			homes.Set(name, *profileHome)
			if _, taken := seated[home.Key(*profileHome)]; !taken {
				seated[home.Key(*profileHome)] = name
			}
			delete(refused, name)
		default:
			hostedMembersReport(own, field, value, detail)
			refused[name] = HostedMemberRefusal{reason, field, detail}
			if profileHome == nil {
				homes.Delete(name)
			} else {
				homes.Set(name, *profileHome)
			}
		}
	}
	return homes, refused, nil
}

// Named refusal of the first local member whose profile the refusals name, nil when every one is seatable
func RefuseUnseatable(members any, refused map[string]HostedMemberRefusal) error {
	list, _ := members.([]any)
	for index, raw := range list {
		entry, isDict := raw.(map[string]any)
		var target, profile any
		if isDict {
			target, profile = entry["target"], entry["profile"]
		}
		if targetMap, ok := target.(map[string]any); ok {
			kind, has := targetMap["kind"]
			if !has {
				kind = "local"
			}
			if !pyrepr.Equal(kind, "local") {
				continue
			}
		}
		name, ok := profile.(string)
		if !ok {
			continue
		}
		if refusal, found := refused[name]; found {
			field := refusal.Field
			detail := fmt.Sprintf("member %d profile '%s': %s", index, name, refusal.Detail)
			return state.NewRuntimeStoreError(refusal.Reason, &field, &detail)
		}
	}
	return nil
}

// Own profile name, served homes by name and refused names over the local bots this process serves
func hostedServedBots(authority *SessionAuthority) (string, *pydict.Dict[string, string], map[string]HostedMemberRefusal) {
	var runner any
	if authority.Runner != nil {
		runner = authority.Runner
	}
	bots := LocalBots(runner)
	own, found := "", false
	for _, bot := range bots {
		if bot.Authority == authority {
			own, found = bot.Name, true
			break
		}
	}
	if !found {
		own = servedProfileName(authority.ProfileID)
	}
	served := pydict.New[string, string]()
	refused := map[string]HostedMemberRefusal{}
	for _, bot := range bots {
		switch {
		case bot.Problem != nil:
			refused[bot.Name] = HostedMemberRefusal{"invalid_params", "members", strings.ReplaceAll(*bot.Problem, " yet", " in groups yet")}
		case bot.Retiring && bot.Authority != authority:
			continue
		default:
			served.Set(bot.Name, bot.Home)
		}
	}
	if _, isRefused := refused[own]; !isRefused {
		if !served.Has(own) {
			served.Set(own, pypath.PurePathStr(authority.ProfileID))
		}
	}
	return own, served, refused
}

// The hosted_rooms.profiles mapping under the owner scope, empty when the section holds no entries
func hostedConfiguredProfiles(ctx context.Context, authority *SessionAuthority, own string) map[string]any {
	section := loadGatewayConfig(ownerScope(ctx, authority, false), "")["hosted_rooms"]
	profiles := section
	if sectionMap, ok := section.(map[string]any); ok {
		profiles = sectionMap["profiles"]
	}
	if profiles == nil {
		return map[string]any{}
	}
	if profileMap, ok := profiles.(map[string]any); ok {
		if len(profileMap) == 0 {
			return map[string]any{}
		}
		return profileMap
	}
	hostedMembersReport(own, "hosted_rooms.profiles", profiles, "must map profile names to profile homes")
	return map[string]any{}
}

// Home one entry seats with an empty reason, else its refusal and any alias home existing rooms keep
func hostedConfigEntry(name string, value any, served *pydict.Dict[string, string], seated map[string]string) (*string, string, string, error) {
	text, isText := value.(string)
	if !roomIdentifierRe.MatchString(name) || !isText {
		return nil, "invalid_params", "must map a profile name to a home", nil
	}
	target := pypath.PurePathStr(text)
	if !filepath.IsAbs(target) {
		return nil, "invalid_params", "must be an absolute, resolved home", nil
	}
	resolved, err := pypath.Resolve(target)
	if err != nil {
		return nil, "", "", err
	}
	if pypath.Normcase(pypath.PurePathStr(resolved)) != pypath.Normcase(target) {
		return nil, "invalid_params", "must be an absolute, resolved home", nil
	}
	key := home.Key(target)
	if servedHome, isServed := served.Get(name); isServed && home.Key(servedHome) != key {
		return nil, "permission_denied", "this gateway serves that profile from another home", nil
	}
	if seatedName, taken := seated[key]; taken && seatedName != name {
		return &target, "invalid_params", fmt.Sprintf("names the home already seated as '%s'", seatedName), nil
	}
	if servedHome, isServed := served.Get(name); isServed {
		return &servedHome, "", "", nil
	}
	return &target, "", "", nil
}

// Logs one ignored entry once per value however often the roster is read
func hostedMembersReport(own, field string, value any, detail string) {
	text := []rune(pyrepr.Repr(value))
	if len(text) > 200 {
		text = text[:200]
	}
	key := [4]string{own, field, string(text), detail}
	hostedMembersReported.Lock()
	defer hostedMembersReported.Unlock()
	if hostedMembersReported.seen[key] {
		return
	}
	hostedMembersReported.seen[key] = true
	hostedMembersLog().Warn(fmt.Sprintf("Ignoring %s for profile=%s: %s", field, own, detail))
}
