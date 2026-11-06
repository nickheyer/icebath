package pm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"gopkg.in/yaml.v3"
)

// Parsed plugin install metadata, an absent file empty and anything but a JSON object refused
func metadataRecords(data []byte, present bool) (map[string]any, error) {
	if !present {
		return map[string]any{}, nil
	}
	changed := &PyError{Class: "ValueError", Message: "Plugin install metadata changed while preparing the update; retry."}
	decoded, err := loadJSON(data)
	if err != nil {
		return nil, changed
	}
	records, ok := decoded.(map[string]any)
	if !ok {
		return nil, changed
	}
	return records, nil
}

// Enabled plugins that admit this core and install packages, plus candidate directories that install packages
func candidateMembers(ctx context.Context, extraDirs []string) ([]MemberDir, error) {
	return candidateMembersFor(ctx, extraDirs, nil)
}

// Candidate members of a proposed selection, a plugin new to the enablement validated in full
func candidateMembersFor(ctx context.Context, extraDirs []string, selection *EnabledPluginsOptions) ([]MemberDir, error) {
	opts := EnabledPluginsOptions{}
	if selection != nil {
		opts = *selection
	}
	selected, err := enabledPluginDirs(ctx, opts)
	if err != nil {
		return nil, err
	}
	current := selected
	if selection != nil {
		if current, err = enabledPluginDirs(ctx, EnabledPluginsOptions{}); err != nil {
			return nil, err
		}
	}
	enabled := map[string]bool{}
	for _, path := range current {
		enabled[resolvePath(path)] = true
	}
	members := []string{}
	for _, source := range selected {
		fits, err := admitted(ctx, source, !enabled[resolvePath(source)])
		if err != nil {
			return nil, err
		}
		if !fits {
			continue
		}
		candidate, err := declaresPackages(source)
		if err != nil {
			return nil, err
		}
		if candidate {
			members = append(members, source)
		}
	}
	for _, directory := range extraDirs {
		directory = purePath(directory)
		if containsString(members, directory) {
			continue
		}
		candidate, err := declaresPackages(directory)
		if err != nil {
			return nil, err
		}
		if candidate {
			members = append(members, directory)
		}
	}
	return sameMemberDirs(members), nil
}

// Admitted package declaring trees of a proposed home selection, then the extra dirs that declare packages
func CandidateMembers(ctx context.Context, extraDirs []string, proposedHome string, enabled, disabled []string) ([]string, error) {
	members, err := candidateMembersFor(ctx, extraDirs, &EnabledPluginsOptions{ProposedHome: proposedHome, Enabled: enabled, Disabled: disabled})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.Source
	}
	return out, nil
}

// Member dirs answering to their own trees, never nil
func sameMemberDirs(dirs []string) []MemberDir {
	out := make([]MemberDir, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, MemberDir{Identity: dir, Source: dir})
	}
	return out
}

// Reports whether a plugin tree declares packages to install into the shared environment
func declaresPackages(pluginDir string) (bool, error) {
	declaration, err := ReadPythonDeclaration(pluginDir)
	if err != nil {
		return false, err
	}
	return declaration.InstallsPackages()
}

// Bytes of one config file, present false when it does not exist
type configBytes struct {
	data    []byte
	present bool
}

// Config files of every dependency home with their bytes, the state a publication compares against
type configSnapshot map[string]configBytes

// Reports whether two snapshots hold the same files with the same bytes
func (s configSnapshot) equal(other configSnapshot) bool {
	if len(s) != len(other) {
		return false
	}
	for path, mine := range s {
		theirs, ok := other[path]
		if !ok || mine.present != theirs.present || !bytes.Equal(mine.data, theirs.data) {
			return false
		}
	}
	return true
}

