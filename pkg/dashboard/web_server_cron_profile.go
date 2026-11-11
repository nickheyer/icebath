package dashboard

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Active profile of this backend, default for the root and for a home outside the profiles tree
func cronDefaultProfile(ctx context.Context) string {
	name := profiles.GetActiveProfileName(ctx)
	if name == "default" || name == "custom" {
		return "default"
	}
	return name
}

// Canonical name and home of a profile query value, 400 when invalid and 404 when missing
func cronProfileHome(ctx context.Context, profile string) (string, string, error) {
	raw := profile
	if raw == "" {
		raw = cronDefaultProfile(ctx)
	}
	raw = pystr.Strip(raw)
	if raw == "" {
		raw = "default"
	}
	canon, err := profiles.NormalizeProfileName(raw)
	if err == nil {
		err = profiles.ValidateProfileName(canon)
	}
	if err != nil {
		return "", "", NewHTTPException(400, err.Error())
	}
	if !profiles.ProfileExists(canon) {
		return "", "", NewHTTPException(404, fmt.Sprintf("Profile '%s' does not exist.", canon))
	}
	dir, err := profiles.GetProfileDir(canon)
	if err != nil {
		return "", "", err
	}
	return canon, dir, nil
}
