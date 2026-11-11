package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ConsoleCommandError is a user facing console command failure, a RuntimeError subclass in hermes
type ConsoleCommandError struct{ Msg string }

func (e *ConsoleCommandError) Error() string { return e.Msg }

// Python class the console failure stands for
func (e *ConsoleCommandError) PyTypeName() string { return "ConsoleCommandError" }

// Python exception classes deriving from RuntimeError across hermes, the ones a console handler turns into an error
var consoleRuntimeErrorClasses = map[string]bool{
	"ActiveSessionRegistryError": true, "AdmissionRefused": true, "AnonChallengeRequired": true, "AnonCredentialDead": true, "ArchiveError": true, "ArtifactPreservationError": true,
	"AttachmentsUnsupportedError": true, "AttemptCanceled": true, "AuditError": true, "AuthError": true, "AuxiliaryClientUnavailable": true, "BackupInProgressError": true,
	"BinaryResolutionError": true, "BrowseBudgetExceeded": true, "BudgetExceeded": true, "BuildFailure": true, "CapturedCDPInvalid": true, "CodexAppServerError": true,
	"CodexAppServerTransportError": true, "CodexThreadResumeError": true, "CommandTokenError": true, "CompressionCheckpointUnavailable": true, "CompressionSessionBusyError": true, "CompressionSessionClosedError": true,
	"ConfigLockedError": true, "ConfigValueError": true, "ConfigWriteError": true, "ConsoleCommandError": true, "CredentialDestinationError": true, "CredentialNotSavedError": true,
	"CredentialPersistError": true, "CronExecutionUnknown": true, "CronSchedulerRegistrationError": true, "CronTickYielded": true, "DeclarativeDriftError": true, "DeletedWalGenerationError": true,
	"DependencyConflict": true, "DownloadError": true, "DownloadPaused": true, "DownloadTransportError": true, "EmptyStreamError": true, "EnvelopeRefusedError": true,
	"EnvironmentConnectionError": true, "FeatureProbeError": true, "FileFetchError": true, "GatewayAuthError": true, "GatewayNotRegisteredError": true, "GatewayUnavailable": true,
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
	"WakeNotAccepted": true, "WakeWordInUse": true, "WeComStreamExpiredError": true, "WebappAccessError": true, "WebappBuildError": true, "WorkerPersistenceError": true,
	"_CronSchedulerRegistrationError": true, "_DoctorLoadError": true, "_ExecApprovalDeclined": true, "_ExternalWorkerPostHandoffError": true, "_GateAuthError": true, "_ImpersonationLeak": true,
	"_LoopUnavailable": true, "_NoStreamConsumer": true, "_PollingLifecycleAbort": true, "_PollingStallError": true, "_ProviderAuthResolutionError": true, "_RangeError": true,
	"_Refused": true, "_RelayPluginConfigurationLoadError": true, "_SQLiteBackupRestarted": true, "_SQLiteBackupTimeout": true, "_SQLiteSnapshotError": true, "_SiblingStillBusy": true,
	"_StdioChildExited": true}

// Whether err stands for a Python RuntimeError subclass by its class name
func consoleIsRuntimeError(err error) bool {
	return consoleRuntimeErrorClasses[pyrepr.ExceptionName(err)]
}

// Process stdout and stderr swapped for pipes, the redirect_stdout and redirect_stderr pair of a console command
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

// Runs fn with stdout and stderr captured, a nonzero status or RuntimeError becoming a console command error
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

// Text of a finished captured command, its exit status and exception mapped the way _capture_output maps them
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
		case consoleIsRuntimeError(raised):
			message, code = pyrepr.ExceptionMessage(raised), 1
		default:
			return "", raised
		}
	}
	text := stdout + stderr
	if code != 0 {
		detail := pystr.Strip(message)
		if detail == "" {
			detail = pystr.Strip(text)
		}
		if detail == "" {
			detail = fmt.Sprintf("Command exited with status %d", code)
		}
		return "", &ConsoleCommandError{Msg: detail}
	}
	return pystr.Rstrip(text), nil
}
