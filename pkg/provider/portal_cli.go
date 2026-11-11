package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Portal home, its subscription page and the Tool Gateway documentation
const (
	DefaultPortalURL      = "https://portal.nousresearch.com"
	PortalSubscriptionURL = "https://portal.nousresearch.com/manage-subscription"
	portalDocsURL         = "https://hermes-agent.nousresearch.com/docs/user-guide/features/tool-gateway"
)

// Partners the Tool Gateway routes to today as key, label and partner
var portalCatalog = [][3]string{
	{"web", "Web search & extract", "Nous-managed"},
	{"image_gen", "Image generation", "FAL"},
	{"tts", "Text-to-speech", "OpenAI TTS"},
	{"browser", "Browser automation", "Browser Use"},
	{"modal", "Cloud terminal", "Modal"},
}

// Routing column shared by the info and tools subcommands
func portalFeatureState(feat NousFeatureState, viaNous string) string {
	if feat.ManagedByNous {
		return cliui.Color(viaNous, cliui.Green)
	}
	if feat.Active {
		if feat.CurrentProvider != "" {
			return feat.CurrentProvider
		}
		return "active"
	}
	return cliui.Color("not configured", cliui.Dim)
}

func portalHeading(w io.Writer, title string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("  "+title, cliui.Magenta))
	fmt.Fprintln(w, cliui.Color("  "+strings.Repeat("─", utf8.RuneCountInString(title)), cliui.Magenta))
}

// Effective config of the bound home, empty when it cannot be loaded
func portalConfig(ctx context.Context) (config.Tree, error) {
	tree, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return nil, err
	}
	if tree == nil {
		tree = config.Tree{}
	}
	return tree, nil
}

// Shows the Portal sign in state and the Tool Gateway routing summary
func portalCmdStatus(ctx context.Context, env PoolEnv, w io.Writer) (int, error) {
	cfg, err := portalConfig(ctx)
	if err != nil {
		return 0, err
	}
	auth := func() (status map[string]any) {
		defer func() {
			if recover() != nil {
				status = nil
			}
		}()
		return GetNousAuthStatusLocal(env)
	}()
	loggedIn := pyrepr.Truthy(auth["logged_in"])
	freeTier := pyrepr.Truthy(auth["free_tier"])
	portalHeading(w, "Nous Portal")
	switch {
	case freeTier:
		fmt.Fprintf(w, "  Auth:    %s\n", cliui.Color(FreeTierLabel+" · "+GuestModel, cliui.Green))
		fmt.Fprintf(w, "           %s\n", UpgradeHint)
		if base := auth["inference_base_url"]; pyrepr.Truthy(base) {
			fmt.Fprintf(w, "  API:     %s\n", pyrepr.Str(base))
		}
	case loggedIn:
		fmt.Fprintf(w, "  Auth:    %s\n", cliui.Color("✓ logged in", cliui.Green))
		portal := DefaultPortalURL
		if base := auth["portal_base_url"]; pyrepr.Truthy(base) {
			portal = pyrepr.Str(base)
		}
		fmt.Fprintf(w, "  Portal:  %s\n", portal)
		if base := auth["inference_base_url"]; pyrepr.Truthy(base) {
			fmt.Fprintf(w, "  API:     %s\n", pyrepr.Str(base))
		}
	default:
		fmt.Fprintf(w, "  Auth:    %s\n", cliui.Color("not logged in", cliui.Yellow))
		fmt.Fprintf(w, "  Sign up: %s\n", PortalSubscriptionURL)
		fmt.Fprintln(w, "  Login:   icebath portal")
	}
	modelCfg, _ := cfg["model"].(map[string]any)
	provider := ""
	if value := modelCfg["provider"]; pyrepr.Truthy(value) {
		provider = pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	}
	if provider == "nous" {
		fmt.Fprintf(w, "  Model:   %s\n", cliui.Color("✓ using Nous as inference provider", cliui.Green))
	} else if provider != "" {
		fmt.Fprintf(w, "  Model:   currently %s (switch with `icebath model`)\n", provider)
	}
	portalHeading(w, "Tool Gateway")
	features, ok := portalFeatures(env, cfg)
	if !ok {
		fmt.Fprintln(w, "  (could not resolve subscription state)")
		return 0, nil
	}
	items := features.Items()
	width := 0
	for _, feat := range items {
		width = max(width, utf8.RuneCountInString(feat.Label))
	}
	for _, feat := range items {
		fmt.Fprintf(w, "  %s   %s\n", pystr.Ljust(feat.Label, width), portalFeatureState(feat, "via Nous Portal"))
	}
	if !loggedIn {
		fmt.Fprintln(w)
		fmt.Fprintln(w, cliui.Color("  Docs: "+portalDocsURL, cliui.Dim))
	}
	return 0, nil
}

