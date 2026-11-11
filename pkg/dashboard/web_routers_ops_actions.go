package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Hex text of n fresh random bytes, the secrets token_hex
func opsTokenHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// Backups folder under the home of the profile an archive belongs to
func dashboardBackupDir(ctx context.Context, profile string) (string, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(home.Dir(sctx), "backups"), nil
}

// Starts a backup in the background, into a fresh archive of the profile backups folder when no output is named
func runBackup(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	archive := ""
	output := pystr.Strip(bodyStr(body, "output"))
	if output == "" {
		stamp := time.Now().Format("2006-01-02-150405")
		dir, err := dashboardBackupDir(ctx, profile)
		if err != nil {
			return nil, err
		}
		archive = pypath.PathJoin(dir, fmt.Sprintf("icebath-backup-%s-%s.zip", stamp, opsTokenHex(4)))
		if err := pyfs.Makedirs(pypath.PathParent(archive), 0o777, true); err != nil {
			return nil, NewHTTPException(500, "Could not create backup directory: "+pypath.OSErrorText(err))
		}
		output = archive
	}
	response, err := opsSpawnAction(ctx, []string{"backup", "-o", output}, "backup", "Failed to spawn backup", "Failed to run backup", profile)
	if err != nil {
		return nil, err
	}
	if archive != "" {
		response.Set("archive", archive)
	}
	return response, nil
}

// Path with its symlinks resolved, failing on a missing component like a strict pathlib resolve
func resolveStrictPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", &pypath.ValueError{Msg: "embedded null byte"}
	}
	resolved, err := pypath.Realpath(pypath.PathStr(p), pypath.StrictAll)
	if err != nil {
		return "", err
	}
	return pypath.PathStr(resolved), nil
}

// HTTP error of a failed backup path resolution, 404 for a missing file and 400 otherwise
func backupPathError(err error) error {
	if _, ok := asHTTPException(err); ok {
		return err
	}
	var value *pypath.ValueError
	if errors.As(err, &value) {
		return err
	}
	if errors.Is(err, fs.ErrNotExist) {
		return NewHTTPException(404, "Backup not found")
	}
	return NewHTTPException(400, "Invalid backup path")
}

// Zip download of an archive that lies inside the backups folder of the profile
func downloadDashboardBackup(ctx context.Context, archive, profile string) (any, error) {
	backupDir, target, err := func() (string, string, error) {
		dir, err := dashboardBackupDir(ctx, profile)
		if err != nil {
			return "", "", err
		}
		if dir, err = pypath.PathExpanduser(dir); err != nil {
			return "", "", err
		}
		if dir, err = pypath.Resolve(dir); err != nil {
			return "", "", err
		}
		expanded, err := pypath.PathExpanduser(archive)
		if err != nil {
			return "", "", err
		}
		resolved, err := resolveStrictPath(expanded)
		return dir, resolved, err
	}()
	if err != nil {
		return nil, backupPathError(err)
	}
	if !pathIsUnder(backupDir, target) {
		return nil, NewHTTPException(403, "Backup is outside the dashboard backup directory")
	}
	if st, err := os.Stat(target); err != nil || !st.Mode().IsRegular() {
		return nil, NewHTTPException(404, "Backup not found")
	}
	name := pypath.PathName(target)
	return NewFileResponse(target, 200, "application/zip", &name, "attachment"), nil
}

// Starts an import of an archive in the background, with force when asked
func spawnImport(ctx context.Context, archive string, force bool, profile string) (*pyjson.Object, error) {
	args := []string{"import", archive}
	if force {
		args = append(args, "--force")
	}
	return opsSpawnAction(ctx, args, "import", "Failed to spawn import", "Failed to run import", profile)
}