// Current bytes of the config file of every dependency home
func selectionSnapshot(ctx context.Context) (configSnapshot, error) {
	homes, err := dependencyHomes(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := configSnapshot{}
	for _, home := range homes {
		path := filepath.Join(home, "config.yaml")
		data, present, err := ReadBytesOrNone(path)
		if err != nil {
			return nil, err
		}
		snapshot[path] = configBytes{data: data, present: present}
	}
	return snapshot, nil
}

// Manifest of a plugin tree that fits this core, a ValueError naming why it does not
func validateManifest(ctx context.Context, source string) (map[string]any, error) {
	declaration, err := ReadPythonDeclaration(source)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(source)
	reason, err := ManifestVersionError(declaration.Manifest, name)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		core, err := coreRequirementError(ctx, declaration, nil, nil)
		if err != nil {
			return nil, err
		}
		if core != "" {
			reason = fmt.Sprintf("Plugin '%s' %s", name, core)
		}
	}
	if reason != "" {
		return nil, &PyError{Class: "ValueError", Message: reason}
	}
	return declaration.Manifest, nil
}

// Refuses a newly enabled misfit, while an already enabled one missing this core sits out with false
func admitted(ctx context.Context, source string, isNew bool) (bool, error) {
	if isNew {
		if _, err := validateManifest(ctx, source); err != nil {
			return false, err
		}
		return true, nil
	}
	declaration, err := ReadPythonDeclaration(source)
	if err != nil {
		return false, err
	}
	reason, err := ManifestVersionError(declaration.Manifest, filepath.Base(source))
	if err != nil {
		return false, err
	}
	if reason != "" {
		return false, &PyError{Class: "ValueError", Message: reason}
	}
	core, err := coreRequirementError(ctx, declaration, nil, nil)
	if err != nil {
		return false, err
	}
	return core == "", nil
}

// PluginSelection is a proposed plugin enablement for one home with the config bytes it will publish
type PluginSelection struct {
	Members  []MemberDir
	configs  configSnapshot
	home     string
	path     string
	previous configBytes
	proposed []byte
}

