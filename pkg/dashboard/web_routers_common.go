package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Logger the extracted dashboard routers share with the web server module
func routersLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.web_server")
}

// Dashboard HTTP error of an error raised as either HTTPException type, false for any other error
func asHTTPException(err error) (*HTTPException, bool) {
	var own *HTTPException
	if errors.As(err, &own) {
		return own, true
	}
	var prov *provider.HTTPException
	if errors.As(err, &prov) {
		return NewHTTPException(prov.StatusCode, prov.Detail), true
	}
	return nil, false
}

// Error with a provider HTTPException turned into the dashboard one, any other error unchanged
func asHTTPError(err error) error {
	if h, ok := asHTTPException(err); ok {
		return h
	}
	return err
}

// Profile scope then the config mutation lock, released together by the returned func
func configWriteScope(ctx context.Context, profile string) (context.Context, func(), error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return ctx, func() {}, err
	}
	lctx, unlock := provider.LockConfigMutation(sctx)
	return lctx, func() { unlock(); release() }, nil
}

// Result of fn run inside the request profile scope
func scopedToThread[T any](ctx context.Context, profile string, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return zero, err
	}
	defer release()
	return fn(sctx)
}

// Result of fn run inside the home and secret scope of the profile without the skills lock
func configScopedToThread[T any](ctx context.Context, profile string, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return zero, err
	}
	return fn(sctx)
}

// Profile a destructive or privileged route acts on, 400 when unnamed while several profiles are served
func destructiveProfile(ctx context.Context, profile, route string) (string, error) {
	if pystr.Strip(profile) != "" {
		return profile, nil
	}
	if provider.IsMultiplexActive(ctx) {
		return "", NewHTTPException(400, route+" requires an explicit profile: this backend serves several profiles, "+
			"so an unnamed target would act on the wrong profile's data.")
	}
	return profile, nil
}

// HTTP error of an expected config backend refusal, nil for an unexpected write failure
func configRefusalHTTP(err error) *HTTPException {
	var locked *config.ConfigLockedError
	if errors.As(err, &locked) {
		return NewHTTPException(403, err.Error())
	}
	var value *config.ConfigValueError
	if errors.As(err, &value) {
		status := 400
		if value.Code == "config_level_too_large" || value.Code == "config_body_too_large" {
			status = 413
		}
		return NewHTTPException(status, err.Error())
	}
	var write *config.ConfigWriteError
	if errors.As(err, &write) && write.Code == "config_version_conflict" {
		return NewHTTPException(409, err.Error())
	}
	return nil
}

// Route error of a failed block, HTTP errors and config refusals kept, anything else logged and answered with status
func httpFailure(err error, logMsg string, status int, prefix string, detail *string) error {
	if err == nil {
		return nil
	}
	if h, ok := asHTTPException(err); ok {
		return h
	}
	var write *config.ConfigWriteError
	if errors.As(err, &write) {
		if refusal := configRefusalHTTP(err); refusal != nil {
			return refusal
		}
	}
	routersLog().Error(logMsg, "exc_info", err)
	if detail != nil {
		return NewHTTPException(status, *detail)
	}
	return NewHTTPException(status, prefix+": "+err.Error())
}

// Stripped value, 400 with detail when empty
func require(value, detail string) (string, error) {
	stripped := pystr.Strip(value)
	if stripped == "" {
		return "", NewHTTPException(400, detail)
	}
	return stripped, nil
}

const redactedCredentialWriteDetail = "Refusing to save a redacted credential preview; re-enter the full secret to replace it."

// Display only credential sentinel string, nil for a falsy value
func redactedCredentialPreview(value any) any {
	if !pyrepr.Truthy(value) {
		return nil
	}
	return "«redacted:" + config.RedactKey(pyrepr.Str(value)) + "»"
}

var legacyMaskRE = regexp.MustCompile(`\A(?:.{4}\.\.\..{4})\z`)

// Whether a submitted value is a current, stale or legacy dashboard preview by shape alone
func isRedactedCredentialPreview(submitted any) bool {
	value := ""
	if pyrepr.Truthy(submitted) {
		value = pyrepr.Str(submitted)
	}
	if strings.HasPrefix(value, "«redacted") {
		return true
	}
	return value == "***" || legacyMaskRE.MatchString(value)
}

const corruptStoreWarnIntervalS = 300.0

var (
	corruptStoreWarnedMu sync.Mutex
	corruptStoreWarnedAt = map[string]float64{}
)

type storeStatusDetail struct{ code, message string }

var corruptStoreDetail = storeStatusDetail{"state_db_corrupt",
	"state.db corrupt — run `icebath doctor` (then `icebath doctor --fix` or `icebath sessions repair`)."}

var deletedWalDetail = storeStatusDetail{"state_db_deleted_wal",
	"another icebath process still holds an old copy of the session database's write-ahead log — " +
		"quit every icebath process on this profile, run `icebath doctor` (it names the holders), " +
		"then start icebath again. Do not run `icebath doctor --fix` while they run."}

var stateDBReplacedDetail = storeStatusDetail{"state_db_replaced",
	"state.db was replaced while icebath was running — stop icebath, run `icebath doctor`, " +
		"then start it again. Do not run `icebath doctor --fix`, which would repair the wrong file in place."}

var storeStatusDetailByCause = map[string]storeStatusDetail{"deleted_wal": deletedWalDetail, "replaced": stateDBReplacedDetail}

// Structured 503 for a malformed store, warning once per store per interval and debug afterwards
func corruptStoreStatus(dbPath string, exc error) *HTTPException {
	key, now := dbPath, pythread.Monotonic()
	detail, ok := storeStatusDetailByCause[state.ClassifyPersistenceError(exc)]
	if !ok {
		detail = corruptStoreDetail
	}
	corruptStoreWarnedMu.Lock()
	last, seen := corruptStoreWarnedAt[key]
	warn := !seen || now-last >= corruptStoreWarnIntervalS
	if warn {
		corruptStoreWarnedAt[key] = now
	}
	corruptStoreWarnedMu.Unlock()
	if warn {
		routersLog().Warn(fmt.Sprintf("state.db at %s is unreadable (%s); dashboard reads return a status payload until it is "+
			"repaired — run `icebath doctor`", dbPath, exc.Error()))
	} else {
		routersLog().Debug(fmt.Sprintf("state.db at %s still has error: %s", dbPath, exc.Error()))
	}
	return NewHTTPException(503, pyjson.ObjectOf("error", detail.code, "message", detail.message, "path", key))
}

// 503 status payload for a corrupt image or replaced store error, busy, locked and every other error unchanged
func corruptStoreAsStatus(dbPath string, err error) error {
	if err == nil {
		return nil
	}
	var replaced *state.StateDBReplacedError
	if !errors.As(err, &replaced) && !state.IsMalformedDBError(err) {
		return err
	}
	return corruptStoreStatus(dbPath, err)
}
