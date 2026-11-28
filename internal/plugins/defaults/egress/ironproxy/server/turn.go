package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	ironconfig "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	ironterminal "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/terminal"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
	egressv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1/egressv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One turn of hiding the keys, saying each step as it goes
type turn struct {
	*service
	ctx context.Context
	cfg *config.Config
	// Tells a step or how the turn ended
	tell func(event *egressv1.TurnEvent) error
	// What a tell failed with, after which nothing more is told
	sent error
	// How the turn ended
	ended *egressv1.TurnDone
}

func (s *service) Turn(ctx context.Context, request *egressv1.TurnRequest, stream egressv1connect.EgressServiceTurnServerStream) error {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return err
	}
	return s.turn(ctx, cfg, request, stream.Send).sent
}

// Turns hiding the keys on or off, the one way it is done: the page, the command line and every write of the switch end here
func (s *service) turn(ctx context.Context, cfg *config.Config, request *egressv1.TurnRequest, tell func(*egressv1.TurnEvent) error) *turn {
	t := &turn{service: s, ctx: ctx, cfg: cfg, tell: tell}
	if request.GetOn() {
		t.on(request)
	} else {
		t.off()
	}
	return t
}

// Takes a write of the switch a person asked for, such as from the command line or a reset in Settings: on sets the proxy up when it needs
// it and starts it, off stops it; how it went is the refusal of the write when it did not do what was asked
func (s *service) guard(cfg *config.Config, _ string, value any) (bool, error) {
	on, _ := value.(bool)
	t := s.turn(context.Background(), cfg, &egressv1.TurnRequest{On: on}, func(*egressv1.TurnEvent) error { return nil })
	if t.ended != nil && !t.ended.GetOk() {
		return true, errors.New(t.ended.GetNote())
	}
	return true, nil
}

func (t *turn) send(event *egressv1.TurnEvent) {
	if done := event.GetDone(); done != nil {
		t.ended = done
	}
	if t.sent == nil {
		t.sent = t.tell(event)
	}
}

func (t *turn) say(text string, tone egressv1.Tone) {
	t.send(&egressv1.TurnEvent{Kind: &egressv1.TurnEvent_Line{Line: &egressv1.TurnLine{Text: text, Tone: tone}}})
}

// Ends the turn with how it went and how the proxy stands now
func (t *turn) done(ok bool, note string) {
	status := t.egress.Status(t.ctx, t.cfg)
	t.send(&egressv1.TurnEvent{Kind: &egressv1.TurnEvent_Done{Done: &egressv1.TurnDone{Ok: ok, Note: note, On: t.cfg.GetBool(ironconfig.Enabled),
		Running: status.PID != 0 && status.Listening, Pid: int32(status.PID), Port: int32(status.Port)}}})
}

// Stops hiding the keys, and the proxy with it
func (t *turn) off() {
	was := t.cfg.GetBool(ironconfig.Enabled)
	if was {
		if err := t.cfg.Set(ironconfig.Enabled, false); err != nil {
			t.done(false, "Hide your keys was not turned off. The error was: "+strings.TrimSuffix(err.Error(), ".")+".")
			return
		}
	}
	stopped, err := t.egress.Stop(t.cfg)
	switch {
	case err != nil:
		t.done(false, "Hide your keys is now Off, but the proxy did not stop. The error was: "+strings.TrimSuffix(err.Error(), ".")+".")
	case !was && !stopped:
		t.done(true, "Hide your keys was already Off.")
	case stopped:
		t.done(true, "Hide your keys is now Off, and the proxy stopped.")
	default:
		t.done(true, "Hide your keys is now Off.")
	}
}

// Hides the keys: sets the proxy up when it is not, or when asked to again, then starts it unless asked to leave it stopped
func (t *turn) on(request *egressv1.TurnRequest) {
	if sandbox.Docker(t.cfg) == "" {
		t.done(false, "The key proxy keeps your keys out of Docker sandboxes, but Docker is not installed on this computer. Install Docker or "+
			"Podman first, then turn it on.")
		return
	}
	status := t.egress.Status(t.ctx, t.cfg)
	if request.GetAgain() || !status.Configured() || len(t.egress.Mappings(t.cfg)) == 0 {
		if !t.setup(request) {
			return
		}
	} else if !t.cfg.GetBool(ironconfig.Enabled) {
		if err := t.cfg.Set(ironconfig.Enabled, true); err != nil {
			t.done(false, "Hide your keys was not turned on. The error was: "+strings.TrimSuffix(err.Error(), ".")+".")
			return
		}
	}
	if request.GetStopped() {
		t.done(true, "The key proxy is set up and stopped. Start it before you start Docker sandboxes.")
		return
	}
	t.say("Start the proxy", egressv1.Tone_TONE_STEP)
	started, err := t.start(t.ctx, t.cfg, false)
	switch {
	case err != nil:
		t.done(false, "Hide your keys is On, but the proxy did not start. "+strings.TrimSuffix(strs.Full(err.Error()), ".")+".")
		return
	case started.GetRefusal() != "":
		for _, note := range started.GetNotes() {
			t.say(note, egressv1.Tone_TONE_WARN)
		}
		t.done(false, "Hide your keys is On, but the proxy did not start. "+started.GetRefusal()+" "+started.GetHint())
		return
	}
	for _, note := range started.GetNotes() {
		t.say(note, egressv1.Tone_TONE_WARN)
	}
	t.say(fmt.Sprintf("The proxy runs as process %d on port %d.", started.GetPid(), started.GetPort()), egressv1.Tone_TONE_GOOD)
	t.done(true, fmt.Sprintf("Hide your keys is now On. Docker sandboxes reach providers through the proxy on port %d.", started.GetPort()))
}

