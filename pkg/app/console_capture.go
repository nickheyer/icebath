package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/botmode/dmdelivery"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/builtin/teamspipeline"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/photon"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/wecom"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/pet/generate"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// A user facing console command failure, its message shown as the command's error result
type ConsoleCommandError struct{ Msg string }

func (e *ConsoleCommandError) Error() string { return e.Msg }

// Kinds of the failures a console command reports as its error result instead of ending the console: RuntimeError, the labels errors carry and the Go type names of failures read by name
var consoleFailureKinds = map[string]bool{
	"ActiveSessionRegistryError": true, "AdmissionRefused": true, "AnonChallengeRequired": true, "AnonCredentialDead": true, "ArchiveError": true, "ArtifactPreservationError": true,
	"AttachmentsUnsupportedError": true, "AttemptCanceled": true, "AuditError": true, "AuthError": true, "AuxiliaryClientUnavailable": true, "BackupInProgressError": true,
	"BinaryResolutionError": true, "BrowseBudgetExceeded": true, "BudgetExceeded": true, "BuildFailure": true, "CapturedCDPInvalid": true, "CodexAppServerError": true,
	"CodexAppServerTransportError": true, "CodexThreadResumeError": true, "CommandTokenError": true, "CompressionCheckpointUnavailable": true, "CompressionSessionBusyError": true, "CompressionSessionClosedError": true,
	"ConfigLockedError": true, "ConfigValueError": true, "ConfigWriteError": true, "ConsoleCommandError": true, "CredentialDestinationError": true, "CredentialNotSavedError": true,
	"CredentialPersistError": true, "CronExecutionUnknown": true, "CronSchedulerRegistrationError": true, "CronTickYielded": true, "DeclarativeDriftError": true, "DeletedWalGenerationError": true,
	"DependencyConflict": true, "DownloadError": true, "DownloadPaused": true, "DownloadTransportError": true, "EmptyStreamError": true, "EnvelopeRefusedError": true,
	"EnvironmentConnectionError": true, "FileFetchError": true, "GatewayAuthError": true, "GatewayNotRegisteredError": true, "GatewayUnavailable": true,
	"GenerationError": true, "GitError": true, "HashError": true, "HealthReportUnavailable": true, "HeldRowRewrittenError": true, "HomeInitializationError": true,
	"HostedRoomSessionError": true, "HumanHasControl": true, "IdempotencyConflict": true, "IdentityUnresolved": true, "InstallBusy": true, "InstallError": true,
	"InvalidUserConfigError": true, "KanbanDbCorruptError": true, "KeylessMCPError": true, "LiveConnectionError": true, "LiveTargetHeldError": true, "LiveTargetUnavailableError": true,
	"LostAndFoundError": true, "ManagedExecutionUnknown": true, "ManifestError": true, "MicrosoftGraphAPIError": true, "MicrosoftGraphAuthError": true, "MicrosoftGraphClientError": true,
	"MicrosoftGraphConfigError": true, "MicrosoftGraphTokenError": true, "MultiplexConfigError": true, "NasCronClientError": true, "NoCommittedEnvironment": true, "NodeAbiMismatchError": true,
	"NotImplementedError": true, "OAuthNonInteractiveError": true, "OTLPUnavailable": true, "OperationAlreadyOpen": true, "OwnerNotRunning": true, "OwnerUnavailable": true,
	"OwnerUnconfirmed": true, "OwnershipConflict": true, "PartialDeliveryError": true, "PauseRefused": true, "PeerRunsHTTPError": true, "PetStoreError": true,
	"PhotonDashboardAuthError": true, "PhotonSidecarError": true, "PhotonSidecarStartupError": true, "PinMismatch": true, "PlaneCredentialError": true, "PluginBackendError": true,
	"PluginHostError": true, "PluginHostUnavailable": true, "PluginHostUnsupported": true, "PortalConnectorUnavailable": true, "PortalToolsUnavailable": true, "PreferencesBusy": true,
	"PreflightCompressionTimedOut": true, "ProductBuildError": true, "ProfileIdentitySettlementPending": true, "ProfileOwnershipRetained": true, "ProfileRouteRejected": true, "ProviderCredentialsExhaustedError": true,
	"ProviderNotConfiguredError": true, "ProviderNotSignedIn": true, "ProvisionRefused": true, "PruneError": true, "PruneRowStaleError": true, "PruneRowUnresolvedError": true,
	"PtyUnavailableError": true, "PythonFinalizationError": true, "RateLimited": true, "RecursionError": true, "RefreshFenceTimeout": true, "RelayRouteUnknown": true,
	"ReleaseRefused": true, "ReservationCancelled": true, "ResolutionConflict": true, "RestartSafeScopeUnavailable": true, "RetagError": true, "RetiredGenerationCaptureError": true,
	"RetryRewriteRefused": true, "RuntimeError": true, "RuntimeStartError": true, "S6CommandError": true, "S6Error": true, "ServicePauseFailed": true,
	"SessionActiveWriteGuardError": true, "SessionCompressionInProgressError": true, "SessionLedgeredError": true, "SessionRecoveryError": true, "SessionRecoverySafetyError": true, "SessionRecoverySourceError": true,
	"SessionTurnLeaseLostError": true, "ShimQuarantineError": true, "SignInCopyError": true, "SkillSlashRefused": true, "SshWorkspaceFsError": true, "StageError": true,
	"StaleHeldHistory": true, "StaleLockRow": true, "StateDbReplacedError": true, "StoreError": true, "SubscriptionMutationConflict": true, "SystemScopeRequiresRootError": true,
	"SystemctlUnavailableError": true, "TeamsMeetingArtifactNotFoundError": true, "TeamsMeetingError": true, "TeamsMeetingNotFoundError": true, "TeamsMeetingPermissionError": true, "TeamsPipelineArtifactNotFoundError": true,
	"TeamsPipelineError": true, "TeamsPipelineRetryableError": true, "TeamsPipelineSinkError": true, "TelephonyError": true, "ToolGatewayError": true, "TraceRedactionError": true,
	"TranscriptReadError": true, "TransportError": true, "TurnBusyError": true, "UnscopedSecretError": true, "UntrackableConnectionError": true, "UserSystemdUnavailableError": true,
	"WakeNotAccepted": true, "WakeWordInUse": true, "WeComStreamExpiredError": true, "WorkerPersistenceError": true,
	"_CronSchedulerRegistrationError": true, "_DoctorLoadError": true, "_ExecApprovalDeclined": true, "_ExternalWorkerPostHandoffError": true, "_GateAuthError": true, "_ImpersonationLeak": true,
	"_LoopUnavailable": true, "_NoStreamConsumer": true, "_PollingLifecycleAbort": true, "_PollingStallError": true, "_ProviderAuthResolutionError": true, "_RangeError": true,
	"_Refused": true, "_RelayPluginConfigurationLoadError": true, "_SQLiteBackupRestarted": true, "_SQLiteBackupTimeout": true, "_SQLiteSnapshotError": true, "_SiblingStillBusy": true,
	"_StdioChildExited": true}