// Snapshots a home's config and renders the proposed enablement into it, refusing a stale or misplaced selection
func newPluginSelection(ctx context.Context, data map[string]any) (*PluginSelection, error) {
	configs, err := selectionSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	home, err := rowPath(data, "home")
	if err != nil {
		return nil, err
	}
	s := &PluginSelection{configs: configs, home: resolvePath(home)}
	if !isRelativeTo(s.home, resolvePath(DependencyHomeRoot(ctx))) {
		return nil, &PyError{Class: "ValueError", Message: "config path is outside icebath state"}
	}
	s.path = filepath.Join(s.home, "config.yaml")
	if s.previous.data, s.previous.present, err = ReadBytesOrNone(s.path); err != nil {
		return nil, err
	}
	actual := "missing"
	if s.previous.present {
		sum := sha256.Sum256(s.previous.data)
		actual = hex.EncodeToString(sum[:])
	}
	if expected := data["expected_config"]; expected != nil && !jsonEquals(expected, actual) {
		return nil, &PyError{Class: "ValueError", Message: "Plugin configuration changed since this selection was read; retry."}
	}
	var doc *yaml.Node
	if s.previous.present && len(s.previous.data) > 0 {
		if doc, err = loadRoundtrip(s.previous.data); err != nil {
			return nil, err
		}
	}
	config := rtRoot(doc)
	if config == nil || rtIsNone(config) {
		doc = rtNewDocument()
		config = doc.Content[0]
	} else if config.Kind != yaml.MappingNode {
		return nil, &PyError{Class: "ValueError", Message: "configuration must be a mapping: " + s.path}
	}
	plugins := rtGet(config, "plugins")
	if plugins == nil {
		plugins = rtMapping()
		rtSet(config, "plugins", plugins)
	} else if plugins.Kind != yaml.MappingNode {
		return nil, &PyError{Class: "ValueError", Message: "plugins must be a mapping in " + s.path}
	}
	rawEnabled, err := rowItem(data, "enabled")
	if err != nil {
		return nil, err
	}
	enabled, err := pyIterStrings(rawEnabled)
	if err != nil {
		return nil, err
	}
	rawDisabled, err := rowItem(data, "disabled")
	if err != nil {
		return nil, err
	}
	disabled, err := pyIterStrings(rawDisabled)
	if err != nil {
		return nil, err
	}
	rtSet(plugins, "enabled", rtStrings(sortedCopy(enabled)))
	rtSet(plugins, "disabled", rtStrings(sortedCopy(disabled)))
	var settled []any
	if raw, present := data["settled"]; present {
		if settled, err = pyIterate(raw); err != nil {
			return nil, err
		}
	}
	if err := settleRecord(plugins, settled); err != nil {
		return nil, err
	}
	if s.proposed, err = dumpRoundtrip(doc); err != nil {
		return nil, err
	}
	var extraDirs []string
	if raw, present := data["extra_dirs"]; present {
		if extraDirs, err = pyIterStrings(raw); err != nil {
			return nil, err
		}
	}
	s.Members, err = candidateMembersFor(ctx, extraDirs, &EnabledPluginsOptions{ProposedHome: s.home, Enabled: enabled, Disabled: disabled})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Sorted copy of a string list
func sortedCopy(list []string) []string {
	out := append([]string{}, list...)
	sort.Strings(out)
	return out
}

// Journals the selection and writes the proposed config, refusing any config edit since the snapshot
func (s *PluginSelection) Publish(ctx context.Context, project string) error {
	now, err := selectionSnapshot(ctx)
	if err != nil {
		return err
	}
	data, present, err := ReadBytesOrNone(s.path)
	if err != nil {
		return err
	}
	if !now.equal(s.configs) || present != s.previous.present || !bytes.Equal(data, s.previous.data) {
		return &PyError{Class: "ValueError", Message: "plugin configuration changed while preparing publication; retry"}
	}
	factsBefore, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(s.proposed)
	row := map[string]any{
		"config":       s.path,
		"previous":     base64Value(s.previous),
		"facts_before": factsBefore,
		"config_after": hex.EncodeToString(sum[:]),
	}
	if err := writePublicationJournal(ctx, project, row); err != nil {
		return err
	}
	return publicationWrite(s.path, s.proposed)
}

// Base64 text of present bytes, nil when the file did not exist
func base64Value(b configBytes) any {
	if !b.present {
		return nil
	}
	return base64.StdEncoding.EncodeToString(b.data)
}

// Durably writes the publication journal row recovery reads after a crash
func writePublicationJournal(ctx context.Context, project string, row map[string]any) error {
	encoded, err := dumpsJSON(row, false)
	if err != nil {
		return err
	}
	return publicationWrite(filepath.Join(InstallStateDir(ctx, project), "publication.json"), encoded)
}

// StagedPlugin is a staged plugin tree that replaces its installed copy, with the members it implies when enabled
type StagedPlugin struct {
	Members      []MemberDir
	Active       bool
	configs      configSnapshot
	target       string
	staged       string
	stagedDigest string
	metadata     string
	oldRecord    any
	newRecord    any
	targetDigest any
}

// Validates a staged plugin update against its install and snapshots everything its publication compares
func newStagedPlugin(ctx context.Context, data map[string]any) (*StagedPlugin, error) {
	configs, err := selectionSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	target, err := rowPath(data, "target")
	if err != nil {
		return nil, err
	}
	staged, err := rowPath(data, "staged")
	if err != nil {
		return nil, err
	}
	s := &StagedPlugin{configs: configs, target: absoluteUnresolved(target), staged: resolvePath(staged)}
	resolvedTarget := resolvePath(s.target)
	if !isRelativeTo(resolvedTarget, resolvePath(DependencyHomeRoot(ctx))) ||
		filepath.Base(filepath.Dir(s.target)) != "plugins" || isSymlink(s.target) ||
		s.staged == resolvedTarget || isRelativeTo(s.staged, resolvedTarget) || isRelativeTo(resolvedTarget, s.staged) {
		return nil, &PyError{Class: "ValueError", Message: "plugin publication paths escape or overlap their home"}
	}
	name := filepath.Base(s.target)
	manifest, err := validateManifest(ctx, s.staged)
	if err != nil {
		return nil, err
	}
	if declared, present := manifest["name"]; present && !jsonEquals(declared, name) {
		return nil, &PyError{Class: "ValueError", Message: "The updated plugin changed its installed name; reinstall it explicitly."}
	}
	if s.stagedDigest, err = TreeDigest(s.staged); err != nil {
		return nil, err
	}
	s.metadata = filepath.Join(filepath.Dir(s.target), ".install-metadata.json")
	previous, present, err := ReadBytesOrNone(s.metadata)
	if err != nil {
		return nil, err
	}
	current, err := metadataRecords(previous, present)
	if err != nil {
		return nil, err
	}
	if s.oldRecord, err = recordOf(data, "old_metadata", name); err != nil {
		return nil, err
	}
	if !jsonEquals(current[name], s.oldRecord) {
		return nil, &PyError{Class: "ValueError", Message: "Plugin install metadata changed while preparing the update; retry."}
	}
	if s.newRecord, err = recordOf(data, "new_metadata", name); err != nil {
		return nil, err
	}
	if s.newRecord == nil {
		return nil, &PyError{Class: "ValueError", Message: "Plugin publication omitted its install metadata record."}
	}
	if s.targetDigest, err = optionalTreeDigest(s.target); err != nil {
		return nil, err
	}
	expected, err := rowItem(data, "target_digest")
	if err != nil {
		return nil, err
	}
	if !jsonEquals(s.targetDigest, expected) {
		return nil, &PyError{Class: "ValueError", Message: "Plugin files changed while preparing the update; retry."}
	}
	selected, err := enabledPluginDirs(ctx, EnabledPluginsOptions{Installing: s.target})
	if err != nil {
		return nil, err
	}
	sources := MemberSources(sameMemberDirs(selected))
	s.Members = []MemberDir{}
	for i := range sources {
		if sources[i].Identity == resolvedTarget {
			s.Active = true
			sources[i].Source = s.staged
		}
	}
	if !s.Active {
		return s, nil
	}
	for _, member := range sources {
		fits, err := admitted(ctx, member.Source, member.Source == s.staged)
		if err != nil {
			return nil, err
		}
		if !fits {
			continue
		}
		candidate, err := declaresPackages(member.Source)
		if err != nil {
			return nil, err
		}
		if candidate {
			s.Members = append(s.Members, member)
		}
	}
	return s, nil
}

// One plugin's record from a metadata mapping of the request, nil when it holds none
func recordOf(data map[string]any, key, name string) (any, error) {
	raw, err := rowItem(data, key)
	if err != nil {
		return nil, err
	}
	records, ok := raw.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
	}
	return records[name], nil
}

