package pm

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/linktier"
)

// Python environment operations of the package manager, set at init by its files and nil in a build without it
var (
	syncVenvEntry                  func(ctx context.Context, opts SyncVenvOptions) error
	ensurePythonToolEntry          func(ctx context.Context, opts EnsurePythonToolOptions) (string, error)
	ensureExtraEntry               func(ctx context.Context, name string, opts ExtraOptions) (string, error)
	extraSupportedEntry            func(ctx context.Context, extra string) (bool, error)
	payloadVenvEntry               func(projectRoot string) (string, error)
	selectedVenvEntry              func(ctx context.Context, projectRoot string) (string, error)
	sitePackagesEntry              func(venv string) (string, error)
	pythonToolEntry                func(name, executable, root string) (string, bool, error)
	pinnedPythonVersionEntry       func() (major, minor string, err error)
	venvCommandEntry               func(ctx context.Context, projectRoot, venv string, options []string) ([]string, error)
	baseVenvEntry                  func(projectRoot string) (string, error)
	anchorImportableEntry          func(ctx context.Context, anchor string) bool
	projectPythonEntry             func(ctx context.Context, projectRoot string) (string, error)
	selectedSitePackagesEntry      func(ctx context.Context) (string, error)
	selectedEntryPointsEntry       func(ctx context.Context) ([]EntryPoint, error)
	resolveModuleOriginEntry       func(ctx context.Context, moduleName string) (string, bool)
	distInstalledEntry             func(req, site string) (installed, known bool)
	requirementAppliesHereEntry    func(ctx context.Context, req string) bool
	activationEnvironmentEntry     func(ctx context.Context, projectRoot string) ([]string, error)
	venvIsCurrentEntry             func(ctx context.Context, opts VenvIsCurrentOptions) (bool, error)
	candidateMembersEntry          func(ctx context.Context, extraDirs []string, proposedHome string, enabled, disabled []string) ([]string, error)
	coreRequirementErrorEntry      func(ctx context.Context, declaration *PythonDeclaration) (string, error)
	anchorSitePackagesEntry        func(ctx context.Context, anchor string) []string
	availableEntry                 func(ctx context.Context, extra string) (bool, error)
	collectGenerationsEntry        func(ctx context.Context, project string, minAge time.Duration) ([]string, error)
	collectRuntimeGenerationsEntry func(root string) ([]string, error)
	ensureRuntimeEnvironmentEntry  func(ctx context.Context, opts RuntimeEnvironmentOptions) (string, error)
	extraPythonCommandEntry        func(ctx context.Context, name string) ([]string, error)
	refreshDependenciesEntry       func(ctx context.Context, projectRoot string) (string, error)
	runtimeEnvironmentReadyEntry   func(ctx context.Context) (string, bool, error)
	nemoRelayWheelEntry            func(target string) (Wheel, bool)
	activateDependenciesEntry      func(ctx context.Context, projectRoot string) error
	committedVenvEntry             func(ctx context.Context, projectRoot string) (string, error)
	runtimeRequirementsEntry       func() ([]RuntimeRequirement, error)
)

// Hash pinned wheel of one target
type Wheel struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// One locked package of the runtime set with its marker and every artifact its lock admits
type RuntimeRequirement struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Marker  string   `json:"marker"`
	Files   []Wheel  `json:"files"`
	Targets []string `json:"targets,omitempty"`
}

// Whether the package installs on target, a wheel only package listing the targets its wheels serve
func (r RuntimeRequirement) ServesTarget(target string) bool {
	if len(r.Targets) == 0 {
		return true
	}
	for _, t := range r.Targets {
		if t == target {
			return true
		}
	}
	return false
}

// ManagerAbsentError is a package manager operation refused by a build that leaves the package manager out
type ManagerAbsentError struct{ Line string }

func (e *ManagerAbsentError) Error() string { return e.Line }

// Whether this build links the package manager, false where plugin changes publish with no dependency environment
func ManagerLinked() bool { return syncVenvEntry != nil }

// Refusal naming the build that carries the package manager
func managerAbsent() error {
	return &ManagerAbsentError{Line: linktier.Missing("the tool package manager", linktier.Extras)}
}

// Syncs an application caller's dependency environment, a foreign checkout through its worker, a projectless tree to its runtime set
func SyncVenv(ctx context.Context, opts SyncVenvOptions) error {
	if syncVenvEntry == nil {
		return managerAbsent()
	}
	return syncVenvEntry(ctx, opts)
}

// Installs an isolated Python tool and returns its validated executable
func EnsurePythonTool(ctx context.Context, opts EnsurePythonToolOptions) (string, error) {
	if ensurePythonToolEntry == nil {
		return "", managerAbsent()
	}
	return ensurePythonToolEntry(ctx, opts)
}

// Provisions an extra in this process with progress on the output, the step a user switching its feature on runs
func EnsureExtra(ctx context.Context, name string, opts ExtraOptions) (string, error) {
	if ensureExtraEntry == nil {
		return "", managerAbsent()
	}
	return ensureExtraEntry(ctx, name, opts)
}

// Reports an extra installable on this machine, false in a build that leaves the package manager out
func ExtraSupported(ctx context.Context, extra string) (bool, error) {
	if extraSupportedEntry == nil {
		return false, nil
	}
	return extraSupportedEntry(ctx, extra)
}

// Selected tool entry point of a named side environment, never an installer executable
func PythonTool(name, executable, root string) (string, bool, error) {
	if pythonToolEntry == nil {
		return "", false, managerAbsent()
	}
	return pythonToolEntry(name, executable, root)
}

