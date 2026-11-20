package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home the content cache writes under, the server env home else the context profile home
func mediaCacheHome(env *tools.Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Dir(context.Background())
}

// Caches MCP image content bytes in the gateway image cache and returns the path
func cacheImageFromBytes(env *tools.Env, data []byte, ext string) (string, error) {
	return mediacache.CacheImageFromBytes(context.Background(), mediaCacheHome(env), data, ext)
}

// Caches MCP audio content bytes in the gateway audio cache and returns the path
func cacheAudioFromBytes(env *tools.Env, data []byte, ext string) (string, error) {
	return mediacache.CacheAudioFromBytes(context.Background(), mediaCacheHome(env), data, ext)
}

// Caches MCP embedded resource bytes in the gateway document cache and returns the path
func cacheDocumentFromBytes(ctx context.Context, env *tools.Env, data []byte, filename string) (string, error) {
	return mediacache.CacheDocumentFromBytes(tools.WithEnvHome(ctx, env), mediaCacheHome(env), data, filename)
}