// Imports an archive already on the host into the named profile, 404 when the path is no file
func runImport(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	profile, err := destructiveProfile(ctx, profile, "POST /api/ops/import")
	if err != nil {
		return nil, err
	}
	archive := pystr.Strip(bodyStr(body, "archive"))
	if archive == "" {
		return nil, NewHTTPException(400, "archive path is required")
	}
	if st, err := os.Stat(archive); err != nil || !st.Mode().IsRegular() {
		return nil, NewHTTPException(404, "Archive not found: "+archive)
	}
	return spawnImport(ctx, archive, pyrepr.Truthy(body.GetOr("force", false)), profile)
}

var backupUploadNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// File name of an uploaded archive reduced to safe characters and ending in zip
func safeBackupUploadName(filename *string) string {
	raw := "backup.zip"
	if filename != nil && *filename != "" {
		raw = *filename
	}
	name := pystr.Strip(pypath.PathName(raw))
	name = strings.Trim(backupUploadNameRE.ReplaceAllString(name, "-"), ".-")
	if name == "" {
		name = "backup.zip"
	}
	if !strings.HasSuffix(pystr.Lower(name), ".zip") {
		name += ".zip"
	}
	return name
}

// Stages an uploaded archive in the profile backups folder and starts its import, 400 unless it is a zip
func runImportUpload(ctx context.Context, file *UploadFile, force bool, profile string) (*pyjson.Object, error) {
	profile, err := destructiveProfile(ctx, profile, "POST /api/ops/import-upload")
	if err != nil {
		return nil, err
	}
	stagingDir, err := dashboardBackupDir(ctx, profile)
	if err != nil {
		return nil, err
	}
	if err := pyfs.Makedirs(stagingDir, 0o777, true); err != nil {
		return nil, NewHTTPException(500, "Could not create import staging directory: "+pypath.OSErrorText(err))
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	target := pypath.PathJoin(stagingDir, fmt.Sprintf("dashboard-import-%s-%s-%s", stamp, opsTokenHex(4), safeBackupUploadName(file.Filename)))
	total, err := streamUploadToPath(file, target, "Archive is too large", "Import staging directory is not writable", "Could not write uploaded archive")
	if err != nil {
		return nil, err
	}
	if !pyzip.IsZipfile(target) {
		if err := pypath.Unlink(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, NewHTTPException(400, "Uploaded archive is not a valid zip file")
	}
	response, err := spawnImport(ctx, target, force, profile)
	if err != nil {
		return nil, err
	}
	response.Set("archive", target)
	response.Set("uploaded_bytes", total)
	return response, nil
}

// Configured shell hooks of a profile with their consent state and the hook events a form may offer
func listHooks(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		validEvents := stringsAny(plugins.ValidHooks())
		specs := []*plugins.ShellHookSpec{}
		if cfg, err := webLoadConfig(sctx); err != nil {
			routersLog().Error("iter_configured_hooks failed", "exc_info", err)
		} else {
			specs = plugins.IterConfiguredShellHooks(cfg)
		}
		out := make([]any, 0, len(specs))
		for _, spec := range specs {
			entry := plugins.ShellHookAllowlistEntry(sctx, spec.Event, spec.Command)
			var matcher any
			if spec.Matcher != "" {
				matcher = spec.Matcher
			}
			var approvedAt any
			if entry != nil {
				approvedAt = entry["approved_at"]
			}
			out = append(out, pyjson.ObjectOf(
				"event", spec.Event,
				"matcher", matcher,
				"command", spec.Command,
				"timeout", int64(spec.Timeout),
				"allowed", entry != nil,
				"approved_at", approvedAt,
				"executable", plugins.ShellHookScriptIsExecutable(spec.Command),
			))
		}
		return pyjson.ObjectOf("hooks", out, "valid_events", validEvents), nil
	})
}

// Stripped event and command of a hook body, 400 when either is empty
func hookBodyFields(body *pyjson.Object) (string, string, error) {
	event := pystr.Strip(bodyStr(body, "event"))
	command := pystr.Strip(bodyStr(body, "command"))
	if event == "" || command == "" {
		return "", "", NewHTTPException(400, "event and command are required")
	}
	return event, command, nil
}