// Matchers of the failure types a console command reports as its error result, anywhere in the error chain
var consoleFailureTypes = []func(error) bool{
	chainHas[*plugins.AdmissionRefused], chainHas[*tools.ArchiveError], chainHas[*kanban.ArtifactPreservationError],
	chainHas[*gateway.AttachmentsUnsupportedError], chainHas[*mcp.AttemptCanceled], chainHas[*provider.AuthError],
	chainHas[*provider.AuxiliaryClientUnavailable], chainHas[*backup.BackupInProgressError], chainHas[*provider.BinaryResolutionError],
	chainHas[*state.BrowseBudgetExceeded], chainHas[*pm.BuildFailure], chainHas[*provider.CodexAppServerError],
	chainHas[*provider.CodexAppServerTransportError], chainHas[*provider.CodexThreadResumeError], chainHas[*provider.CommandTokenError],
	chainHas[*provider.CompressionCheckpointUnavailable], chainHas[*state.CompressionSessionBusyError], chainHas[*state.CompressionSessionClosedError],
	chainHas[*config.ConfigLockedError], chainHas[*config.ConfigValueError], chainHas[*config.ConfigWriteError],
	chainHas[*remote.CredentialDestinationError], chainHas[*provider.CredentialNotSavedError], chainHas[*provider.CredentialPersistError],
	chainHas[*cron.CronExecutionUnknown], chainHas[*cron.CronSchedulerRegistrationError], chainHas[*cron.CronTickYielded],
	chainHas[*state.DeclarativeDriftError], chainHas[*state.DeletedWalGenerationError], chainHas[*plugins.DependencyConflict], chainHas[*pm.DownloadError],
	chainHas[*provider.EmptyStreamError], chainHas[*tools.EnvironmentConnectionError], chainHas[*tools.FileFetchError],
	chainHas[*gatewaycli.GatewayNotRegisteredError], chainHas[*generate.GenerationError], chainHas[*state.HeldRowRewrittenError],
	chainHas[*config.HomeInitializationError], chainHas[*botdesktop.HumanHasControl], chainHas[*gateway.IdentityUnresolved],
	chainHas[*botdesktop.InstallBusy], chainHas[*pm.InstallError], chainHas[*config.InvalidUserConfigError], chainHas[*tools.KeylessMCPError],
	chainHas[*state.LiveConnectionError], chainHas[*dmdelivery.LiveTargetHeldError], chainHas[*dmdelivery.LiveTargetUnavailableError],
	chainHas[*state.LiveSystemGuardError], chainHas[*gatewaycli.LoopThreadError], chainHas[*gateway.ManagedExecutionUnknown], chainHas[*pet.ManifestError],
	chainHas[*messagetools.MicrosoftGraphAPIError], chainHas[*messagetools.MicrosoftGraphClientError], chainHas[*messagetools.MicrosoftGraphConfigError],
	chainHas[*messagetools.MicrosoftGraphTokenError], chainHas[*gatewayrun.MultiplexConfigError], chainHas[*pm.NoCommittedEnvironment],
	chainHas[*gatewaycli.NotImplementedError], chainHas[*mcp.OAuthNonInteractiveError], chainHas[*tools.OperationAlreadyOpen],
	chainHas[*gateway.OwnershipConflict], chainHas[*gateway.PartialDeliveryError], chainHas[*selfupdate.PauseRefused], chainHas[*gateway.PeerRunsHTTPError],
	chainHas[*photon.PhotonDashboardAuthError], chainHas[*remote.PlaneCredentialError], chainHas[*browser.PluginBackendError],
	chainHas[*plugins.PluginHostError], chainHas[*plugins.PluginHostUnavailable], chainHas[*plugins.PluginHostUnsupported],
	chainHas[*agent.PreflightCompressionTimedOut], chainHas[*manage.ProfileIdentitySettlementPending], chainHas[*gatewayrun.ProfileOwnershipRetained],
	chainHas[*gateway.ProfileRouteRejected], chainHas[*provider.ProviderCredentialsExhaustedError], chainHas[*provider.ProviderNotConfiguredError],
	chainHas[*state.PruneRowStaleError], chainHas[*state.PruneRowUnresolvedError], chainHas[*mcp.RefreshFenceTimeout], chainHas[*gateway.RelayRouteUnknown],
	chainHas[*pm.ResolutionConflict], chainHas[*tools.RestartSafeScopeUnavailable], chainHas[*state.RetiredGenerationCaptureError],
	chainHas[*gateway.RetryRewriteRefused], chainHas[*state.RewindRuntimeError], chainHas[*config.RuntimeError], chainHas[*kanban.RuntimeError],
	chainHas[*manage.RuntimeError], chainHas[*mcp.RuntimeError], chainHas[*gateway.RuntimeStartError], chainHas[*gatewaycli.S6CommandError],
	chainHas[*selfupdate.ServicePauseFailed], chainHas[*state.SessionActiveWriteGuardError], chainHas[*state.SessionCompressionInProgressError],
	chainHas[*state.SessionLedgeredError], chainHas[*state.SessionTurnLeaseLostError], chainHas[*provider.SignInCopyError], chainHas[*install.StageError],
	chainHas[*agent.StaleHeldHistory], chainHas[*pet.StoreError], chainHas[*provider.StreamEventOrderError],
	chainHas[*webhooksubs.SubscriptionMutationConflict], chainHas[*gateway.SystemScopeRequiresRootError],
	chainHas[*teamspipeline.TeamsMeetingArtifactNotFoundError], chainHas[*teamspipeline.TeamsMeetingError],
	chainHas[*teamspipeline.TeamsMeetingNotFoundError], chainHas[*teamspipeline.TeamsMeetingPermissionError],
	chainHas[*teamspipeline.TeamsPipelineArtifactNotFoundError], chainHas[*teamspipeline.TeamsPipelineError],
	chainHas[*teamspipeline.TeamsPipelineRetryableError], chainHas[*teamspipeline.TeamsPipelineSinkError], chainHas[*gateway.TranscriptReadError],
	chainHas[*remote.TransportError], chainHas[*tools.TurnBusyError], chainHas[*provider.UnscopedSecretError], chainHas[*state.UntrackableConnectionError],
	chainHas[*gateway.UserSystemdUnavailableError], chainHas[*gateway.WakeNotAccepted], chainHas[*media.WakeWordInUse],
	chainHas[*wecom.WeComStreamExpiredError], chainHas[*tools.WorkerPersistenceError],
	chainHas[gateway.SystemctlUnavailableError],
}