// Tree digest of a path that exists, nil otherwise
func optionalTreeDigest(path string) (any, error) {
	if !pathExists(path) {
		return nil, nil
	}
	digest, err := TreeDigest(path)
	if err != nil {
		return nil, err
	}
	return digest, nil
}

// Journals and swaps the staged tree over its install under the metadata lock, refusing any concurrent input edit
func (s *StagedPlugin) Publish(ctx context.Context, project string) error {
	now, err := selectionSnapshot(ctx)
	if err != nil {
		return err
	}
	if !now.equal(s.configs) {
		return &PyError{Class: "ValueError", Message: "Plugin enablement changed while preparing the update; retry."}
	}
	digest, err := TreeDigest(s.staged)
	if err != nil {
		return err
	}
	if digest != s.stagedDigest {
		return &PyError{Class: "ValueError", Message: "Staged plugin files changed while preparing the update; retry."}
	}
	release, err := lockPluginMetadata(s.metadata+".lock", 10*time.Second, "Timed out waiting for the plugin install metadata lock")
	if err != nil {
		return err
	}
	defer release()
	name := filepath.Base(s.target)
	previous, present, err := ReadBytesOrNone(s.metadata)
	if err != nil {
		return err
	}
	metadata, err := metadataRecords(previous, present)
	if err != nil {
		return err
	}
	if !jsonEquals(metadata[name], s.oldRecord) {
		return &PyError{Class: "ValueError", Message: "Plugin install metadata changed while preparing the update; retry."}
	}
	metadata[name] = s.newRecord
	proposed, err := dumpsJSON(metadata, true)
	if err != nil {
		return err
	}
	proposed = append(proposed, '\n')
	current, err := optionalTreeDigest(s.target)
	if err != nil {
		return err
	}
	if !jsonEquals(current, s.targetDigest) {
		return &PyError{Class: "ValueError", Message: "Plugin files changed while preparing the update; retry."}
	}
	backup := filepath.Join(filepath.Dir(s.target), ".previous-"+randomHex32())
	existed := pathExists(s.target)
	factsBefore, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
	if err != nil {
		return err
	}
	row := map[string]any{
		"kind": "plugin", "target": s.target, "backup": backup, "metadata": s.metadata,
		"target_existed": existed, "facts_before": factsBefore,
		"metadata_before": base64Value(configBytes{data: previous, present: present}),
		"metadata_after":  base64.StdEncoding.EncodeToString(proposed),
	}
	if err := writePublicationJournal(ctx, project, row); err != nil {
		return err
	}
	if existed {
		if err := publicationRename(s.target, backup); err != nil {
			return err
		}
	}
	if err := publicationRename(s.staged, s.target); err != nil {
		return err
	}
	return publicationWrite(s.metadata, proposed)
}

