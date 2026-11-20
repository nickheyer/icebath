//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Probes of the admission layers, the marker read, the sealed tree steward, Termux and the install method
var (
	admissionReadProvenance = func() *ImageProvenance { return ReadImageProvenance("") }
	admissionSealedSteward  = SealedSteward
	admissionIsTermux       = hostinfo.IsTermux
	admissionInstallMethod  = config.DetectInstallMethod
)

// Why an in place update is refused, the full text to print and the one line command to run instead
type UpdateRefusal struct {
	Code, Message, UpdateCommand string
}

// Install method whose CLI command remediates a tree sealed by each steward
var stewardUpdateMethods = map[string]string{StewardDocker: "docker", StewardNix: "nix", StewardAptTermux: "apt"}

// Refusal for an install method, the given message of its command, else docker's text or the bare command
func refusal(code, method string, message func(command string) string) *UpdateRefusal {
	command := config.RecommendedUpdateCommandForMethod(method)
	text := command
	switch {
	case message != nil:
		text = message(command)
	case method == "docker":
		text = config.FormatDockerUpdateMessage()
	}
	return &UpdateRefusal{Code: code, Message: text, UpdateCommand: command}
}

// Refusal for a tree sealed by a steward, its own instructions when no CLI command remediates it
func stewardRefusal(steward string) *UpdateRefusal {
	method, known := stewardUpdateMethods[steward]
	if method == "docker" {
		return refusal(steward, method, nil)
	}
	if known {
		return refusal(steward, method, func(string) string { return StewardUpdateMessage(steward) })
	}
	return &UpdateRefusal{Code: steward, Message: StewardUpdateMessage(steward), UpdateCommand: "update via " + steward}
}

// Runs one admission layer, a failure logged at debug and treated as no verdict
func admissionLayer(message string, layer func() (*UpdateRefusal, error)) (verdict *UpdateRefusal) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf(message, r))
			verdict = nil
		}
	}()
	verdict, err := layer()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf(message, launchErrorText(err)))
		return nil
	}
	return verdict
}

// The refusal of an install that must not update in place, nil for a git checkout or a mutable tree
func evaluateUpdateAdmission(ctx context.Context, projectRoot string) (*UpdateRefusal, error) {
	if IsCommitBuild(projectRoot) {
		return &UpdateRefusal{Code: "commit-build", Message: CommitBuildUpdateMessage}, nil
	}
	if verdict := admissionLayer("Image provenance check failed (using heuristics): %s", func() (*UpdateRefusal, error) {
		provenance := admissionReadProvenance()
		if provenance == nil {
			return nil, nil
		}
		if !provenance.Valid {
			defect := "None"
			if provenance.Error != nil {
				defect = *provenance.Error
			}
			return refusal("image-marker-invalid", "docker", func(command string) string {
				return "✗ This install is image-managed, but its provenance " +
					"marker is invalid (" + defect + ").\n" +
					"  In-place update is disabled. Update by pulling a " +
					"new image:\n    " + command
			}), nil
		}
		return refusal("image-marker", provenance.Manager, nil), nil
	}); verdict != nil {
		return verdict, nil
	}
	if verdict := admissionLayer("Steward admission check failed: %s", func() (*UpdateRefusal, error) {
		steward := admissionSealedSteward(projectRoot)
		if steward == nil {
			if admissionIsTermux() {
				return &UpdateRefusal{Code: StewardAptTermux, Message: SourceOnTermuxUpdateMessage, UpdateCommand: SourceOnTermuxUpdateCommand}, nil
			}
			return nil, nil
		}
		if *steward != "unknown" {
			return stewardRefusal(*steward), nil
		}
		return nil, nil
	}); verdict != nil {
		return verdict, nil
	}
	if verdict := admissionLayer("Install-method admission check failed: %s", func() (*UpdateRefusal, error) {
		method, err := admissionInstallMethod(ctx, home.Dir(ctx), projectRoot)
		if err != nil {
			return nil, err
		}
		if method == "docker" {
			return refusal("docker", method, nil), nil
		}
		if config.IsNixInstallMethod(method) || method == "apt" {
			code := "nix"
			if method == "apt" {
				code = "apt"
			}
			return refusal(code, method, nil), nil
		}
		if config.IsPackageManagerInstallMethod(method) {
			return refusal(method, method, func(string) string { return StewardUpdateMessage(method) }), nil
		}
		return nil, nil
	}); verdict != nil {
		return verdict, nil
	}
	return unmanagedTreeRefusal(projectRoot), nil
}

// Refusal for a tree the installer did not create, telling a source checkout how a checkout is updated
func unmanagedTreeRefusal(projectRoot string) *UpdateRefusal {
	if install.InstallerCreated(projectRoot) {
		return nil
	}
	lead := "✗ " + install.UnmanagedTreeDescription(projectRoot) + ", so icebath update leaves it alone.\n"
	if install.IsSourceCheckout(projectRoot) {
		steps := install.CheckoutUpdateCommands()
		return &UpdateRefusal{Code: "source-checkout", UpdateCommand: strings.Join(steps, " && "),
			Message: lead + "  Update a checkout from inside it:\n    cd " + projectRoot + "\n    " + strings.Join(steps, "\n    ")}
	}
	return &UpdateRefusal{Code: "not-installer-tree", UpdateCommand: install.InstallerCommand(),
		Message: lead + "  Install icebath with the installer to get updates:\n    " + install.InstallerCommand()}
}

// Writes the minimal refused receipt of a blocked update attempt, never failing the command
func recordRefusalReceipt(ctx context.Context, refusal *UpdateRefusal) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("Could not record refusal receipt: %v", r))
		}
	}()
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	detail := fmt.Sprintf("not updatable in place (%s)", refusal.Code)
	if refusal.UpdateCommand != "" {
		detail += "; use: " + refusal.UpdateCommand
	} else {
		detail += "; " + refusal.Message
	}
	updaterecord.RecordStep(ctx, "admission", false, detail)
	updaterecord.FinalizeUpdateReceipt(ctx, "refused", nil, refusal.Code)
}