// Appends a shell hook to the profile config and records its consent when the body approves it
func createHook(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	profile, err := destructiveProfile(ctx, profile, "POST /api/ops/hooks")
	if err != nil {
		return nil, err
	}
	event, command, err := hookBodyFields(body)
	if err != nil {
		return nil, err
	}
	valid := plugins.ValidHooks()
	if !slices.Contains(valid, event) {
		return nil, NewHTTPException(400, fmt.Sprintf("Unknown event '%s'. Valid: %s", event, strings.Join(valid, ", ")))
	}
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		err := func() error {
			wctx, release, err := configWriteScope(sctx, profile)
			if err != nil {
				return err
			}
			defer release()
			cfg, err := webLoadConfig(wctx)
			if err != nil {
				return err
			}
			hooksCfg, ok := cfg["hooks"].(map[string]any)
			if !ok {
				hooksCfg = map[string]any{}
				cfg["hooks"] = hooksCfg
			}
			entries, _ := hooksCfg[event].([]any)
			newEntry := map[string]any{"command": command}
			if matcher := bodyStr(body, "matcher"); matcher != "" {
				newEntry["matcher"] = matcher
			}
			if timeout := body.GetOr("timeout", nil); timeout != nil {
				newEntry["timeout"] = timeout
			}
			hooksCfg[event] = append(entries, newEntry)
			return webSaveConfig(wctx, cfg, config.SaveOptions{})
		}()
		if err != nil {
			return nil, err
		}
		approved := false
		if pyrepr.Truthy(body.GetOr("approve", true)) {
			if err := plugins.RecordShellHookApproval(sctx, event, command); err != nil {
				routersLog().Error("hook consent record failed", "exc_info", err)
			} else {
				approved = true
			}
		}
		return pyjson.ObjectOf("ok", true, "event", event, "command", command, "approved", approved), nil
	})
}

// Removes a shell hook from the profile config and revokes its consent, 404 when no entry matched
func deleteHook(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	profile, err := destructiveProfile(ctx, profile, "DELETE /api/ops/hooks")
	if err != nil {
		return nil, err
	}
	event, command, err := hookBodyFields(body)
	if err != nil {
		return nil, err
	}
	removed, err := configScopedToThread(ctx, profile, func(sctx context.Context) (bool, error) {
		removed, err := func() (bool, error) {
			wctx, release, err := configWriteScope(sctx, profile)
			if err != nil {
				return false, err
			}
			defer release()
			cfg, err := webLoadConfig(wctx)
			if err != nil {
				return false, err
			}
			hooksCfg, ok := cfg["hooks"].(map[string]any)
			if !ok {
				return false, nil
			}
			entries, ok := hooksCfg[event].([]any)
			if !ok {
				return false, nil
			}
			kept := make([]any, 0, len(entries))
			for _, e := range entries {
				if m, ok := e.(map[string]any); ok && m["command"] == command {
					continue
				}
				kept = append(kept, e)
			}
			hooksCfg[event] = kept
			if len(kept) == 0 {
				delete(hooksCfg, event)
			}
			if len(hooksCfg) == 0 {
				delete(cfg, "hooks")
			}
			return len(kept) < len(entries), webSaveConfig(wctx, cfg, config.SaveOptions{})
		}()
		if err != nil {
			return false, err
		}
		_, _ = plugins.RevokeShellHook(sctx, command)
		return removed, nil
	})
	if err != nil {
		return nil, err
	}
	if !removed {
		return nil, NewHTTPException(404, "No matching hook found")
	}
	return pyjson.ObjectOf("ok", true), nil
}

