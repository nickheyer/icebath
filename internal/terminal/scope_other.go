//go:build !linux

package terminal

import "github.com/nickheyer/icebath/internal/config"

// Background commands go in scopes of their own only under the systemd of Linux
type scopes struct{}

func newScopes() *scopes { return &scopes{} }

func (r *Jobs) scope(_ *config.Config, _ *Job, command Command, _, _ string) Command { return command }

func stopUnit(string) bool { return true }