// Argv prefix running Python inside a venv, a sealed payload's venv first healed onto the payload's store interpreter
func VenvCommand(ctx context.Context, projectRoot, venv string, options []string) ([]string, error) {
	if venvCommandEntry == nil {
		return nil, managerAbsent()
	}
	return venvCommandEntry(ctx, projectRoot, venv, options)
}

// Reports whether an extra environment or the selected environment holds the module
func AnchorImportable(ctx context.Context, anchor string) bool {
	if anchorImportableEntry == nil {
		return false
	}
	return anchorImportableEntry(ctx, anchor)
}

// Whether the environment marker of a requirement admits this machine, true for one no marker probe can judge
func RequirementAppliesHere(ctx context.Context, req string) bool {
	if requirementAppliesHereEntry == nil {
		return true
	}
	return requirementAppliesHereEntry(ctx, req)
}

// Environment of the installed PM packages with the committed site packages and the activation sentinel
func ActivationEnvironment(ctx context.Context, projectRoot string) ([]string, error) {
	if activationEnvironmentEntry == nil {
		return nil, managerAbsent()
	}
	return activationEnvironmentEntry(ctx, projectRoot)
}

// Probes dependency currency without bootstrapping, a cold foreign worker answering not current
func VenvIsCurrent(ctx context.Context, opts VenvIsCurrentOptions) (bool, error) {
	if venvIsCurrentEntry == nil {
		return false, managerAbsent()
	}
	return venvIsCurrentEntry(ctx, opts)
}

// Admitted package declaring trees of a proposed home selection, then the extra dirs that declare packages
func CandidateMembers(ctx context.Context, extraDirs []string, proposedHome string, enabled, disabled []string) ([]string, error) {
	if candidateMembersEntry == nil {
		return nil, managerAbsent()
	}
	return candidateMembersEntry(ctx, extraDirs, proposedHome, enabled, disabled)
}

// Why an applicable icebath-agent requirement of the declaration rejects the running icebath, empty when none does
func CoreRequirementError(ctx context.Context, declaration *PythonDeclaration) (string, error) {
	if coreRequirementErrorEntry == nil {
		return "", managerAbsent()
	}
	return coreRequirementErrorEntry(ctx, declaration)
}

// Site packages of every provisioned extra that names the anchor among its imports
func AnchorSitePackages(ctx context.Context, anchor string) []string {
	if anchorSitePackagesEntry == nil {
		return nil
	}
	return anchorSitePackagesEntry(ctx, anchor)
}

// Reports every anchor of an extra importable here, false in a build that leaves the package manager out
func Available(ctx context.Context, extra string) (bool, error) {
	if availableEntry == nil {
		return false, nil
	}
	return availableEntry(ctx, extra)
}

// Removes unselected lease managed generations older than minAge whose readers have exited
func CollectGenerations(ctx context.Context, project string, minAge time.Duration) ([]string, error) {
	if collectGenerationsEntry == nil {
		return nil, managerAbsent()
	}
	return collectGenerationsEntry(ctx, project, minAge)
}

// Removes runtime generations nothing can run from, skipping while a stage holds the prepare lock
func CollectRuntimeGenerations(root string) ([]string, error) {
	if collectRuntimeGenerationsEntry == nil {
		return nil, managerAbsent()
	}
	return collectRuntimeGenerationsEntry(root)
}

// Relay environment made to hold the pinned set in this process, the step the user switching Relay on runs
func EnsureRuntimeEnvironment(ctx context.Context, opts RuntimeEnvironmentOptions) (string, error) {
	if ensureRuntimeEnvironmentEntry == nil {
		return "", managerAbsent()
	}
	return ensureRuntimeEnvironmentEntry(ctx, opts)
}

// Isolated interpreter command of a provisioned extra, an install error naming the command to run when it is missing
func ExtraPythonCommand(ctx context.Context, name string) ([]string, error) {
	if extraPythonCommandEntry == nil {
		return nil, managerAbsent()
	}
	return extraPythonCommandEntry(ctx, name)
}

// Re-resolves the durable selection after an external update replaced its inputs and reports what happened
func RefreshDependencies(ctx context.Context, projectRoot string) (string, error) {
	if refreshDependenciesEntry == nil {
		return "", managerAbsent()
	}
	return refreshDependenciesEntry(ctx, projectRoot)
}

// Relay environment and whether it holds the pinned set with no provisioning changing it or left unfinished
func RuntimeEnvironmentReady(ctx context.Context) (string, bool, error) {
	if runtimeEnvironmentReadyEntry == nil {
		return "", false, managerAbsent()
	}
	return runtimeEnvironmentReadyEntry(ctx)
}

// Locked nemo-relay wheel of a target, false where the lock carries no wheel for it
func NemoRelayWheel(target string) (Wheel, bool) {
	if nemoRelayWheelEntry == nil {
		return Wheel{}, false
	}
	return nemoRelayWheelEntry(target)
}

// Selects the committed generation at boot, leasing it and pointing child imports and PATH at it
func ActivateDependencies(ctx context.Context, projectRoot string) error {
	if activateDependenciesEntry == nil {
		return managerAbsent()
	}
	return activateDependenciesEntry(ctx, projectRoot)
}

// Locked relay runtime set the binary embeds
func RuntimeRequirements() ([]RuntimeRequirement, error) {
	if runtimeRequirementsEntry == nil {
		return nil, managerAbsent()
	}
	return runtimeRequirementsEntry()
}