// File count and byte size of each rollback checkpoint session of a profile with the total
func listCheckpoints(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		cpDir := pypath.PathJoin(home.Dir(sctx), "checkpoints")
		sessions := []any{}
		totalBytes := int64(0)
		if st, err := os.Stat(cpDir); err == nil && st.IsDir() {
			entries, err := os.ReadDir(cpDir)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			slices.Sort(names)
			for _, name := range names {
				child := pypath.PathJoin(cpDir, name)
				if st, err := os.Stat(child); err != nil || !st.IsDir() {
					continue
				}
				files, err := pypath.Glob(child, "**/*")
				if err != nil {
					return nil, err
				}
				size, count := int64(0), int64(0)
				for _, f := range files {
					if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
						size += st.Size()
						count++
					}
				}
				totalBytes += size
				sessions = append(sessions, pyjson.ObjectOf("session", name, "files", count, "bytes", size))
			}
		}
		return pyjson.ObjectOf("sessions", sessions, "total_bytes", totalBytes), nil
	})
}

// Router of pairing, webhooks, gateway start and stop, the credential pool, memory and the ops actions
func opsRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/pairing", &Endpoint{
		Name:   "list_pairing",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listPairing(ctx, profileQuery(c)) },
	})
	r.Post("/api/pairing/approve", &Endpoint{
		Name:   "approve_pairing",
		Body:   []BodyParam{Body("body", pairingApproveModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return approvePairing(ctx, c.Object("body")) },
	})
	r.Post("/api/pairing/revoke", &Endpoint{
		Name:   "revoke_pairing",
		Body:   []BodyParam{Body("body", pairingRevokeModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return revokePairing(ctx, c.Object("body")) },
	})
	r.Post("/api/pairing/clear-pending", &Endpoint{
		Name:   "clear_pending_pairing",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return clearPendingPairing(ctx, profileQuery(c)) },
	})
	r.Get("/api/webhooks", &Endpoint{
		Name:   "list_webhooks",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listWebhooks(ctx, profileQuery(c)) },
	})
	r.Post("/api/webhooks/enable", &Endpoint{
		Name:   "enable_webhooks",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return enableWebhooks(ctx, profileQuery(c)) },
	})
	r.Post("/api/webhooks", &Endpoint{
		Name:   "create_webhook",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", webhookCreateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return createWebhook(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Delete("/api/webhooks/{name}", &Endpoint{
		Name:   "delete_webhook",
		Params: []Param{PathParam("name", Str), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return deleteWebhook(ctx, c.Str("name"), profileQuery(c))
		},
	})
	r.Put("/api/webhooks/{name}/enabled", &Endpoint{
		Name: "set_webhook_enabled",
		Doc: "Disabled routes stay on disk (re-enable later) but the gateway rejects\n" +
			"    their events with 403; it hot-reloads the file, so no restart is needed.",
		Params: []Param{PathParam("name", Str), optProfile},
		Body:   []BodyParam{Body("body", webhookEnabledToggleModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return setWebhookEnabled(ctx, c.Str("name"), c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/gateway/start", &Endpoint{
		Name:   "start_gateway",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return startGateway(ctx, profileQuery(c)) },
	})
	r.Post("/api/gateway/stop", &Endpoint{
		Name:   "stop_gateway",
		Params: []Param{optProfile, QueryParam("interrupt", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return stopGateway(ctx, profileQuery(c), c.Bool("interrupt"))
		},
	})
	r.Get("/api/credentials/pool", &Endpoint{
		Name:   "list_credential_pool",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listCredentialPool(ctx, profileQuery(c)) },
	})
	r.Post("/api/credentials/pool", &Endpoint{
		Name:   "add_credential_pool_entry",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", credentialPoolAddModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return addCredentialPoolEntry(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Delete("/api/credentials/pool/{provider}/{index}", &Endpoint{
		Name: "remove_credential_pool_entry",
		Doc: "Remove a pool entry (``index`` is 1-based, as listed).\n\n" +
			"    Removal must be sticky: ``load_pool()`` re-seeds entries from their backing\n" +
			"    source (.env var, OAuth file, custom-provider config) on every call, so\n" +
			"    deleting only the row silently reverts on the next refresh. Dispatch through\n" +
			"    the same RemovalStep registry as ``icebath auth remove``: each source cleans\n" +
			"    its external state and suppresses ``(provider, source)`` so seeders skip it.\n" +
			"    Manual entries have no step — nothing external, and they aren't re-seeded.\n\n" +
			"    See #55217.",
		Params: []Param{PathParam("provider", Str), PathParam("index", Int), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return removeCredentialPoolEntry(ctx, c.Str("provider"), c.Int("index"), profileQuery(c))
		},
	})
	r.Get("/api/memory", &Endpoint{
		Name:   "get_memory_status",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getMemoryStatus(ctx, profileQuery(c)) },
	})
	r.Put("/api/memory/provider", &Endpoint{
		Name:   "set_memory_provider",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", memoryProviderSelectModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return setMemoryProvider(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/memory/reset", &Endpoint{
		Name:   "reset_memory",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", memoryResetModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return resetMemory(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/ops/doctor", &Endpoint{
		Name:   "run_doctor",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return opsSpawnAction(ctx, []string{"doctor"}, "doctor", "Failed to spawn doctor", "Failed to run doctor", profileQuery(c))
		},
	})
	r.Post("/api/ops/security-audit", &Endpoint{
		Name:   "run_security_audit",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return opsSpawnAction(ctx, []string{"security", "audit"}, "security-audit", "Failed to spawn security audit",
				"Failed to run security audit", profileQuery(c))
		},
	})
	r.Post("/api/ops/backup", &Endpoint{
		Name:   "run_backup",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", backupRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return runBackup(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Get("/api/ops/backup/download", &Endpoint{
		Name:   "download_dashboard_backup",
		Params: []Param{RequiredQuery("archive", Str), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return downloadDashboardBackup(ctx, c.Str("archive"), profileQuery(c))
		},
	})
	r.Post("/api/ops/import", &Endpoint{
		Name:   "run_import",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", importRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return runImport(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/ops/import-upload", &Endpoint{
		Name:   "run_import_upload",
		Params: []Param{optProfile},
		Body:   []BodyParam{FileField("file"), FormFieldDefault("force", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			file, _ := c.Value("file").(*UploadFile)
			return runImportUpload(ctx, file, c.Bool("force"), profileQuery(c))
		},
	})
	r.Get("/api/ops/hooks", &Endpoint{
		Name: "list_hooks",
		Doc: "Configured shell hooks with consent (allowlist) status, whether the\n" +
			"    script is currently executable, and the valid hook events for the form.",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listHooks(ctx, profileQuery(c)) },
	})
	r.Post("/api/ops/hooks", &Endpoint{
		Name: "create_hook",
		Doc: "Add a shell hook to config.yaml and optionally record consent.\n\n" +
			"    Shell hooks run arbitrary commands, so this is privileged: it writes the\n" +
			"    ``hooks:`` block and, with ``approve``, records the allowlist entry so the\n" +
			"    hook actually fires. Takes effect on the next session / gateway restart.",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", hookCreateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return createHook(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Delete("/api/ops/hooks", &Endpoint{
		Name:   "delete_hook",
		Doc:    "Remove a hook from config.yaml and revoke its consent allowlist entry.",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", hookDeleteModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return deleteHook(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Get("/api/ops/checkpoints", &Endpoint{
		Name: "list_checkpoints",
		Doc: "/rollback shadow-store checkpoints (read-only): count + size per session\n" +
			"    so the UI can show what a prune reclaims; pruning itself is a spawned CLI\n" +
			"    action so the confirmation logic stays in one place.",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listCheckpoints(ctx, profileQuery(c)) },
	})
	r.Post("/api/ops/checkpoints/prune", &Endpoint{
		Name:   "prune_checkpoints",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			profile, err := destructiveProfile(ctx, profileQuery(c), "POST /api/ops/checkpoints/prune")
			if err != nil {
				return nil, err
			}
			return opsSpawnAction(ctx, []string{"checkpoints", "prune"}, "checkpoints-prune", "Failed to spawn checkpoints prune",
				"Failed to prune checkpoints", profile)
		},
	})
	return r
}