// Sets the proxy up: finds the provider keys it swaps first, so nothing is downloaded or written for none, then installs the program, makes
// its certificate authority, mints its tokens and writes what it runs with, a running proxy stopped while it is written. False when a step
// failed, the turn ended
func (t *turn) setup(request *egressv1.TurnRequest) bool {
	cfg := t.cfg
	port := int(request.GetTunnelPort())
	if port != 0 && (port < 1 || port > 65534) {
		t.done(false, "The port must be from 1 to 65,534, because the proxy also takes plain HTTP on the port after it.")
		return false
	}
	t.say("Find your provider keys", egressv1.Tone_TONE_STEP)
	var names []string
	if request.GetFromBitwarden() {
		var refusal []string
		if names, refusal = t.bitwardenNames(); names == nil {
			t.done(false, strings.Join(refusal, " "))
			return false
		}
		t.say("Read the names of "+strs.Count(len(names), "secret")+" from Bitwarden.", egressv1.Tone_TONE_DIM)
	}
	existing := t.egress.Mappings(cfg)
	minted := ironterminal.Merge(existing, ironterminal.Discover(cfg, names), request.GetRotate())
	if len(minted) == 0 {
		t.done(false, "No API key of a provider the proxy knows is set, such as "+strings.Join(ironterminal.BearerNames()[:min(3, len(ironterminal.BearerNames()))], ", ")+
			". Set one, then turn this on again.")
		return false
	}
	if request.GetRotate() && len(existing) > 0 {
		if backup, err := t.egress.Backup(cfg); err != nil {
			t.say("The old tokens could not be saved before they were replaced: "+err.Error(), egressv1.Tone_TONE_WARN)
		} else if backup != "" {
			t.say("Saved the old tokens to "+backup+".", egressv1.Tone_TONE_DIM)
		}
	}
	shown := &egressv1.Minted{Uncovered: ironterminal.Uncovered(cfg, names)}
	for _, m := range minted {
		shown.Mappings = append(shown.Mappings, mapping(m))
	}
	t.send(&egressv1.TurnEvent{Kind: &egressv1.TurnEvent_Minted{Minted: shown}})
	t.say("Install iron-proxy", egressv1.Tone_TONE_STEP)
	binary := t.egress.Find(t.ctx, cfg, false)
	if binary == "" {
		t.say("iron-proxy is not on your PATH, so icebath downloads it…", egressv1.Tone_TONE_DIM)
		var err error
		if binary, err = t.egress.Install(t.ctx, cfg); err != nil {
			t.done(false, "iron-proxy could not be installed. "+strs.Full(err.Error())+" You can install it yourself from "+
				"https://github.com/ironsh/iron-proxy/releases.")
			return false
		}
	}
	t.say("iron-proxy is at "+binary+". "+version(t.egress.Version(t.ctx, binary)), egressv1.Tone_TONE_GOOD)
	t.say("Make a CA certificate", egressv1.Tone_TONE_STEP)
	certificate, key, err := t.egress.Authority(t.ctx, cfg, false)
	if err != nil {
		t.done(false, "The CA certificate could not be made. "+strs.Full(err.Error()))
		return false
	}
	t.say("The CA certificate is "+certificate+".", egressv1.Tone_TONE_GOOD)
	t.say("Write proxy.yaml and save the tokens", egressv1.Tone_TONE_STEP)
	if t.egress.Status(t.ctx, cfg).PID != 0 {
		if _, err := t.egress.Stop(cfg); err != nil {
			t.done(false, "The running proxy did not stop, so its settings were not written. "+strs.Full(err.Error()))
			return false
		}
		t.say("Stopped the running proxy while its settings are written.", egressv1.Tone_TONE_DIM)
	}
	if !t.write(request, minted, certificate, key, port) {
		return false
	}
	return true
}

