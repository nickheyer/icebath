package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/spf13/cobra"
)

// Parsed arguments of one icebath profile command line, Action empty for the bare command
type ProfileArgs struct {
	Action        string
	ProfileName   string
	Clone         bool
	CloneAll      bool
	CloneFrom     *string
	CloneChannels bool
	SyncImports   bool
	NoAlias       bool
	NoSkills      bool
	Description   *string
	Yes           bool
	Text          string
	Auto          bool
	Overwrite     bool
	AllMissing    bool
	Remove        bool
	AliasName     string
	OldName       string
	NewName       string
	Output        string
	Archive       string
	ImportName    string
	Source        string
	InstallName   string
	Alias         bool
	Force         bool
	ForceConfig   bool
}

type profileIO struct {
	ctx context.Context
	out io.Writer
	err io.Writer
	in  *bufio.Reader
}

type profileAction func(p *profileIO, a ProfileArgs) error

// Handler per profile subcommand, the empty action being the bare status command
var profileActions = map[string]profileAction{
	"":                 profileStatus,
	"list":             profileList,
	"use":              profileUse,
	"create":           profileCreate,
	"delete":           profileDelete,
	"describe":         profileDescribe,
	"show":             profileShow,
	"alias":            profileAlias,
	"rename":           profileRename,
	"purge-identity":   profilePurgeIdentity,
	"migrate-identity": profileMigrateIdentity,
	"export":           profileExport,
	"import":           profileImport,
	"install":          profileInstall,
	"update":           profileUpdate,
	"info":             profileInfo,
}

// Lifecycle calls the profile tests replace
var (
	purgeIdentityFn   = manage.PurgeProfileIdentity
	migrateIdentityFn = manage.MigrateProfileIdentity
)

// Binds icebath profile and every profile action to its handler
func (c *CLI) bindProfile() {
	for action := range profileActions {
		c.Bind(strings.TrimSpace("profile "+action), func(cmd *cobra.Command, args []string) error {
			return runProfile(cmd, action, args)
		})
	}
}

