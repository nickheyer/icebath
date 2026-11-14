package config

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

// Process environment as a full load would leave it for the names its env files define, nothing published
func bootstrapEnv(homeDir, projectEnv string) map[string]string {
	env := processEnviron()
	userEnv := home.EnvFile.In(homeDir)
	type layer struct {
		path     string
		override bool
	}
	layers := []layer{{userEnv, true}}
	if os.Getenv(envOnePasswordToken) == "" {
		layers = append(layers, layer{home.OpEnvFile.In(homeDir), false})
	}
	if projectEnv != "" {
		layers = append(layers, layer{projectEnv, !fileExists(userEnv)})
	}
	if managed := managedFile(".env"); managed != "" {
		layers = append(layers, layer{managed, true})
	}
	for _, l := range layers {
		assignments, err := bootstrapStatements(l.path)
		if err != nil {
			continue
		}
		for _, a := range resolveEnvAssignments(assignments, env, l.override) {
			if _, present := env[a.name]; a.set && (l.override || !present) {
				env[a.name] = a.value
			}
		}
	}
	return env
}

// Backend kind a bootstrap environment selects, the file backend when it names none
func selectedKind(env map[string]string) string {
	if kind := strings.ToLower(strings.TrimSpace(env[EnvConfigBackend])); kind != "" {
		return kind
	}
	return FileBackendKind
}

// Config backend a load would select for the home of ctx, decided without loading or booting anything
func (e *Env) SelectedBackend(ctx context.Context) (string, error) {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return selectedKind(bootstrapEnv(h, "")), nil
}

// Publishes the backend selector and the names that backend needs before its first read, from the env files
func (s *Store) publishDeployment(_ context.Context, homeDir, projectEnv string) error {
	if s.worker != nil {
		return nil
	}
	if homeDir == "" {
		launch, err := home.RoutingHome()
		if err != nil {
			return err
		}
		homeDir = launch
	}
	env := bootstrapEnv(homeDir, projectEnv)
	names := []string{EnvConfigBackend}
	switch kind := selectedKind(env); {
	case kind == FileBackendKind:
		names = append(names, s.file.BootstrapEnvNames()...)
	case backendKind.MatchString(kind) && s.backends[kind] != nil:
		names = append(names, s.backends[kind].BootstrapEnvNames()...)
	}
	for _, name := range names {
		value, ok := env[name]
		if !ok {
			continue
		}
		if cur, present := os.LookupEnv(name); !present || cur != value {
			if err := setProcessEnv(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}