// Takes the advisory metadata lock within the timeout, stamping this pid and naming a live holder on timeout
func lockPluginMetadata(path string, timeout time.Duration, message string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	flags := os.O_RDWR | os.O_CREATE | os.O_APPEND
	if runtime.GOOS == "windows" {
		if st, err := os.Stat(path); err != nil || st.Size() == 0 {
			_ = os.WriteFile(path, []byte(" "), 0o666)
		}
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(path, flags, 0o666)
	if err != nil {
		return nil, err
	}
	held, err := LockFD(f, true, max(time.Second, timeout))
	if err != nil {
		f.Close()
		return nil, err
	}
	if !held {
		f.Close()
		if hint := metadataLockHolderHint(path); hint != "" {
			return nil, &PyError{Class: "TimeoutError", Message: message + "; " + hint}
		}
		return nil, &PyError{Class: "TimeoutError", Message: message}
	}
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Sync()
	}
	return func() {
		if err := f.Truncate(0); err == nil && runtime.GOOS == "windows" {
			_, _ = f.Write([]byte(" "))
		}
		_ = f.Sync()
		UnlockFD(f)
		f.Close()
	}, nil
}

// Hint naming the live process whose pid a holder stamped into the lock file, empty for none or a dead one
func metadataLockHolderHint(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text, _ := decodeUTF8Replace(raw, true)
	fields := strings.FieldsFunc(strings.TrimPrefix(text, "\ufeff"), pystr.IsSpace)
	if len(fields) == 0 {
		return ""
	}
	pid, err := strconv.Atoi(strings.ReplaceAll(fields[0], "_", ""))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return ""
	}
	if runtime.GOOS != "windows" {
		if process, err := os.FindProcess(pid); err == nil && errors.Is(process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			return ""
		}
	}
	return fmt.Sprintf("another icebath process (pid %d) probably still holds it", pid)
}

const yamlEOL = `\n?\z`

// Implicit resolvers of the YAML 1.1 round trip editor in the order it tries them
var (
	rtBoolRe  = regexp.MustCompile(`\A(?:y|Y|yes|Yes|YES|n|N|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)` + yamlEOL)
	rtFloatRe = regexp.MustCompile(`\A(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))` + yamlEOL)
	rtIntRe   = regexp.MustCompile(`\A(?:[-+]?0b[0-1_]+|[-+]?0?[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)` + yamlEOL)
	rtMergeRe = regexp.MustCompile(`\A<<` + yamlEOL)
	rtNullRe  = regexp.MustCompile(`\A(?:~|null|Null|NULL|)` + yamlEOL)
	rtTimeRe  = regexp.MustCompile(`\A(?:[0-9]{4}-[0-9][0-9]-[0-9][0-9]|[0-9]{4}-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)` + yamlEOL)
	rtValueRe = regexp.MustCompile(`\A=` + yamlEOL)
)

