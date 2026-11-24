package tuiserver

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hubConfigFilesRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubConfigFilesRegistry, tuigateway.ConfigFiles.Name(), 5198, rpcShape{}, configFiles)
	hubConfigFilesRegistry.install()
}

// Whether a file exists, an unreadable path reported as the error it is
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// Settings and keys files of the profile, config.yaml made from the example first when asked and the files may be edited by hand
func configFiles(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ConfigFiles.Params(raw)
	if err != nil {
		return rpcErr(rid, 4198, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	out := tuigateway.ConfigFilesResult{Config: config.GetConfigPath(h), Env: config.GetEnvPath(h)}
	if system := config.ManagedSystem(ctx, h); system != "" {
		out.Locked, out.Manager = "managed", system
	} else if files, err := config.SupportsFileTooling(ctx); err != nil {
		return nil, err
	} else if !files {
		out.Locked = "no_file"
	}
	if out.ConfigExists, err = fileExists(out.Config); err != nil {
		return nil, err
	}
	if p.Seed && out.Locked == "" && !out.ConfigExists {
		if _, err := config.SeedConfigFile(ctx, h, out.Config, ""); err != nil {
			return nil, err
		}
		out.Seeded, out.ConfigExists = true, true
	}
	if out.EnvExists, err = fileExists(out.Env); err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