// Whether a T is in the chain of err
func chainHas[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// Whether err is a failure a console command reports as its error result: a kind or type of the console failures, or an unsupported operation
func consoleIsCommandFailure(err error) bool {
	if consoleFailureKinds[errorutil.Kind(err)] || errors.Is(err, errors.ErrUnsupported) {
		return true
	}
	for _, matches := range consoleFailureTypes {
		if matches(err) {
			return true
		}
	}
	return false
}

// Process stdout and stderr swapped for pipes while a console command runs
type consoleCapture struct {
	savedOut, savedErr *os.File
	outW, errW         *os.File
	out, errs          bytes.Buffer
	wg                 sync.WaitGroup
	once               sync.Once
}

// Swaps the process stdout and stderr for pipes drained into buffers
func startConsoleCapture() (*consoleCapture, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	c := &consoleCapture{savedOut: os.Stdout, savedErr: os.Stderr, outW: outW, errW: errW}
	c.wg.Add(2)
	drain := func(r *os.File, into *bytes.Buffer) {
		defer c.wg.Done()
		io.Copy(into, r)
		r.Close()
	}
	go drain(outR, &c.out)
	go drain(errR, &c.errs)
	os.Stdout, os.Stderr = outW, errW
	return c, nil
}

// Restores the process streams and returns what the command wrote to stdout and to stderr
func (c *consoleCapture) stop() (string, string) {
	c.once.Do(func() {
		os.Stdout, os.Stderr = c.savedOut, c.savedErr
		c.outW.Close()
		c.errW.Close()
		c.wg.Wait()
	})
	return c.out.String(), c.errs.String()
}

// Runs fn with stdout and stderr captured, a nonzero status or a command failure becoming a console command error
func consoleCaptureOutput(fn func() (int, error)) (string, error) {
	c, err := startConsoleCapture()
	if err != nil {
		return "", err
	}
	var code int
	var raised error
	func() {
		defer func() {
			if r := recover(); r != nil {
				c.stop()
				panic(r)
			}
		}()
		code, raised = fn()
	}()
	stdout, stderr := c.stop()
	return consoleSettle(code, raised, stdout, stderr)
}

// Text of a finished captured command, its exit status and error mapped to the console result
func consoleSettle(code int, raised error, stdout, stderr string) (string, error) {
	message := ""
	if raised != nil {
		var console *ConsoleCommandError
		var exit *commands.ExitError
		switch {
		case errors.As(raised, &console):
			return "", raised
		case errors.As(raised, &exit):
			code = exit.Code
			if exit.Msg != "" && exit.Code == 0 {
				stderr += exit.Msg + "\n"
			} else if exit.Msg != "" {
				message = exit.Msg
			}
		case consoleIsCommandFailure(raised):
			message, code = raised.Error(), 1
		default:
			return "", raised
		}
	}
	text := stdout + stderr
	if code != 0 {
		detail := strings.TrimSpace(message)
		if detail == "" {
			detail = strings.TrimSpace(text)
		}
		if detail == "" {
			detail = fmt.Sprintf("Command exited with status %d", code)
		}
		return "", &ConsoleCommandError{Msg: detail}
	}
	return strings.TrimRightFunc(text, unicode.IsSpace), nil
}