// Loads config bytes as a comment preserving node tree, nil for an empty document
func loadRoundtrip(data []byte) (*yaml.Node, error) {
	if err := strictUTF8(data); err != nil {
		return nil, &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, &PyError{Class: "YAMLError", Message: err.Error()}
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil
	}
	if err := rtRefuseDuplicates(doc.Content[0]); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Refuses a mapping that repeats a key, as the round trip loader does
func rtRefuseDuplicates(n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		seen := map[string]*yaml.Node{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind == yaml.ScalarNode && rtTag(key) == "merge" {
				continue
			}
			id := rtKey(key)
			if first, ok := seen[id]; ok {
				return &PyError{Class: "DuplicateKeyError", Message: fmt.Sprintf(
					"while constructing a mapping\n  in \"<unicode string>\", line %d, column %d\nfound duplicate key \"%s\" with value \"%s\" (original value: \"%s\")\n  in \"<unicode string>\", line %d, column %d",
					n.Line, n.Column, key.Value, n.Content[i+1].Value, rtValueAfter(n, first), key.Line, key.Column)}
			}
			seen[id] = key
		}
		fallthrough
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, child := range n.Content {
			if err := rtRefuseDuplicates(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// Scalar text of the value a mapping holds under a key node
func rtValueAfter(m *yaml.Node, key *yaml.Node) string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i] == key {
			return m.Content[i+1].Value
		}
	}
	return ""
}

// Root node of a loaded document, nil for none
func rtRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil || len(doc.Content) == 0 {
		return nil
	}
	return rtDeref(doc.Content[0])
}

// Fresh document holding one empty block mapping
func rtNewDocument() *yaml.Node {
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{rtMapping()}}
}

// Fresh empty block mapping
func rtMapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

// Node an alias points at, the node itself otherwise
func rtDeref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// Resolved tag of a node under YAML 1.1, explicit tags kept and plain scalars resolved implicitly
func rtTag(n *yaml.Node) string {
	n = rtDeref(n)
	switch n.Kind {
	case yaml.MappingNode:
		return "map"
	case yaml.SequenceNode:
		return "seq"
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return strings.TrimPrefix(n.Tag, "!!")
	}
	if n.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return "str"
	}
	return rtImplicitTag(n.Value)
}

// Tag the YAML 1.1 implicit resolvers give plain text
func rtImplicitTag(value string) string {
	switch {
	case rtBoolRe.MatchString(value):
		return "bool"
	case rtFloatRe.MatchString(value):
		return "float"
	case rtIntRe.MatchString(value):
		return "int"
	case rtMergeRe.MatchString(value):
		return "merge"
	case rtNullRe.MatchString(value):
		return "null"
	case rtTimeRe.MatchString(value):
		return "timestamp"
	case rtValueRe.MatchString(value):
		return "value"
	}
	return "str"
}

// Identity of a scalar under Python equality, numbers of equal value and booleans sharing one
func rtKey(n *yaml.Node) string {
	n = rtDeref(n)
	if n.Kind != yaml.ScalarNode {
		return fmt.Sprintf("node:%p", n)
	}
	text := strings.ReplaceAll(n.Value, "_", "")
	switch rtTag(n) {
	case "str":
		return "s:" + n.Value
	case "bool":
		if map[string]bool{"y": true, "yes": true, "true": true, "on": true}[strings.ToLower(n.Value)] {
			return "i:1"
		}
		return "i:0"
	case "null":
		return "n"
	case "int":
		if value, ok := rtInt(text); ok {
			return "i:" + value.String()
		}
	case "float":
		if value, ok := rtFloat(text); ok {
			if value.IsInt() {
				integer, _ := value.Int(nil)
				return "i:" + integer.String()
			}
			return "f:" + value.Text('g', -1)
		}
	}
	return rtTag(n) + ":" + n.Value
}

// Integer of YAML 1.1 int text without underscores, binary, octal, hex and base sixty included
func rtInt(text string) (*big.Int, bool) {
	sign := ""
	if strings.HasPrefix(text, "-") || strings.HasPrefix(text, "+") {
		sign, text = text[:1], text[1:]
	}
	value := new(big.Int)
	var ok bool
	switch {
	case strings.Contains(text, ":"):
		ok = true
		for _, part := range strings.Split(text, ":") {
			digit, good := new(big.Int).SetString(part, 10)
			ok = ok && good
			if good {
				value.Mul(value, big.NewInt(60)).Add(value, digit)
			}
		}
	case strings.HasPrefix(text, "0b"):
		_, ok = value.SetString(text[2:], 2)
	case strings.HasPrefix(text, "0x"):
		_, ok = value.SetString(text[2:], 16)
	case len(text) > 1 && text[0] == '0':
		_, ok = value.SetString(text[1:], 8)
	default:
		_, ok = value.SetString(text, 10)
	}
	if sign == "-" {
		value.Neg(value)
	}
	return value, ok
}