// Subscription features of the config, false when resolving them fails
func portalFeatures(env PoolEnv, cfg config.Tree) (features NousSubscriptionFeatures, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return GetNousSubscriptionFeatures(env, map[string]any(cfg), false), true
}

// Opens the Portal subscription page in the default browser
func portalCmdOpen(w io.Writer) int {
	fmt.Fprintf(w, "Opening %s\n", PortalSubscriptionURL)
	opened := func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		return openBrowserURL(PortalSubscriptionURL) == nil
	}()
	if opened {
		return 0
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Could not launch a browser. Visit the URL above manually.")
	return 1
}

// Lists the Tool Gateway catalog with the current routing of each tool
func portalCmdTools(ctx context.Context, env PoolEnv, w, errw io.Writer) (int, error) {
	cfg, err := portalConfig(ctx)
	if err != nil {
		return 0, err
	}
	features, ok := portalFeatures(env, cfg)
	if !ok {
		fmt.Fprintln(errw, "Could not resolve Tool Gateway state.")
		return 1, nil
	}
	portalHeading(w, "Tool Gateway catalog")
	if !features.NousAuthPresent {
		fmt.Fprintln(w, cliui.Color("  Not logged into Nous Portal — sign in with `icebath portal`.", cliui.Yellow))
		fmt.Fprintln(w)
	}
	labelWidth := 0
	for _, row := range portalCatalog {
		labelWidth = max(labelWidth, utf8.RuneCountInString(row[1]))
	}
	for _, row := range portalCatalog {
		key, label, partner := row[0], row[1], row[2]
		if key == "image_gen" {
			if managed := ManagedImagePartner(map[string]any(cfg)); managed != "" {
				partner = managed
			}
		}
		state := cliui.Color("unknown", cliui.Dim)
		if feat, known := features.Features[key]; known {
			state = portalFeatureState(feat, "✓ via Nous Portal")
		}
		fmt.Fprintf(w, "  %s  partner: %s %s\n", pystr.Ljust(label, labelWidth), pystr.Ljust(partner, 14), state)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("  Manage your subscription: "+PortalSubscriptionURL, cliui.Dim))
	fmt.Fprintln(w, cliui.Color("  Docs: "+portalDocsURL, cliui.Dim))
	return 0, nil
}

// One shot Nous Portal onboarding, the wiring behind setup --portal
func portalCmdLogin(ctx context.Context, w io.Writer) (int, error) {
	cfg, err := portalConfig(ctx)
	if err != nil {
		return 0, err
	}
	if err := RunPortalOneShot(ctx, cfg); err != nil {
		if setupInterruptedOrEOF(err) {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Portal setup cancelled.")
			return 1, nil
		}
		return 0, err
	}
	return 0, nil
}

// Runs icebath portal, the login when no subcommand is given, else info, status, open or tools
func PortalCommand(ctx context.Context, env PoolEnv, ns map[string]any, w, errw io.Writer) (int, error) {
	sub := ns["portal_command"]
	name, isText := sub.(string)
	switch {
	case sub == nil || (isText && (name == "" || name == "login")):
		return portalCmdLogin(ctx, w)
	case isText && (name == "info" || name == "status"):
		return portalCmdStatus(ctx, env, w)
	case isText && name == "open":
		return portalCmdOpen(w), nil
	case isText && name == "tools":
		return portalCmdTools(ctx, env, w, errw)
	}
	fmt.Fprintf(errw, "Unknown portal subcommand: %s\n", pyrepr.Str(sub))
	fmt.Fprintln(errw, "Run `icebath portal -h` for usage.")
	return 1, nil
}
