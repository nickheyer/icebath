package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	serviceBase        = "icebath-gateway"
	ServiceDescription = "Icebath Agent Gateway - Messaging Platform Integration"
)

var (
	serviceProfileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\n?$`)
	errUnitDecode        = errors.New("'utf-8' codec can't decode the systemd unit file")
	serviceGeteuid       = os.Geteuid
	serviceGOOS          = runtime.GOOS
	systemUnitDir        = "/etc/systemd/system"
	serviceLookupAccount = func() (*user.User, error) { return user.LookupId(strconv.Itoa(os.Getuid())) }
)

// Installs the service naming facts pkg/profiles/manage reaches through profiles.GatewayServiceHooks
func init() {
	h := &profiles.GatewayServiceHooks
	h.ServiceName = func(ctx context.Context) string { return mustService(GetServiceName(ctx)) }
	h.UserSystemdUnitDir = UserSystemdUnitDir
	h.SystemUnitDir = func() string { return systemUnitDir }
	h.LaunchdPlistPath = func(ctx context.Context) string { return mustService(GetLaunchdPlistPath(ctx)) }
	h.PinnedHome = func(definition string) (string, bool) {
		v, err := ServicePinnedHome(definition)
		return mustService(v, err), v != ""
	}
}

// Value of a naming call, raising its error the way the uncaught Python exception does
func mustService(v string, err error) string {
	if err != nil {
		panic(err)
	}
	return v
}

// Profile name when resolved home is a service safe child of the profiles dir under def, empty otherwise
func profileNameFromHome(resolvedHome, def string) string {
	base, err := pypath.Resolve(filepath.Join(def, "profiles"))
	if err != nil {
		return ""
	}
	prefix := strings.TrimSuffix(base, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(resolvedHome, prefix) {
		return ""
	}
	parts := strings.Split(strings.Trim(resolvedHome[len(prefix):], string(filepath.Separator)), string(filepath.Separator))
	if len(parts) == 1 && parts[0] != "" && serviceProfileNameRE.MatchString(parts[0]) {
		return parts[0]
	}
	return ""
}

// Resolved account native default home plus the sudo invoker native default when root under sudo
func nativeServiceHomes(ctx context.Context) (map[string]bool, error) {
	homes := map[string]bool{}
	for _, h := range []string{home.AccountDefault(ctx), home.SudoInvokerDefaultHome()} {
		if h == "" {
			continue
		}
		r, err := pypath.Resolve(h)
		if err != nil {
			return nil, err
		}
		homes[r] = true
	}
	return homes, nil
}

// Resolved ICEBATH_HOME pinned by the installed bare unit this process may read, empty off Linux or when none pins
func bareUnitPinnedHome(ctx context.Context) (string, error) {
	if serviceGOOS != "linux" {
		return "", nil
	}
	dir := systemUnitDir
	if serviceGeteuid() != 0 {
		dir = UserSystemdUnitDir(ctx)
	}
	pinned, err := hermesHomePinnedByUnit(filepath.Join(dir, serviceBase+".service"))
	if err != nil || pinned == "" {
		return "", err
	}
	r, err := pypath.Resolve(pypath.Expanduser(pinned))
	if err != nil {
		return "", nil
	}
	return r, nil
}

// Whether the resolved home owns the bare service name
func homeOwnsBareServiceName(ctx context.Context, resolvedHome string) (bool, error) {
	native, err := nativeServiceHomes(ctx)
	if err != nil {
		return false, err
	}
	if native[resolvedHome] {
		return true, nil
	}
	pinned, err := bareUnitPinnedHome(ctx)
	return err == nil && resolvedHome == pinned, err
}

// Service suffix of the home in effect, empty for the bare name owner, the profile name or an eight digit path hash
func ProfileSuffix(ctx context.Context) (string, error) {
	current := home.Dir(ctx)
	resolved, err := pypath.Resolve(current)
	if err != nil {
		return "", err
	}
	owns, err := homeOwnsBareServiceName(ctx, resolved)
	if err != nil || owns {
		return "", err
	}
	root, err := pypath.Resolve(home.RootOf(current))
	if err != nil {
		return "", err
	}
	if name := profileNameFromHome(resolved, root); name != "" {
		return name, nil
	}
	if !utf8.ValidString(resolved) {
		return "", errors.New("'utf-8' codec can't encode characters: surrogates not allowed")
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])[:8], nil
}

// Profile id of the home in effect relative to the profile root, the service suffix when it has none
func currentProfileName(ctx context.Context) (string, error) {
	if name, ok := ProfileNameForHome(home.Dir(ctx)); ok && name != "" {
		return name, nil
	}
	return ProfileSuffix(ctx)
}

// Exported currentProfileName
func CurrentProfileName(ctx context.Context) (string, error) { return currentProfileName(ctx) }

// --profile flag for a profiles child of the default root, empty for the root or any other home
func profileArg(ctx context.Context, icebathHome, defaultRoot string) (string, error) {
	if icebathHome == "" {
		icebathHome = home.Dir(ctx)
	}
	h, err := pypath.Resolve(icebathHome)
	if err != nil {
		return "", err
	}
	if defaultRoot == "" {
		defaultRoot = home.RootOf(home.Dir(ctx))
	}
	def, err := pypath.Resolve(defaultRoot)
	if err != nil {
		return "", err
	}
	if h == def {
		return "", nil
	}
	if name := profileNameFromHome(h, def); name != "" {
		return "--profile " + name, nil
	}
	return "", nil
}

// Systemd service name, icebath-gateway for the bare owner and icebath-gateway-suffix otherwise
func GetServiceName(ctx context.Context) (string, error) {
	suffix, err := ProfileSuffix(ctx)
	if err != nil {
		return "", err
	}
	if suffix != "" {
		return serviceBase + "-" + suffix, nil
	}
	return serviceBase, nil
}

// XDG_CONFIG_HOME/systemd/user with the account home .config as the default base
func UserSystemdUnitDir(ctx context.Context) string {
	base := pystr.Strip(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		base = filepath.Join(home.RealHome(ctx, nil), ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

// Unit file path of the gateway service in system or user scope
func GetSystemdUnitPath(ctx context.Context, system bool) (string, error) {
	name, err := GetServiceName(ctx)
	if err != nil {
		return "", err
	}
	if system {
		return filepath.Join(systemUnitDir, name+".service"), nil
	}
	return filepath.Join(UserSystemdUnitDir(ctx), name+".service"), nil
}

// LaunchAgents plist of the gateway label under the password database home, the real home when lookup fails
func GetLaunchdPlistPath(ctx context.Context) (string, error) {
	suffix, err := ProfileSuffix(ctx)
	if err != nil {
		return "", err
	}
	name := "ai.icebath.gateway"
	if suffix != "" {
		name += "-" + suffix
	}
	dir := ""
	if u, err := serviceLookupAccount(); err == nil {
		dir = u.HomeDir
	} else {
		dir = home.RealHome(ctx, nil)
	}
	return filepath.Join(dir, "Library", "LaunchAgents", name+".plist"), nil
}

// Value of one Environment NAME directive with systemd quoting undone, empty when the file or key is absent
func unitEnvironmentValue(unitPath, name string) (string, error) {
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		return "", nil
	}
	if !utf8.Valid(raw) {
		return "", errUnitDecode
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	for _, line := range pystr.Splitlines(text, false) {
		body := pystr.Strip(line)
		if !strings.HasPrefix(body, "Environment=") {
			continue
		}
		body = pystr.Strip(body[len("Environment="):])
		if len(body) >= 2 && strings.HasPrefix(body, `"`) && strings.HasSuffix(body, `"`) {
			body = body[1 : len(body)-1]
			body = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(body, `\"`, `"`), `\\`, `\`), "%%", "%")
		} else if body == `"` {
			body = ""
		}
		if strings.HasPrefix(body, name+"=") {
			return pystr.Strip(body[len(name)+1:]), nil
		}
	}
	return "", nil
}

// ICEBATH_HOME pinned by the unit file, empty when absent or unreadable
func hermesHomePinnedByUnit(unitPath string) (string, error) {
	return unitEnvironmentValue(unitPath, "ICEBATH_HOME")
}

// ICEBATH_HOME of the on disk gateway unit in system or user scope
func hermesHomeFromSystemdUnitFile(ctx context.Context, system bool) (string, error) {
	p, err := systemdUnitPathOf(ctx, system)
	if err != nil {
		return "", err
	}
	return hermesHomePinnedByUnit(p)
}
