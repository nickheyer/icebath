package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Default root that may multiplex home when home is a named profile under it, empty otherwise
func MultiplexerRootFor(h string) string {
	if home.NamedProfileHome(h) == "" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(h))
	if root == h {
		return ""
	}
	return root
}

// Root whose gateway must serve named profile home when nothing records a verdict
func ImpliedHostRoot(ctx context.Context, h string) (string, error) {
	root := MultiplexerRootFor(h)
	if root == "" {
		return "", nil
	}
	verdict, known, err := HostServesNamedProfile(ctx, root, h)
	if err != nil {
		return "", err
	}
	if known && !verdict {
		return "", nil
	}
	return root, nil
}

// Root whose possibly stopped multiplexer serves home by boot policy or its recorded served set
func MultiplexerServesHome(ctx context.Context, h string) (string, error) {
	root := MultiplexerRootFor(h)
	if root == "" {
		return "", nil
	}
	verdict, known, err := HostServesNamedProfile(ctx, root, h)
	if err != nil {
		return "", err
	}
	if known {
		if verdict {
			return root, nil
		}
		return "", nil
	}
	served, ok := recGet(ReadRuntimeStatus(filepath.Join(root, "gateway_state.json")), "served_profiles").([]any)
	if !ok {
		return "", nil
	}
	names := map[string]bool{}
	for _, p := range served {
		if !pyrepr.Truthy(p) {
			continue
		}
		name, err := profiles.NormalizeProfileName(pyrepr.Str(p))
		if err != nil {
			return "", err
		}
		names[name] = true
	}
	own, err := profiles.NormalizeProfileName(filepath.Base(h))
	if err != nil {
		return "", err
	}
	if names[own] {
		return root, nil
	}
	return "", nil
}
