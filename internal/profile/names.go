package profile

import (
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// Profile that is always there and can't be deleted, the one settings are kept in until another is made and used
const Default = "default"

// Name a profile can have: a lowercase letter or digit, then up to 63 more of those, dashes and underscores
var id = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Names that collide with the installation itself or a common system binary
var reserved = map[string]bool{"icebath": true, "default": true, "test": true, "tmp": true, "root": true, "sudo": true}

// Top level commands of icebath an alias must not shadow
var subcommands = map[string]bool{"chat": true, "model": true, "gateway": true, "whatsapp": true, "login": true, "logout": true, "status": true,
	"cron": true, "doctor": true, "dump": true, "config": true, "pairing": true, "skills": true, "tools": true, "mcp": true, "sessions": true,
	"insights": true, "version": true, "update": true, "uninstall": true, "profile": true, "plugins": true, "acp": true}

const rule = "Use lowercase letters, numbers, '-' or '_', starting with a letter or number, up to 64 characters"

// Characters a suggested name drops
var unsuggested = regexp.MustCompile(`[^a-z0-9_-]+`)

// Request that names no profile it can act on, or names it in a way it can't take
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Profile a command needs that is not there, matching fs.ErrNotExist
type NotFoundError struct{ Msg string }

func (e *NotFoundError) Error() string        { return e.Msg }
func (e *NotFoundError) Is(target error) bool { return target == fs.ErrNotExist }

// Profile that is already there, matching fs.ErrExist
type ExistsError struct{ Msg string }

func (e *ExistsError) Error() string        { return e.Msg }
func (e *ExistsError) Is(target error) bool { return target == fs.ErrExist }

// Refusal of a command naming a profile that was never made
func missing(name string) error {
	return &NotFoundError{fmt.Sprintf("Profile '%s' does not exist. Create it with: icebath profile create %s", name, name)}
}

// Refusal of a delete, rename or export naming no profile, likely a typo
func unknown(name string) error {
	return &NotFoundError{fmt.Sprintf("No profile named '%s'. See your profiles with: icebath profile list", name)}
}

// Refusal to make a profile whose name is taken
func taken(name string) error {
	return &ExistsError{fmt.Sprintf("A profile named '%s' already exists. Switch to it with `icebath profile use %s`, see all profiles with `icebath "+
		"profile list`, or choose a different name.", name, name)}
}

// Whether the text has the shape of a profile name, reserved ones included
func Shaped(name string) bool {
	return id.MatchString(name)
}

// Valid name made of a typed one, my-work when nothing usable is left
func Suggest(name string) string {
	candidate := strings.Trim(unsuggested.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-_")
	if runes := []rune(candidate); len(runes) > 64 {
		candidate = string(runes[:64])
	}
	if id.MatchString(candidate) {
		return candidate
	}
	return "my-work"
}

// Refusal of a name that is no profile name, with a valid one to use instead
func Invalid(name string) error {
	s := Suggest(name)
	return &ValueError{fmt.Sprintf("%s is not a valid profile name. %s (for example: %s). Then run `icebath profile create %s`.", strconv.Quote(name),
		rule, s, s)}
}

// Name a profile goes by: lower case, default matched in any case
func Normalize(name string) (string, error) {
	stripped := strings.TrimSpace(name)
	if stripped == "" {
		return "", &ValueError{"profile name cannot be empty"}
	}
	if strings.EqualFold(stripped, Default) {
		return Default, nil
	}
	return strings.ToLower(stripped), nil
}

// Error unless the name can name a profile: shaped like one and not reserved, default passing
func Validate(name string) error {
	if name == Default {
		return nil
	}
	if !id.MatchString(name) {
		return Invalid(name)
	}
	if reserved[name] {
		return &ValueError{fmt.Sprintf("Profile name %s is reserved — it collides with either the icebath installation itself or a common "+
			"system binary.  Pick a different name.", strconv.Quote(name))}
	}
	return nil
}

// Name a profile goes by once normalized and checked
func Canon(name string) (string, error) {
	canon, err := Normalize(name)
	if err != nil {
		return "", err
	}
	if err := Validate(canon); err != nil {
		return "", err
	}
	return canon, nil
}

// Error unless the name is safe as the file name of an alias in the folder of aliases
func ValidAlias(name string) error {
	if !id.MatchString(name) {
		return &ValueError{fmt.Sprintf("Invalid alias name %s. %s.", strconv.Quote(name), rule)}
	}
	return nil
}

// Display name of a profile followed by its name in parentheses, the name alone when they are the same or it has none
func Label(name, display string) string {
	if display = strings.TrimSpace(display); display != "" && display != name {
		return fmt.Sprintf("%s (%s)", display, name)
	}
	return name
}
