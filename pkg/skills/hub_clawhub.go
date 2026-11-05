package skills

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var clawhubSlugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Slug and owner of a ClawHub identifier, ok false for GitHub style or malformed ids
func clawhubParseIdentifier(identifier string) (slug, owner string, hasOwner, ok bool) {
	raw := pystr.Strip(identifier)
	if raw == "" {
		return "", "", false, false
	}
	hadAt := strings.HasPrefix(raw, "@")
	stripped := strings.TrimPrefix(strings.TrimPrefix(raw, "@"), "clawhub/")
	if strings.HasPrefix(stripped, "@") {
		hadAt = true
		stripped = strings.TrimPrefix(stripped, "@")
	}
	var parts []string
	for _, p := range strings.Split(stripped, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	switch {
	case len(parts) == 1:
		slug = parts[0]
	case (len(parts) == 2 && hadAt) || (len(parts) == 3 && pystr.Lower(parts[1]) == "skills"):
		owner, slug, hasOwner = parts[0], parts[len(parts)-1], true
	default:
		return "", "", false, false
	}
	if !clawhubSlugRe.MatchString(slug) || (hasOwner && !clawhubSlugRe.MatchString(owner)) {
		return "", "", false, false
	}
	return slug, owner, hasOwner, true
}

// Owner less clawhub/<slug> catalog card id of a ClawHub identifier, ok false for anything else
func clawhubCatalogIdentifier(identifier any) (string, bool) {
	s, isStr := identifier.(string)
	if !isStr {
		return "", false
	}
	slug, _, _, ok := clawhubParseIdentifier(s)
	if !ok {
		return "", false
	}
	return "clawhub/" + slug, true
}
