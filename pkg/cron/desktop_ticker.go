package cron

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Collaborators the desktop ticker resolves at call time, the module attributes hermes tests monkeypatch
var (
	desktopProfilesToServe = func(ctx context.Context) ([]profiles.ServedProfile, error) {
		return profiles.ProfilesToServe(ctx, true, false, false)
	}
	desktopCheckGatewayRunning = func(profileDir string) bool {
		return desktopStatusHook(profiles.GatewayStatusHooks.ProfileRunning)(profileDir)
	}
	desktopServedByRunningMultiplexer = func(name string) bool { return desktopStatusHook(profiles.GatewayStatusHooks.ServesNamedProfile)(name) }
	desktopIsBuiltin                  = func(p CronScheduler) bool { _, ok := p.(*InProcessCronScheduler); return ok }
	desktopLog                        = func() *slog.Logger { return slog.Default() }
)

// Gateway status hook pkg/gateway installs, panicking with its hermes source when the binary does not link it
func desktopStatusHook[F any](h F) F {
	if any(h) == nil || reflect.ValueOf(h).IsNil() {
		panic("cron: pkg/gateway must install profiles.GatewayStatusHooks (hermes_cli/profiles.py _check_gateway_running, _served_by_running_multiplexer)")
	}
	return h
}

// Reports whether a gateway already ticks the profile store, its own process or the live default multiplexer
func gatewayOwnsCron(name, profileHome string) bool {
	return desktopCheckGatewayRunning(profileHome) || (name != "default" && desktopServedByRunningMultiplexer(name))
}

// Text of a recovered panic for the exception log lines
func desktopPanicText(r any) string {
	if err, ok := r.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(r)
}

// Ticks every local profile store from the desktop dashboard backend, standing down per tick where a gateway owns cron
func StartDesktopCronTicker(ctx context.Context, stop <-chan struct{}, interval int) {
	provider := schedulerResolver(cronStoreFrom(ctx))
	ownHome := home.Dir(ctx)
	ownName := home.ProfileNameFor(ownHome)
	if ownName == "" {
		ownName = "default"
	}
	gate := func(name *string, profileHome string) bool {
		n := ""
		if name != nil {
			n = *name
		}
		return !gatewayOwnsCron(n, profileHome)
	}
	every := float64(interval)
	opts := StartOptions{Interval: &every}
	log := desktopLog()
	if desktopIsBuiltin(provider) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("Desktop cron: profile enumeration failed; ticking active profile only\n" + desktopPanicText(r))
				}
			}()
			profileHomes := func() ([]ProfileHome, error) {
				served, err := desktopProfilesToServe(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]ProfileHome, 0, len(served))
				for _, sp := range served {
					name := sp.Name
					out = append(out, ProfileHome{Name: &name, Home: sp.Home})
				}
				return out, nil
			}
			initial, err := profileHomes()
			if err != nil {
				log.Error("Desktop cron: profile enumeration failed; ticking active profile only\n" + err.Error())
				return
			}
			if len(initial) == 0 {
				return
			}
			opts.ProfileHomesFn = profileHomes
			opts.ProfileGate = gate
			homes := make([]string, 0, len(initial))
			names := make([]any, 0, len(initial))
			for _, e := range initial {
				homes = append(homes, e.Home)
				names = append(names, *e.Name)
			}
			if _, err := logging.EnableProfileLogRouting(homes); err != nil {
				log.Error("Desktop cron: profile enumeration failed; ticking active profile only\n" + err.Error())
				return
			}
			log.Info(fmt.Sprintf("Desktop cron scheduler will tick %d profile(s): %s", len(initial), pyrepr.Repr(names)))
		}()
		if opts.ProfileHomesFn == nil {
			opts.ProfileHomesFn = func() ([]ProfileHome, error) {
				name := ownName
				return []ProfileHome{{Name: &name, Home: ownHome}}, nil
			}
			opts.ProfileGate = gate
		}
	} else {
		owned := func() (out bool) {
			defer func() {
				if r := recover(); r != nil {
					log.Warn("Desktop cron: gateway-ownership probe failed; starting the ticker\n" + desktopPanicText(r))
					out = false
				}
			}()
			return gatewayOwnsCron(ownName, ownHome)
		}
		if owned() {
			log.Info(fmt.Sprintf("Desktop cron scheduler waiting: live gateway owns cron on this ICEBATH_HOME; "+
				"the gateway ticks with live adapters (re-probing every %ds)", interval))
			for {
				if tickerWait(stop, every) {
					return
				}
				if !owned() {
					break
				}
			}
		}
	}
	log.Info(fmt.Sprintf("Desktop cron scheduler started (provider=%s, interval=%ds)", provider.Name(), interval))
	provider.Start(ctx, stop, opts)
}
