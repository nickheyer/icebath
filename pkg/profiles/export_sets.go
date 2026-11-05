package profiles

// Package manager runtime trees at a home root, foreign state an export or import never carries
var PMRuntimeRootDirs = map[string]bool{"installs": true, "tools": true, "cache": true}

// Home relative OS credential stores dropped from every profile export
var OSCredentialStores = osCredentialStores

// Root entries of the default home a full export never carries
var DefaultExportExcludeRoot = func() map[string]bool {
	out := map[string]bool{}
	for _, n := range []string{
		"icebath-agent", ".worktrees", "profiles", "bin", "node_modules", ".icebath-runtime", "node",
		"state.db", "state.db-shm", "state.db-wal", "icebath_state.db",
		"response_store.db", "response_store.db-shm", "response_store.db-wal",
		"gateway.pid", "gateway_state.json", "processes.json", "auth.json", ".env",
		"auth.lock", "active_profile", ".update_check", "source-checks", "errors.log", ".icebath_history",
		"image_cache", "audio_cache", "document_cache", "browser_screenshots", "checkpoints", "sandboxes", "logs",
	} {
		out[n] = true
	}
	for n := range PMRuntimeRootDirs {
		out[n] = true
	}
	return out
}()

// Root entries of the default home a default profile export keeps, everything else at the root is dropped
var DefaultExportIncludeRoot = map[string]bool{
	"config.yaml": true, "SOUL.md": true, "MEMORY.md": true, "USER.md": true, "todo.json": true,
	"system_prompt.md": true, "AGENTS.md": true, "CLAUDE.md": true, ".cursorrules": true, "desktop.json": true,
	"skills": true, "cron": true, "scripts": true, "sessions": true, "plugins": true, "memories": true,
	"knowledge": true, "preferences": true,
}