// Reads one profile command line into ProfileArgs and runs its action
func runProfile(cmd *cobra.Command, action string, args []string) error {
	f := cmd.Flags()
	str := func(name string) string { v, _ := f.GetString(name); return v }
	flag := func(name string) bool { v, _ := f.GetBool(name); return v }
	opt := func(name string) *string {
		if !f.Changed(name) {
			return nil
		}
		v := str(name)
		return &v
	}
	a := ProfileArgs{Action: action}
	switch action {
	case "rename", "migrate-identity":
		a.OldName, a.NewName = args[0], args[1]
	case "import":
		a.Archive, a.ImportName = args[0], str("name")
	case "install":
		a.Source, a.InstallName = args[0], str("name")
		a.Alias, a.Force, a.Yes = flag("alias"), flag("force"), flag("yes")
	case "", "list":
	default:
		if len(args) > 0 {
			a.ProfileName = args[0]
		}
	}
	switch action {
	case "create":
		a.Clone, a.CloneAll, a.CloneFrom = flag("clone"), flag("clone-all"), opt("clone-from")
		a.CloneChannels, a.SyncImports = flag("clone-channels"), flag("sync-imports")
		a.NoAlias, a.NoSkills, a.Description = flag("no-alias"), flag("no-skills"), opt("description")
	case "delete":
		a.Yes = flag("yes")
	case "describe":
		a.Text, a.Auto, a.Overwrite, a.AllMissing = str("text"), flag("auto"), flag("overwrite"), flag("all")
	case "alias":
		a.Remove, a.AliasName = flag("remove"), str("name")
	case "export":
		a.Output = str("output")
	case "update":
		a.ForceConfig, a.Yes = flag("force-config"), flag("yes")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return CmdProfile(ctx, a, cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin())
}

// Runs the profile action a names, a nonzero exit becoming an ExitError
func CmdProfile(ctx context.Context, a ProfileArgs, out, errw io.Writer, in io.Reader) error {
	handler, ok := profileActions[a.Action]
	if !ok {
		return nil
	}
	return handler(&profileIO{ctx: ctx, out: out, err: errw, in: bufio.NewReader(in)}, a)
}

func (p *profileIO) println(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// Prints msg to stdout or stderr and exits with code
func (p *profileIO) die(msg string, code int, toErr bool) error {
	w := p.out
	if toErr {
		w = p.err
	}
	fmt.Fprintln(w, msg)
	if code == 0 {
		return nil
	}
	return &ExitError{Code: code}
}

// Prints "Error" with the Python text of err and exits 1
func (p *profileIO) dieErr(err error) error {
	return p.die("Error: "+pyErrText(err), 1, false)
}

// Yes or no prompt where end of input counts as no
func (p *profileIO) confirm(prompt string) bool {
	fmt.Fprint(p.out, prompt)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := pystr.Lower(pystr.Strip(strings.TrimSuffix(line, "\n")))
	return answer == "y" || answer == "yes"
}

// Python str of an exception, OS errors in their errno form
func pyErrText(err error) string {
	switch err.(type) {
	case *fs.PathError, *os.LinkError, *os.SyscallError, syscall.Errno, *exec.Error:
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

func isValueError(err error) bool {
	var v *profiles.ValueError
	return errors.As(err, &v)
}

func isDistributionError(err error) bool {
	var d *profiles.DistributionError
	return errors.As(err, &d)
}

func isRuntimeError(err error) bool {
	var r *manage.RuntimeError
	var s *manage.ProfileIdentitySettlementPending
	return errors.As(err, &r) || errors.As(err, &s)
}

func isNotFound(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func isExists(err error) bool { return errors.Is(err, fs.ErrExist) }

func isDirPath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func isFilePath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Path shown relative to the user home with a tilde, the plain path outside it
func tildePosix(path string) string {
	if rel, ok := pypath.PathRelativeTo(path, pypath.Expanduser("~")); ok {
		return "~/" + rel
	}
	return pypath.PathStr(path)
}

// Value padded to width code points on the right like a Python format spec
func ljust(s string, width int) string {
	padded, _ := pyrepr.Format(s, fmt.Sprintf("<%d", width))
	return padded
}

// First n code points of a value Python slices, with the error Python raises for an unsliceable value
func pySliceText(v any, n int) (string, error) {
	switch x := v.(type) {
	case string:
		r := []rune(x)
		if len(r) > n {
			r = r[:n]
		}
		return string(r), nil
	case []any:
		return "", fmt.Errorf("unsupported format string passed to list.__format__")
	case map[string]any:
		return "", fmt.Errorf("slice(None, %d, None)", n)
	}
	return "", fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(v))
}

// Python value or a fallback when the value is falsy
func pyOr(v any, fallback any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return fallback
}

func isActive(p manage.ProfileInfo, active string) bool {
	return p.Name == active || (active == "default" && p.IsDefault)
}

// Whether key is assigned in the dotenv file, an unreadable or misencoded file reading as unset
func envFileHasKey(envPath, key string) bool {
	_, ok := config.LoadEnvFile(envPath)[key]
	return ok
}

// Prints the summary of a pending distribution install
func renderDistributionPlan(p *profileIO, plan *manage.InstallPlan) {
	mf := plan.Manifest
	p.println("\nDistribution: %s v%s", mf.Name, mf.Version)
	if mf.Description != "" {
		p.println("  %s", mf.Description)
	}
	if mf.Author != "" {
		p.println("  Author:   %s", mf.Author)
	}
	if mf.HermesRequires != "" {
		p.println("  Requires: Icebath %s", mf.HermesRequires)
	}
	p.println("  Source:   %s", plan.Provenance)
	p.println("  Target:   %s", plan.TargetDir)
	if plan.Existing {
		if isFilePath(filepath.Join(plan.TargetDir, profiles.ManifestFilename)) {
			p.println("  (profile exists — will overwrite distribution-owned files only)")
		} else {
			p.println("  ⚠ Profile exists but is NOT a distribution.  Installing here will\n" +
				"    overwrite its SOUL.md and mcp.json and replace any skill or cron job\n" +
				"    of the same name the distribution ships.\n" +
				"    Your memories, sessions, auth.json, and .env will be preserved,\n" +
				"    but any hand-edits to distribution-owned files will be lost.")
		}
	}
	if len(mf.EnvRequires) > 0 {
		p.println("\n  Env vars:")
		for _, er := range mf.EnvRequires {
			tag := "optional"
			if er.Required {
				tag = "required"
			}
			_, inEnv := os.LookupEnv(er.Name)
			already := inEnv || (isDirPath(plan.TargetDir) && envFileHasKey(filepath.Join(plan.TargetDir, ".env"), er.Name))
			status := "—"
			switch {
			case already:
				status = "✓ set"
			case er.Required:
				status = "needs setting"
			}
			line := fmt.Sprintf("    • %s (%s, %s)", er.Name, tag, status)
			if er.Description != "" {
				line += " — " + er.Description
			}
			p.println("%s", line)
		}
	}
	if plan.HasCron {
		p.println("\n  ⚠ This distribution ships cron jobs.  They will NOT run automatically — review and enable manually.")
	}
}

// Bare icebath profile, the status of the current profile
func profileStatus(p *profileIO, _ ProfileArgs) error {
	profileName := profiles.GetActiveProfileName(p.ctx)
	dhh := tildePosix(home.Dir(p.ctx))
	list, err := manage.ListProfiles(p.ctx, false)
	if err != nil {
		return err
	}
	var current *manage.ProfileInfo
	for i := range list {
		if isActive(list[i], profileName) {
			current = &list[i]
			break
		}
	}
	display := ""
	if current != nil {
		display = current.DisplayName
	}
	p.println("\nActive profile: %s", profiles.FormatProfileLabel(profileName, display))
	p.println("Path:           %s", dhh)
	if current != nil {
		if pyrepr.Truthy(current.Model) {
			line := "Model:          " + pyrepr.Str(current.Model)
			if pyrepr.Truthy(current.Provider) {
				line += " (" + pyrepr.Str(current.Provider) + ")"
			}
			p.println("%s", line)
		}
		p.println("Gateway:        %s", runningText(current.GatewayRunning))
		p.println("Skills:         %d installed", current.SkillCount)
		if current.AliasPath != "" {
			p.println("Alias:          %s → icebath -p %s", pyOr(current.AliasName, current.Name), current.Name)
		}
	}
	p.println("")
	return nil
}

func runningText(running bool) string {
	if running {
		return "running"
	}
	return "stopped"
}

// Table of every profile with model, gateway, alias and distribution, then shared credential warnings
func profileList(p *profileIO, _ ProfileArgs) error {
	list, err := manage.ListProfiles(p.ctx, false)
	if err != nil {
		return err
	}
	active := profiles.GetActiveProfileName(p.ctx)
	if len(list) == 0 {
		p.println("No profiles found.")
		return nil
	}
	p.println("\n %s %s %s %s %s", ljust("Profile", 16), ljust("Model", 28), ljust("Gateway", 12), ljust("Alias", 12), "Distribution")
	p.println(" %s    %s    %s    %s    %s", strings.Repeat("─", 15), strings.Repeat("─", 27), strings.Repeat("─", 11),
		strings.Repeat("─", 11), strings.Repeat("─", 20))
	for _, info := range list {
		marker := "  "
		if isActive(info, active) {
			marker = " ◆"
		}
		name := profiles.FormatProfileLabel(info.Name, info.DisplayName)
		model, err := pySliceText(pyOr(info.Model, "—"), 26)
		if err != nil {
			return err
		}
		alias := "—"
		if info.AliasPath != "" && !info.IsDefault {
			alias = pyrepr.Str(pyOr(info.AliasName, info.Name))
		}
		dist := "—"
		if pyrepr.Truthy(info.DistributionName) {
			dist, _ = pySliceText(pyrepr.Str(info.DistributionName)+"@"+pyrepr.Str(pyOr(info.DistributionVersion, "?")), 30)
		}
		p.println("%s%s %s %s %s %s", marker, ljust(name, 15), ljust(model, 28), ljust(runningText(info.GatewayRunning), 12),
			ljust(alias, 12), dist)
	}
	p.println("")
	for _, line := range sharedCredentialWarnings(p.ctx, list) {
		p.println("%s", line)
	}
	return nil
}

// One warning per named profile whose bot credential is identical to the default's, then a blank line
func sharedCredentialWarnings(ctx context.Context, list []manage.ProfileInfo) []string {
	var def *manage.ProfileInfo
	for i := range list {
		if list[i].IsDefault {
			def = &list[i]
			break
		}
	}
	if def == nil {
		return nil
	}
	var lines []string
	for _, info := range list {
		if info.IsDefault {
			continue
		}
		shared, err := manage.SharedChannelCredentials(ctx, info.Path, def.Path)
		if err != nil {
			continue
		}
		if len(shared) > 0 {
			lines = append(lines, manage.SharedCredentialWarning(info.Name, shared, "default"))
		}
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	return lines
}

// Sets the sticky active profile
func profileUse(p *profileIO, a ProfileArgs) error {
	if err := profiles.SetActiveProfile(a.ProfileName); err != nil {
		if isValueError(err) || isNotFound(err) {
			return p.dieErr(err)
		}
		return err
	}
	if a.ProfileName == "default" {
		p.println("Switched to: default (~/.icebath)")
	} else {
		p.println("Switched to: %s", a.ProfileName)
	}
	return nil
}

// Directory of the clone source profile, a not found error when it is not a directory
func sourceProfileDir(label string) (string, error) {
	dir, err := profiles.GetProfileDir(label)
	if err != nil {
		return "", err
	}
	if !isDirPath(dir) {
		return "", &profiles.NotFoundError{Msg: dir}
	}
	return dir, nil
}

// Names the channels a clone left behind, or warns about credentials a channel clone now shares
func printChannelCloneNotice(p *profileIO, name, sourceLabel string, cloneChannels bool, cloneFlag string) error {
	sourceDir, err := sourceProfileDir(sourceLabel)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cloneChannels {
		platforms, err := manage.ChannelPlatformsConfigured(p.ctx, sourceDir)
		if err != nil {
			return err
		}
		for _, line := range manage.FormatStrippedNotice(name, platforms, cloneFlag) {
			p.println("%s", line)
		}
		return nil
	}
	dir, err := profiles.GetProfileDir(name)
	if err != nil {
		return err
	}
	shared, err := manage.SharedChannelCredentials(p.ctx, dir, sourceDir)
	if err != nil {
		return err
	}
	if len(shared) > 0 {
		p.println("%s", manage.SharedCredentialWarning(name, shared, sourceLabel))
	}
	return nil
}

// Length of a Python sized value, zero for anything unsized
func pyLen(v any) int {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		return rv.Len()
	}
	return 0
}

// Creates a profile, seeding or cloning it, then its wrapper and the next steps
func profileCreate(p *profileIO, a ProfileArgs) error {
	name := a.ProfileName
	cloneConfig := a.Clone || a.CloneFrom != nil
	cloned := cloneConfig || a.CloneAll
	sourceLabel := ""
	if a.CloneFrom != nil {
		sourceLabel = *a.CloneFrom
	}
	if sourceLabel == "" {
		sourceLabel = profiles.GetActiveProfileName(p.ctx)
	}
	profileDir, err := manage.CreateProfile(p.ctx, name, manage.CreateOptions{
		CloneFrom: a.CloneFrom, CloneAll: a.CloneAll, CloneConfig: cloneConfig, NoSkills: a.NoSkills,
		Description: a.Description, CloneChannels: a.CloneChannels, SyncImports: a.SyncImports,
	})
	if err != nil {
		if isValueError(err) || isExists(err) || isNotFound(err) {
			return p.dieErr(err)
		}
		return err
	}
	p.println("\nProfile '%s' created at %s", name, profileDir)
	if cloned {
		if err := printCloneSummary(p, a, name, sourceLabel, profileDir); err != nil {
			return err
		}
	} else {
		result := manage.SeedProfileSkills(p.ctx, profileDir, false)
		switch {
		case len(result) > 0 && pyrepr.Truthy(result["skipped_opt_out"]):
			p.println("No bundled skills seeded (--no-skills). Delete .no-bundled-skills in the profile to opt back in.")
		case len(result) > 0:
			p.println("%d bundled skills synced.", pyLen(result["copied"]))
		default:
			p.println("⚠ Skills could not be seeded. Run `%s update` to retry.", name)
		}
	}
	if !a.NoAlias {
		if err := createProfileAlias(p, name); err != nil {
			return err
		}
	}
	return printCreateNextSteps(p, a, name, profileDir)
}

// What a clone carried over, the channel notice and the Honcho config clone
func printCloneSummary(p *profileIO, a ProfileArgs, name, sourceLabel, profileDir string) error {
	if a.CloneAll {
		p.println("Full copy from %s (excluding session history, cron jobs, backups, and snapshots).", sourceLabel)
	} else {
		p.println("Cloned config, .env, SOUL.md, and skills from %s.", sourceLabel)
		if names := manage.ClonedPluginNames(profileDir); len(names) > 0 {
			p.println("Cloned installed plugins too: %s.", strings.Join(names, ", "))
		}
		if provider := memory.ClonedMemoryProvider(profileDir); provider != "" {
			p.println("Cloned memory provider config (%s) too.", provider)
		}
	}
	if a.SyncImports {
		p.println("Import sources carried over — `icebath -p %s import-agent --sync` "+
			"keeps pulling the same Claude Code / Codex trees.", name)
	}
	cloneFlag := "--clone"
	if a.CloneAll {
		cloneFlag = "--clone-all"
	}
	if err := printChannelCloneNotice(p, name, sourceLabel, a.CloneChannels, cloneFlag); err != nil {
		return err
	}
	clonedHoncho, err := memory.CloneHonchoForProfile(p.ctx, name)
	var refused *memory.ConfigWriteRefused
	switch {
	case err == nil && clonedHoncho:
		p.println("Honcho config cloned (peer: %s)", name)
	case errors.As(err, &refused):
		p.println("Honcho config not cloned: %s", refused.Error())
	}
	return nil
}

// Creates the profile wrapper unless its name collides, warning when the wrapper dir is off PATH
func createProfileAlias(p *profileIO, name string) error {
	collision, err := profiles.CheckAliasCollision(name)
	if err != nil {
		return err
	}
	if collision != "" {
		p.println("\n⚠ Cannot create alias '%s' — %s", name, collision)
		p.println("  Choose a custom alias:  icebath profile alias %s --name <custom>", name)
		p.println("  Or access via flag:     icebath -p %s chat", name)
		return nil
	}
	wrapperPath, err := profiles.CreateWrapperScript(name, "")
	if err != nil || wrapperPath == "" {
		return err
	}
	p.println("Wrapper created: %s", wrapperPath)
	inPath, err := profiles.IsWrapperDirInPath()
	if err != nil || inPath {
		return err
	}
	dir, err := profiles.WrapperDir()
	if err != nil {
		return err
	}
	p.println("\n⚠ %s is not in your PATH.", dir)
	p.println("  Add to your shell config (~/.bashrc or ~/.zshrc):")
	p.println(`    export PATH="$HOME/.local/bin:$PATH"`)
	return nil
}

// Next steps after a create, naming the running multiplexer when it serves or can serve the profile
func printCreateNextSteps(p *profileIO, a ProfileArgs, name, profileDir string) error {
	display := tildePosix(profileDir)
	p.println("\nNext steps:")
	p.println("  %s setup              Configure API keys and model", name)
	p.println("  %s chat               Start chatting", name)
	served, recorded, err := manage.RecordedServedProfiles()
	if err != nil {
		return err
	}
	servedNow := false
	if recorded {
		canon, err := profiles.NormalizeProfileName(name)
		if err != nil {
			return err
		}
		for _, s := range served {
			sc, err := profiles.NormalizeProfileName(s)
			if err != nil {
				return err
			}
			servedNow = servedNow || sc == canon
		}
	}
	switch {
	case servedNow:
		p.println("  (served now by the running multiplexed gateway — add its bot token and it connects)")
	case recorded:
		p.println("  icebath gateway restart    Serve this profile from the running multiplexed gateway")
	default:
		p.println("  %s gateway start      Start the messaging gateway", name)
	}
	if a.Clone || a.CloneAll {
		p.println("\n  Edit %s/.env for different API keys", display)
		p.println("  Edit %s/SOUL.md for different personality", display)
	} else {
		p.println("\n  ⚠ This profile has no API keys yet. Run '%s setup' first,", name)
		p.println("    or it will inherit keys from your shell environment.")
		p.println("  Edit %s/SOUL.md to customize personality", display)
	}
	p.println("")
	return nil
}

// Deletes a profile after its confirmation
func profileDelete(p *profileIO, a ProfileArgs) error {
	if _, err := manage.DeleteProfile(p.ctx, a.ProfileName, a.Yes); err != nil {
		if isValueError(err) || isNotFound(err) || isRuntimeError(err) {
			return p.dieErr(err)
		}
		return err
	}
	return nil
}

// Profile dir describe edits, default meaning the current home
func describeTargetDir(ctx context.Context, name string) (string, error) {
	canon, err := profiles.NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	if canon == "default" {
		return home.Dir(ctx), nil
	}
	return profiles.GetProfileDir(name)
}

// Shows, sets or generates a profile description
func profileDescribe(p *profileIO, a ProfileArgs) error {
	name := a.ProfileName
	switch {
	case a.AllMissing && !a.Auto:
		return p.die("profile describe: --all requires --auto", 2, true)
	case a.AllMissing && (a.Text != "" || name != ""):
		return p.die("profile describe: --all is mutually exclusive with a profile name / --text", 2, true)
	case !a.AllMissing && name == "":
		return p.die("profile describe: profile name is required (or --all --auto)", 2, true)
	case a.Text != "" && a.Auto:
		return p.die("profile describe: --text is mutually exclusive with --auto", 2, true)
	}
	if name != "" && a.Text == "" && !a.Auto {
		profileDir, err := describeTargetDir(p.ctx, name)
		if err != nil {
			return p.die("Error: "+pyErrText(err), 1, true)
		}
		if !isDirPath(profileDir) {
			return p.die(fmt.Sprintf("Error: profile '%s' not found", name), 1, true)
		}
		meta := profiles.ReadProfileMeta(profileDir)
		if meta.Description == "" {
			p.println("(no description set for '%s')", name)
		} else {
			tag := ""
			if meta.DescriptionAuto {
				tag = "[auto] "
			}
			p.println("%s%s", tag, meta.Description)
		}
		return nil
	}
	if a.Text != "" {
		dir, err := describeTargetDir(p.ctx, name)
		if err == nil {
			auto := false
			err = profiles.WriteProfileMeta(p.ctx, dir, profiles.MetaUpdate{Description: &a.Text, DescriptionAuto: &auto})
		}
		if err != nil {
			return p.die("Error: "+pyErrText(err), 1, true)
		}
		p.println("Description updated for '%s'.", name)
		return nil
	}
	targets := []string{name}
	if a.AllMissing {
		missing, err := manage.ListDescribableProfiles(p.ctx, true)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return p.die("All profiles already have descriptions.", 0, false)
		}
		targets = missing
	}
	okCount := 0
	for _, target := range targets {
		outcome, err := manage.DescribeProfile(p.ctx, target, a.Overwrite, 0)
		if err != nil {
			return err
		}
		if outcome.OK {
			okCount++
			description := "None"
			if outcome.Description != nil {
				description = *outcome.Description
			}
			p.println("Described '%s': %s", outcome.ProfileName, description)
		} else {
			fmt.Fprintf(p.err, "profile describe %s: %s\n", outcome.ProfileName, outcome.Reason)
		}
	}
	if (a.AllMissing && okCount > 0) || (!a.AllMissing && okCount == 1) {
		return nil
	}
	return &ExitError{Code: 1}
}

// Details of one profile
func profileShow(p *profileIO, a ProfileArgs) error {
	name := a.ProfileName
	if !profiles.ProfileExists(name) {
		return p.die(fmt.Sprintf("Error: Profile '%s' does not exist.", name), 1, false)
	}
	profileDir, err := profiles.GetProfileDir(name)
	if err != nil {
		return err
	}
	model, provider, err := profiles.ReadConfigModel(p.ctx, profileDir)
	if err != nil {
		return err
	}
	gw := manage.CheckGatewayRunning(profileDir) || manage.ServedByRunningMultiplexer(name)
	distName, distVersion, distSource := profiles.ReadDistributionMeta(profileDir)
	aliasName, err := profiles.FindAliasForProfile(name)
	if err != nil {
		return err
	}
	display := profiles.ReadProfileMeta(profileDir).DisplayName
	p.println("\nProfile: %s", profiles.FormatProfileLabel(name, display))
	p.println("Path:    %s", profileDir)
	if pyrepr.Truthy(model) {
		line := "Model:   " + pyrepr.Str(model)
		if pyrepr.Truthy(provider) {
			line += " (" + pyrepr.Str(provider) + ")"
		}
		p.println("%s", line)
	}
	p.println("Gateway: %s", runningText(gw))
	p.println("Skills:  %d", manage.CountSkills(profileDir))
	p.println(".env:    %s", existsText(pathExists(filepath.Join(profileDir, ".env"))))
	p.println("SOUL.md: %s", existsText(pathExists(filepath.Join(profileDir, "SOUL.md"))))
	if pyrepr.Truthy(distName) {
		p.println("Distribution: %s@%s", pyrepr.Str(distName), pyrepr.Str(pyOr(distVersion, "?")))
		if pyrepr.Truthy(distSource) {
			p.println("Installed from: %s", pyrepr.Str(distSource))
		}
		p.println("  (run `icebath profile info %s` for full manifest)", name)
	}
	if aliasName != "" {
		wrapper, err := profiles.WrapperPath(aliasName)
		if err != nil {
			return err
		}
		p.println("Alias:   %s → icebath -p %s  (%s)", aliasName, name, wrapper)
	}
	p.println("")
	return nil
}

func existsText(exists bool) string {
	if exists {
		return "exists"
	}
	return "not configured"
}

// Creates or removes a profile wrapper script, removal working for an orphan alias
func profileAlias(p *profileIO, a ProfileArgs) error {
	name := a.ProfileName
	aliasName := a.AliasName
	if aliasName == "" {
		aliasName = name
	}
	if err := profiles.ValidateAliasName(aliasName); err != nil {
		if isValueError(err) {
			return p.dieErr(err)
		}
		return err
	}
	if a.Remove {
		exists := profiles.ProfileExists(name)
		if a.AliasName != "" && !exists {
			return p.die(fmt.Sprintf("Error: Profile '%s' does not exist.", name), 1, false)
		}
		removed, err := profiles.RemoveWrapperScript(aliasName)
		switch {
		case err != nil:
			return err
		case removed:
			p.println("✓ Removed alias '%s'", aliasName)
		case !exists:
			return p.die(fmt.Sprintf("Error: No alias '%s' and no profile '%s'. `icebath doctor` lists orphan aliases.", aliasName, name), 1, false)
		default:
			p.println("No alias '%s' found to remove.", aliasName)
		}
		return nil
	}
	if !profiles.ProfileExists(name) {
		return p.die(fmt.Sprintf("Error: Profile '%s' does not exist.", name), 1, false)
	}
	collision, err := profiles.CheckAliasCollision(aliasName)
	if err != nil {
		return err
	}
	if collision != "" {
		return p.die("Error: "+collision, 1, false)
	}
	target := ""
	if a.AliasName != "" {
		target = name
	}
	wrapperPath, err := profiles.CreateWrapperScript(aliasName, target)
	if err != nil || wrapperPath == "" {
		return err
	}
	p.println("✓ Alias created: %s", wrapperPath)
	inPath, err := profiles.IsWrapperDirInPath()
	if err != nil || inPath {
		return err
	}
	dir, err := profiles.WrapperDir()
	if err != nil {
		return err
	}
	p.println("⚠ %s is not in your PATH.", dir)
	return nil
}

// Renames a profile, a default rename only setting its display name
func profileRename(p *profileIO, a ProfileArgs) error {
	newDir, err := manage.RenameProfile(p.ctx, a.OldName, a.NewName)
	if err == nil {
		var canon string
		if canon, err = profiles.NormalizeProfileName(a.OldName); err == nil && canon != "default" {
			p.println("\nProfile renamed: %s → %s", a.OldName, a.NewName)
			p.println("Path: %s\n", newDir)
		}
	}
	if err != nil {
		if isValueError(err) || isExists(err) || isNotFound(err) {
			return p.dieErr(err)
		}
		return err
	}
	return nil
}

// Retries the identity migration of a completed rename, failing while it stays pending
func profileMigrateIdentity(p *profileIO, a ProfileArgs) error {
	migrated, err := migrateIdentityFn(p.ctx, a.OldName, a.NewName)
	if err != nil {
		if isValueError(err) || isNotFound(err) {
			return p.dieErr(err)
		}
		return err
	}
	if !migrated {
		return p.die(fmt.Sprintf("Error: session identity was not migrated. Restart or stop the gateway, then run:\n"+
			"    icebath profile migrate-identity %s %s", a.OldName, a.NewName), 1, true)
	}
	p.println("✓ Session/routing identity migrated: %s → %s", a.OldName, a.NewName)
	return nil
}

// Retries the identity purge of a completed delete, failing while it stays pending
func profilePurgeIdentity(p *profileIO, a ProfileArgs) error {
	purged, err := purgeIdentityFn(p.ctx, a.ProfileName)
	if err != nil {
		if isValueError(err) {
			return p.dieErr(err)
		}
		return err
	}
	if !purged {
		return p.die(fmt.Sprintf("Error: session identity was not purged. Restart or stop the gateway, then run:\n"+
			"    icebath profile purge-identity %s", a.ProfileName), 1, true)
	}
	p.println("✓ Session/routing identity purged: %s", a.ProfileName)
	return nil
}

// Exports a profile to an archive, the managed export path when no output is given
func profileExport(p *profileIO, a ProfileArgs) error {
	output := a.Output
	var err error
	if output == "" {
		output, err = manage.GetProfileExportPath(a.ProfileName, "")
	}
	var result string
	if err == nil {
		result, err = manage.ExportProfile(a.ProfileName, output, nil)
	}
	if err != nil {
		if isValueError(err) || pypath.IsOSError(err) {
			return p.dieErr(err)
		}
		return err
	}
	p.println("✓ Exported '%s' to %s", a.ProfileName, result)
	return nil
}

// Imports a profile archive and creates its wrapper when the name is free
func profileImport(p *profileIO, a ProfileArgs) error {
	err := func() error {
		profileDir, err := manage.ImportProfile(a.Archive, a.ImportName)
		if err != nil {
			return err
		}
		name := filepath.Base(profileDir)
		p.println("✓ Imported profile '%s' at %s", name, profileDir)
		collision, err := profiles.CheckAliasCollision(name)
		if err != nil {
			return err
		}
		if collision == "" {
			wrapperPath, err := profiles.CreateWrapperScript(name, "")
			if err != nil {
				return err
			}
			if wrapperPath != "" {
				p.println("  Wrapper created: %s", wrapperPath)
			}
		}
		p.println("")
		return nil
	}()
	if err != nil && (isValueError(err) || isExists(err) || isNotFound(err)) {
		return p.dieErr(err)
	}
	return err
}

// Previews a distribution from a scratch stage, confirms, then installs it
func profileInstall(p *profileIO, a ProfileArgs) error {
	err := func() error {
		proceed, err := previewInstall(p, a)
		if err != nil || !proceed {
			return err
		}
		plan, err := manage.InstallDistribution(p.ctx, a.Source, a.InstallName, a.Force, a.Alias)
		if err != nil {
			return err
		}
		p.println("\n✓ Installed '%s' v%s", plan.Manifest.Name, plan.Manifest.Version)
		p.println("  Profile path: %s", plan.TargetDir)
		if len(plan.Manifest.EnvRequires) > 0 {
			p.println("  Next: copy .env.EXAMPLE to .env and fill in required keys:\n    %s/.env.EXAMPLE", plan.TargetDir)
		}
		if plan.HasCron {
			p.println("  Cron jobs were included but are NOT scheduled automatically.\n  Review them with:  icebath -p %s cron list",
				plan.Manifest.Name)
		}
		p.println("\n  Use with:      icebath -p %s chat", plan.Manifest.Name)
		return nil
	}()
	if err != nil && (isDistributionError(err) || isValueError(err)) {
		return p.dieErr(err)
	}
	return err
}

// Stages the source into a scratch dir removed afterwards, prints its plan and asks for confirmation unless yes
func previewInstall(p *profileIO, a ProfileArgs) (proceed bool, err error) {
	tmp, err := os.MkdirTemp("", "icebath_dist_preview_")
	if err != nil {
		return false, err
	}
	defer pyfs.CloseTempDir(tmp, &err)
	plan, err := manage.PlanInstall(p.ctx, a.Source, tmp, a.InstallName)
	if err != nil {
		return false, err
	}
	renderDistributionPlan(p, plan)
	if !a.Yes && !p.confirm("\nProceed with install? [y/N] ") {
		p.println("Install cancelled.")
		return false, nil
	}
	return true, nil
}

// Re-pulls an installed distribution after a confirmation unless yes
func profileUpdate(p *profileIO, a ProfileArgs) error {
	err := func() error {
		canon, err := profiles.NormalizeProfileName(a.ProfileName)
		if err != nil {
			return err
		}
		dir, err := profiles.GetProfileDir(canon)
		if err != nil {
			return err
		}
		current, err := profiles.ReadManifest(dir)
		if err != nil {
			return err
		}
		if current == nil {
			return p.die(fmt.Sprintf("Error: Profile '%s' is not a distribution (no distribution.yaml). "+
				"Only profiles installed via `icebath profile install` can be updated.", canon), 1, false)
		}
		if !a.Yes {
			source := current.Source
			if source == "" {
				source = "(no source)"
			}
			p.println("\nUpdate '%s' from: %s", canon, source)
			p.println("  Currently at version %s", current.Version)
			if a.ForceConfig {
				p.println("  --force-config set: config.yaml WILL be overwritten.")
			} else {
				p.println("  config.yaml will be preserved (pass --force-config to overwrite).")
			}
			p.println("  User data (memories, sessions, auth, .env) will NOT be touched.")
			if !p.confirm("\nProceed? [y/N] ") {
				p.println("Update cancelled.")
				return nil
			}
		}
		plan, err := manage.UpdateDistribution(p.ctx, canon, a.ForceConfig)
		if err != nil {
			return err
		}
		p.println("\n✓ Updated '%s' → v%s", plan.Manifest.Name, plan.Manifest.Version)
		if plan.HasCron {
			p.println("  Cron files were refreshed.  Review with:  icebath -p %s cron list", plan.Manifest.Name)
		}
		return nil
	}()
	if err != nil && (isDistributionError(err) || isValueError(err)) {
		return p.dieErr(err)
	}
	return err
}

// Manifest keys info prints when set, with their labels
var profileInfoFields = [][2]string{
	{"description", "Description:  "},
	{"author", "Author:       "},
	{"license", "License:      "},
	{"hermes_requires", "Requires:     Icebath "},
	{"source", "Source:       "},
	{"installed_at", "Installed:    "},
}

// Shows a profile's distribution manifest
func profileInfo(p *profileIO, a ProfileArgs) error {
	data, err := manage.DescribeDistribution(a.ProfileName)
	if err != nil {
		if isDistributionError(err) || isValueError(err) {
			return p.dieErr(err)
		}
		return err
	}
	if len(data) == 0 {
		p.println("Profile '%s' is not a distribution (no distribution.yaml).", a.ProfileName)
		return nil
	}
	version, ok := data["version"]
	if !ok {
		version = "?"
	}
	p.println("\nDistribution: %s", pyrepr.Str(data["name"]))
	p.println("Version:      %s", pyrepr.Str(version))
	for _, field := range profileInfoFields {
		if v := data[field[0]]; pyrepr.Truthy(v) {
			p.println("%s%s", field[1], pyrepr.Str(v))
		}
	}
	envReqs, _ := pyOr(data["env_requires"], []any{}).([]any)
	if len(envReqs) > 0 {
		p.println("\nEnvironment variables:")
		for _, raw := range envReqs {
			er, _ := raw.(map[string]any)
			tag := "optional"
			if req, ok := er["required"]; !ok || pyrepr.Truthy(req) {
				tag = "required"
			}
			line := fmt.Sprintf("  %s (%s)", pyrepr.Str(er["name"]), tag)
			if pyrepr.Truthy(er["description"]) {
				line += " — " + pyrepr.Str(er["description"])
			}
			p.println("%s", line)
			if def, ok := er["default"]; ok && def != nil {
				p.println("      default: %s", pyrepr.Str(def))
			}
		}
	}
	p.println("")
	return nil
}