// Float of YAML 1.1 float text without underscores, base sixty included, false for infinities and nan
func rtFloat(text string) (*big.Float, bool) {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "inf") || strings.Contains(lower, "nan") {
		return nil, false
	}
	if strings.Contains(text, ":") {
		sign := 1.0
		if strings.HasPrefix(text, "-") {
			sign, text = -1, text[1:]
		}
		text = strings.TrimPrefix(text, "+")
		total := 0.0
		for _, part := range strings.Split(text, ":") {
			digit, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return nil, false
			}
			total = total*60 + digit
		}
		return big.NewFloat(sign * total), true
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, false
	}
	return big.NewFloat(value), true
}

// Reports whether a node is absent or a null scalar
func rtIsNone(n *yaml.Node) bool {
	n = rtDeref(n)
	return n == nil || (n.Kind == yaml.ScalarNode && rtTag(n) == "null")
}

// Python truthiness of a node under YAML 1.1
func rtTruthy(n *yaml.Node) bool {
	n = rtDeref(n)
	if n == nil {
		return false
	}
	switch n.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) > 0
	}
	switch key := rtKey(n); {
	case key == "n", key == "i:0", key == "s:":
		return false
	case strings.HasPrefix(key, "f:"):
		value, ok := rtFloat(strings.ReplaceAll(n.Value, "_", ""))
		return !ok || value.Sign() != 0
	}
	return true
}

// Index of a string key in a mapping, -1 when absent
func rtIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if rtKey(m.Content[i]) == "s:"+key {
			return i
		}
	}
	return -1
}

// Value a mapping holds under a string key, merged mappings consulted after its own keys, nil when absent
func rtGet(m *yaml.Node, key string) *yaml.Node {
	m = rtDeref(m)
	if i := rtIndex(m, key); i >= 0 {
		return rtDeref(m.Content[i+1])
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if rtTag(m.Content[i]) != "merge" {
			continue
		}
		merged := rtDeref(m.Content[i+1])
		sources := []*yaml.Node{merged}
		if merged.Kind == yaml.SequenceNode {
			sources = merged.Content
		}
		for _, source := range sources {
			if source = rtDeref(source); source.Kind == yaml.MappingNode {
				if found := rtGet(source, key); found != nil {
					return found
				}
			}
		}
	}
	return nil
}

// Stores a value under a string key in place, keeping the comments of a replaced value, appending a new key
func rtSet(m *yaml.Node, key string, value *yaml.Node) {
	if i := rtIndex(m, key); i >= 0 {
		old := m.Content[i+1]
		value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
		m.Content[i+1] = value
		return
	}
	m.Content = append(m.Content, rtString(key), value)
}

// Removes a key from a mapping by its Python identity
func rtDeleteKey(m *yaml.Node, id string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if rtKey(m.Content[i]) == id {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// Fresh string scalar, single quoted when YAML 1.1 would read the plain text as something else
func rtString(value string) *yaml.Node {
	if rtImplicitTag(value) != "str" {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.SingleQuotedStyle}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

// Fresh boolean scalar
func rtBool(value bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(value)}
}

// Fresh block sequence of strings
func rtStrings(values []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, value := range values {
		seq.Content = append(seq.Content, rtString(value))
	}
	return seq
}

// Reports whether a node is the string scalar text
func rtIsString(n *yaml.Node, text string) bool {
	return rtKey(n) == "s:"+text
}

// Renders a node tree the way the round trip editor dumps config, two space mappings and offset sequences
func dumpRoundtrip(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Durable writes and renames at each publication boundary, replaced by crash tests to stop the process there
var (
	publicationWrite  = DurableWriteBytes
	publicationRename = os.Rename
)