// Writes proxy.yaml, the tokens and the audit log, then the settings that turn hiding the keys on, saying where each file is
func (t *turn) write(request *egressv1.TurnRequest, minted []ironterminal.Mapping, certificate, key string, port int) bool {
	cfg := t.cfg
	if port == 0 {
		port = cfg.GetInt(ironconfig.TunnelPort)
	}
	hosts := slices.Clone(ironterminal.DefaultHosts)
	for _, h := range cfg.GetStringSlice(ironconfig.ExtraHosts) {
		if !slices.Contains(ironterminal.DefaultHosts, h) {
			hosts = append(hosts, h)
		}
	}
	audit := filepath.Join(t.egress.Dir(cfg), "audit.log")
	if err := ironterminal.Audit(audit); err != nil {
		t.say("The audit log could not be made: "+err.Error(), egressv1.Tone_TONE_WARN)
		audit = ""
	}
	plan := ironterminal.Plan{Mappings: minted, CA: certificate, Key: key, Port: port, Hosts: hosts,
		DenyCIDRs: cfg.GetStringSlice(ironconfig.DenyCIDRs), DenySet: cfg.Stored(ironconfig.DenyCIDRs) != nil}
	written, err := t.egress.WriteConfig(cfg, t.egress.Build(t.ctx, plan))
	if err != nil {
		t.done(false, "proxy.yaml could not be written. "+strs.Full(err.Error()))
		return false
	}
	t.say("Settings: "+written, egressv1.Tone_TONE_GOOD)
	tokens, err := t.egress.WriteMappings(cfg, plan.Mappings)
	if err != nil {
		t.done(false, "The tokens could not be saved. "+strs.Full(err.Error()))
		return false
	}
	t.say("Tokens: "+tokens, egressv1.Tone_TONE_GOOD)
	if audit != "" {
		t.say("Audit log: "+audit+". iron-proxy 0.39 does not write it yet, and each request is recorded in iron-proxy.log.", egressv1.Tone_TONE_DIM)
	}
	if _, err := t.egress.Management(cfg, false); err != nil {
		t.done(false, "The key of the proxy's management could not be made. "+strs.Full(err.Error()))
		return false
	}
	existing := cfg.GetString(ironconfig.Source)
	source := ironconfig.FromEnv
	switch {
	case request.GetFromBitwarden():
		source = ironconfig.FromBitwarden
	case request.GetNoBitwarden():
		if existing == ironconfig.FromBitwarden {
			t.say("Key source is now Environment instead of Bitwarden.", egressv1.Tone_TONE_WARN)
		}
	case existing == ironconfig.FromBitwarden:
		source = existing
		t.say("Key source stays Bitwarden, as it was.", egressv1.Tone_TONE_WARN)
	}
	for key, value := range map[string]any{ironconfig.TunnelPort: port, ironconfig.Enabled: true, ironconfig.Source: source} {
		if err := cfg.Set(key, value); err != nil {
			t.done(false, "The settings of the key proxy were not saved. "+strs.Full(err.Error()))
			return false
		}
	}
	return true
}

// Names of the secrets of the Bitwarden project for a setup that finds its keys there, or the sentences saying why there are none
func (t *turn) bitwardenNames() ([]string, []string) {
	host := t.secrets.Host()
	source, ok := host.Source(ironterminal.Bitwarden)
	if !ok || !host.Enabled(t.cfg, ironterminal.Bitwarden) {
		return nil, []string{"Keys come from Bitwarden, but Bitwarden is off in /secrets.",
			"Run `icebath secrets bitwarden setup` first, or set the proxy up without Bitwarden."}
	}
	tokenEnv := host.TokenEnv(t.cfg, source.Spec())
	if strings.TrimSpace(host.Existing(t.cfg)[tokenEnv]) == "" {
		return nil, []string{"Keys come from Bitwarden, but " + tokenEnv + " is not set in the environment.",
			"Export the Bitwarden access token as " + tokenEnv + ", then set the proxy up again."}
	}
	result := host.Fetch(t.ctx, t.cfg, source, true)
	if !result.OK() {
		return nil, []string{"The secrets in Bitwarden could not be listed: " + strings.TrimRight(result.Error, ".") + ".",
			"Fix Bitwarden in /secrets and try again, or set the proxy up without Bitwarden."}
	}
	if len(result.Secrets) == 0 {
		return nil, []string{"Bitwarden listed no secrets.", "Check the project of Bitwarden in /secrets, and that the access token may read that " +
			"project."}
	}
	names := make([]string, 0, len(result.Secrets))
	for name := range result.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// Sentence saying which version of iron-proxy is installed
func version(text string) string {
	if text == "" {
		return "Its version is not known."
	}
	return "Its version is " + strings.TrimPrefix(text, "v") + "."
}
