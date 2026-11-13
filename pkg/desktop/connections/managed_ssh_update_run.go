package connections

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

const (
	defaultRemoteUpdateTimeout    = time.Hour
	defaultRemoteClearanceTimeout = 5 * time.Minute
	defaultRemoteUpdatePoll       = time.Second
	receiptGrace                  = 15 * time.Second
	managedUpdateApplyJoin        = 120 * time.Second
	remoteUpdateExecTimeout       = 30 * time.Second
	darwinDrainUnsupported        = "darwin-drain-unsupported"
)

// ManagedUpdateOwedStep is a post commit step a committed update still owes
type ManagedUpdateOwedStep struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// ManagedUpdateScopeResult is the restore outcome of one drained profile
type ManagedUpdateScopeResult struct {
	Profile  string `json:"profile"`
	Restored bool   `json:"restored"`
	Error    string `json:"error,omitempty"`
}

// ManagedConnectionUpdateResult is the outcome of one managed SSH update transaction
type ManagedConnectionUpdateResult struct {
	ConnectionID  string                     `json:"connectionId"`
	CorrelationID string                     `json:"correlationId"`
	OK            bool                       `json:"ok"`
	UpdateOK      bool                       `json:"updateOk"`
	RestoreOK     bool                       `json:"restoreOk"`
	Outcome       string                     `json:"outcome"`
	ExitCode      *int                       `json:"exitCode"`
	Receipt       any                        `json:"receipt"`
	Scopes        []ManagedUpdateScopeResult `json:"scopes"`
	Owed          []ManagedUpdateOwedStep    `json:"owed,omitempty"`
	Error         string                     `json:"error,omitempty"`
	Message       string                     `json:"message,omitempty"`
	SkipReason    string                     `json:"skipReason,omitempty"`
}

// RemoteUpdateTarget is the remote install a managed update launches and observes
type RemoteUpdateTarget struct {
	SSH            RemoteShell
	Platform       string
	HermesPath     string
	HermesHome     string
	ExecutablePath string
}

// RemoteUpdateObservation is one read of the install marker, launch intent, terminal code and receipt
type RemoteUpdateObservation struct {
	Marker           string
	MarkerPID        int
	LaunchIntent     string
	ExitCode         *int
	Receipt          *jsjson.OrderedObject
	CoordinatorReady *CoordinatorReady
}

// CoordinatorReady is the correlated readiness proof of a Windows update coordinator
type CoordinatorReady struct {
	CorrelationID string
	PID           int
}

// RemoteUpdateProof is the correlated exit code and receipt of a finished remote update
type RemoteUpdateProof struct {
	ExitCode int
	Receipt  *jsjson.OrderedObject
}

var remoteControlRE = regexp.MustCompile("[\x00\r\n]")

func validateRemoteValue(value, label string) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" || remoteControlRE.MatchString(normalized) {
		return "", fmt.Errorf("Managed SSH update %s is invalid.", label)
	}
	return normalized, nil
}

func posixChildPath(home, name string) string { return strings.TrimRight(home, "/") + "/" + name }

func windowsChildPath(home, name string) string { return strings.TrimRight(home, `\/`) + `\` + name }

// Detached POSIX update launcher that proves its intent before the SSH channel returns
func BuildPosixManagedUpdateLaunch(t RemoteUpdateTarget, correlationID string) (string, error) {
	correlation, err := ValidateCorrelationID(correlationID)
	if err != nil {
		return "", err
	}
	home, err := validateRemoteValue(t.HermesHome, "Icebath home")
	if err != nil {
		return "", err
	}
	hermesPath, err := validateRemoteValue(t.HermesPath, "launcher path")
	if err != nil {
		return "", err
	}
	words := map[string]string{}
	for name, p := range map[string]string{
		"home": home, "status": posixChildPath(home, ".update_exit_code."+correlation), "intent": posixChildPath(home, ".update_launch_intent."+correlation),
		"output": posixChildPath(home, "logs/desktop-update-"+correlation+".log"), "launcher": hermesPath,
	} {
		w, err := ExpandRemotePath(p)
		if err != nil {
			return "", err
		}
		words[name] = w
	}
	updateCommand := "env ICEBATH_HOME=" + words["home"] + " " +
		"ICEBATH_UPDATE_CORRELATION_ID=" + Shq(correlation) + " " +
		"ICEBATH_UPDATE_ORIGIN_PROFILE=default " +
		"ICEBATH_UPDATE_ORIGIN_HOME=" + words["home"] + " " +
		"ICEBATH_UPDATE_OUTPUT_PATH=" + words["output"] + " " +
		words["launcher"] + " update --yes"
	inner := `set +e; echo "managed-update start pid=$$ $(date -u +%Y-%m-%dT%H:%M:%SZ)" >&2; ` +
		`if [ -r "/proc/$$/stat" ]; then ` +
		`intent_creation="linux:$(awk '{print $22}' "/proc/$$/stat")"; ` +
		`else intent_creation="darwin:$(ps -o lstart= -p "$$" | sed 's/^ *//')"; fi; ` +
		`intent_tmp=` + words["intent"] + `."$$".tmp; ` +
		`printf '{"correlation":"%s","pid":%s,"creation":"%s"}' ` + Shq(correlation) + ` "$$" "$intent_creation" > "$intent_tmp" && ` +
		`mv -f "$intent_tmp" ` + words["intent"] + ` || exit 70; ` +
		updateCommand + `; rc=$?; ` +
		`echo "managed-update exit rc=$rc $(date -u +%Y-%m-%dT%H:%M:%SZ)" >&2; ` +
		fmt.Sprintf(`if [ "$rc" -ne %d ] && [ ! -e %s ]; then `, UpdateExitIndependentHandoff, words["status"]) +
		`tmp=` + words["status"] + `."$$".tmp; umask 077; ` +
		`printf "%s" "$rc" > "$tmp" && mv -f "$tmp" ` + words["status"] + `; fi; ` +
		`exit "$rc"`
	return `umask 077 && mkdir -p "$(dirname ` + words["output"] + `)" && rm -f ` + words["status"] + " " + words["intent"] + ` && ` +
		`if command -v setsid >/dev/null 2>&1; then ` +
		`setsid sh -c ` + Shq(inner) + ` </dev/null >>` + words["output"] + ` 2>&1 & ` +
		`else nohup sh -c ` + Shq(inner) + ` </dev/null >>` + words["output"] + ` 2>&1 & fi; child=$!; ` +
		`i=0; while [ ! -e ` + words["intent"] + ` ]; do kill -0 "$child" 2>/dev/null || exit 1; ` +
		`i=$((i+1)); [ "$i" -ge 200 ] && exit 1; sleep 0.05; done; printf MANAGED_UPDATE_STARTED`, nil
}

// Windows update launcher, a hidden PowerShell child that proves its intent before the channel returns
func BuildWindowsManagedUpdateLaunch(t RemoteUpdateTarget, correlationID string) (string, error) {
	correlation, err := ValidateCorrelationID(correlationID)
	if err != nil {
		return "", err
	}
	home, err := validateRemoteValue(t.HermesHome, "Icebath home")
	if err != nil {
		return "", err
	}
	hermesPath, err := validateRemoteValue(t.HermesPath, "launcher path")
	if err != nil {
		return "", err
	}
	statusPath := windowsChildPath(home, ".update_exit_code."+correlation)
	readyPath := windowsChildPath(home, ".update_coordinator_ready."+correlation)
	intentPath := windowsChildPath(home, ".update_launch_intent."+correlation)
	outputPath := windowsChildPath(home, `logs\desktop-update-`+correlation+".log")
	wrapper := strings.Join([]string{
		`$ErrorActionPreference="Continue"`,
		"$env:ICEBATH_HOME=" + PsLiteral(home),
		"$env:ICEBATH_UPDATE_CORRELATION_ID=" + PsLiteral(correlation),
		`$env:ICEBATH_UPDATE_ORIGIN_PROFILE="default"`,
		"$env:ICEBATH_UPDATE_ORIGIN_HOME=" + PsLiteral(home),
		"$env:ICEBATH_UPDATE_OUTPUT_PATH=" + PsLiteral(outputPath),
		"$env:ICEBATH_UPDATE_WINDOWS_DETACHED=" + PsLiteral(correlation),
		"$env:ICEBATH_UPDATE_TAURI_OUTCOME_PATH=" + PsLiteral(statusPath),
		"$env:ICEBATH_UPDATE_TAURI_READY_PATH=" + PsLiteral(readyPath),
		"$intentTmp=" + PsLiteral(intentPath) + `+"."+$PID+".tmp"`,
		`$intentCreation="windows:"+[string]([Diagnostics.Process]::GetCurrentProcess().StartTime.ToUniversalTime().ToFileTimeUtc())`,
		"$intentPayload=[ordered]@{correlation=" + PsLiteral(correlation) + ";pid=$PID;creation=$intentCreation}|ConvertTo-Json -Compress",
		`[IO.File]::WriteAllText($intentTmp,$intentPayload,[Text.UTF8Encoding]::new($false))`,
		"Move-Item -LiteralPath $intentTmp -Destination " + PsLiteral(intentPath) + " -Force",
		"& " + PsLiteral(hermesPath) + " update --yes *>> " + PsLiteral(outputPath),
		`$rc=$LASTEXITCODE`,
		"$handoffAccepted=($rc -eq 0 -and (Test-Path -LiteralPath " + PsLiteral(readyPath) + "))",
		fmt.Sprintf("if($rc -ne %d -and -not $handoffAccepted -and -not (Test-Path -LiteralPath %s)){", UpdateExitIndependentHandoff, PsLiteral(statusPath)),
		"  $tmp=" + PsLiteral(statusPath) + `+"."+$PID+".tmp"`,
		`  [IO.File]::WriteAllText($tmp,[string]$rc,[Text.UTF8Encoding]::new($false))`,
		"  Move-Item -LiteralPath $tmp -Destination " + PsLiteral(statusPath) + " -Force",
		`}`,
		`exit $rc`,
	}, ";")
	outer := strings.Join([]string{
		`$ErrorActionPreference="Stop"`,
		"$output=" + PsLiteral(outputPath),
		`New-Item -ItemType Directory -Force -Path (Split-Path -Parent $output)|Out-Null`,
		"Remove-Item -LiteralPath " + PsLiteral(statusPath) + "," + PsLiteral(readyPath) + "," + PsLiteral(intentPath) + " -Force -ErrorAction SilentlyContinue",
		`$args=@("-NoProfile","-NonInteractive","-ExecutionPolicy","Bypass","-EncodedCommand",` + PsLiteral(EncodedPowerShell(wrapper)) + `)`,
		`$child=Start-Process -FilePath "powershell.exe" -ArgumentList $args -WindowStyle Hidden -PassThru`,
		`if(-not $child){throw "managed update launcher did not start"}`,
		`$deadline=[DateTime]::UtcNow.AddSeconds(10)`,
		"while(-not [IO.File]::Exists(" + PsLiteral(intentPath) + `)){if($child.HasExited){throw "managed update launcher exited before intent proof"};if([DateTime]::UtcNow -ge $deadline){throw "managed update launcher intent timed out"};Start-Sleep -Milliseconds 50}`,
		`[ordered]@{started=$true;pid=$child.Id}|ConvertTo-Json -Compress`,
	}, ";")
	return PowerShellCommand(outer), nil
}

// Observation of the install marker, intent, status, receipt and readiness of one correlation
func BuildRemoteUpdateObservationCommand(t RemoteUpdateTarget, correlationID string) (string, error) {
	correlation, err := ValidateCorrelationID(correlationID)
	if err != nil {
		return "", err
	}
	home, err := validateRemoteValue(t.HermesHome, "Icebath home")
	if err != nil {
		return "", err
	}
	if t.Platform == "Windows" {
		launcher, err := validateRemoteValue(t.HermesPath, "launcher path")
		if err != nil {
			return "", err
		}
		return helperCommand(&WindowsRuntime{HermesPath: launcher}, "update-observation", []string{home, correlation}), nil
	}
	return "python3 -c " + Shq(RemoteUpdateObservationPy) + " " + Shq(home) + " " + Shq(correlation), nil
}

func owedStep(v any) *ManagedUpdateOwedStep {
	o := jsObject(toJSONValue(v))
	if o == nil {
		return nil
	}
	return &ManagedUpdateOwedStep{Step: jsString(o["step"]), Reason: jsNullishString(o["reason"])}
}

// Receipt with its followups and user action read as none when an older observer omits them
func withReceiptDebt(receipt *jsjson.OrderedObject) *jsjson.OrderedObject {
	out := receipt.Clone()
	followups := []any{}
	if list, ok := receipt.Get("followups").([]any); ok {
		for _, f := range list {
			if step := owedStep(f); step != nil {
				followups = append(followups, jsjson.NewOrderedObject("step", step.Step, "reason", step.Reason))
			}
		}
	}
	out.Set("followups", followups)
	if step := owedStep(receipt.Get("userAction")); step != nil {
		out.Set("userAction", jsjson.NewOrderedObject("step", step.Step, "reason", step.Reason))
	} else {
		out.SetRaw("userAction", nil)
	}
	return out
}

func receiptDebt(receipt *jsjson.OrderedObject) ([]ManagedUpdateOwedStep, *ManagedUpdateOwedStep) {
	if receipt == nil {
		return nil, nil
	}
	var followups []ManagedUpdateOwedStep
	if list, ok := receipt.Get("followups").([]any); ok {
		for _, f := range list {
			if step := owedStep(f); step != nil {
				followups = append(followups, *step)
			}
		}
	}
	return followups, owedStep(receipt.Get("userAction"))
}

// Outcome copy naming every owed step and its remedy
func managedUpdateMessage(outcome string, restoreOK bool, receipt *jsjson.OrderedObject) ([]ManagedUpdateOwedStep, string) {
	followups, userAction := receiptDebt(receipt)
	owed := append([]ManagedUpdateOwedStep{}, followups...)
	if userAction != nil {
		owed = append(owed, *userAction)
	}
	status := "The remote update transaction could not restore every managed SSH profile."
	if outcome == "updated" {
		status = "Remote Icebath updated"
	} else if restoreOK {
		status = "The remote update failed, but every managed SSH profile was restored."
	}
	if len(owed) == 0 {
		if outcome == "updated" {
			return nil, status + " and every managed SSH profile is ready."
		}
		return nil, status
	}
	var parts, names []string
	for _, step := range followups {
		names = append(names, step.Step)
		if step.Reason != "" {
			parts = append(parts, step.Step+" ("+step.Reason+")")
		} else {
			parts = append(parts, step.Step)
		}
	}
	if userAction != nil {
		parts = append(parts, userAction.Step)
	}
	message := status + " These steps are still owed: "
	if outcome == "updated" {
		message = status + ", but these steps are still owed: "
	}
	message += strings.Join(parts, "; ") + "."
	if len(followups) > 0 {
		message += " Re-run `icebath update` on the remote to finish " + strings.Join(names, ", ") + "."
	}
	if userAction != nil && userAction.Reason != "" {
		message += " " + userAction.Reason
	}
	return owed, message
}

func lastNonEmptyLine(raw string) string {
	text := strings.TrimSpace(strings.TrimPrefix(raw, string(rune(0xfeff))))
	var last string
	for _, line := range crlfSplit.Split(text, -1) {
		if line != "" {
			last = line
		}
	}
	return last
}

func safePID(v any, upper float64) (int, bool) {
	n := ecmaToNumber(v)
	if n != float64(int64(n)) || n <= 0 || n > upper || n > 9007199254740991 {
		return 0, false
	}
	return int(n), true
}

// Validated observation of one correlation, refusing a malformed or foreign answer
func ParseRemoteUpdateObservation(raw, correlationID string) (RemoteUpdateObservation, error) {
	correlation, err := ValidateCorrelationID(correlationID)
	if err != nil {
		return RemoteUpdateObservation{}, err
	}
	line := lastNonEmptyLine(raw)
	if line == "" {
		line = "null"
	}
	parsed, err := jsjson.ParseOrdered(line)
	if err != nil {
		return RemoteUpdateObservation{}, errors.New("Remote update observation was malformed.")
	}
	p := ordered(parsed)
	marker := ostr(p, "marker")
	if p == nil || !containsString([]string{"absent", "dead", "live", "malformed", "unavailable"}, marker) || p.Get("marker") != marker {
		return RemoteUpdateObservation{}, errors.New("Remote update marker state was malformed.")
	}
	intent, _ := p.Get("launchIntent").(string)
	if !containsString([]string{"absent", "dead", "present", "malformed", "unavailable"}, intent) {
		return RemoteUpdateObservation{}, errors.New("Remote update launch intent was malformed.")
	}
	if s, ok := p.Get("exitCode").(string); ok && (s == "malformed" || s == "unavailable") {
		return RemoteUpdateObservation{}, fmt.Errorf("Remote update terminal status was %s.", s)
	}
	out := RemoteUpdateObservation{Marker: marker, LaunchIntent: intent}
	if v := p.Get("exitCode"); v != nil {
		n := ecmaToNumber(v)
		if n != float64(int64(n)) || n < 0 || n > 9007199254740991 {
			return RemoteUpdateObservation{}, errors.New("Remote update terminal status was malformed.")
		}
		code := int(n)
		out.ExitCode = &code
	}
	if v := p.Get("receipt"); v != nil {
		r := ordered(v)
		if _, isString := r.Get("outcome").(string); r == nil || r.Get("correlationId") != correlation || !isString {
			return RemoteUpdateObservation{}, errors.New("Remote update receipt did not match this transaction.")
		}
		out.Receipt = withReceiptDebt(r)
	}
	if v := p.Get("coordinatorReady"); v != nil {
		r := ordered(v)
		pid, ok := safePID(r.Get("pid"), 4294967295)
		if r == nil || r.Get("correlationId") != correlation || !ok {
			return RemoteUpdateObservation{}, errors.New("Remote update coordinator readiness proof was malformed.")
		}
		out.CoordinatorReady = &CoordinatorReady{CorrelationID: correlation, PID: pid}
	}
	if v := p.Get("markerPid"); v != nil {
		pid, ok := safePID(v, 4294967295)
		if !ok {
			return RemoteUpdateObservation{}, errors.New("Remote update marker PID was malformed.")
		}
		out.MarkerPID = pid
	}
	return out, nil
}

// One observation of the remote install for a correlation
func ObserveManagedRemoteUpdate(t RemoteUpdateTarget, correlationID string) (RemoteUpdateObservation, error) {
	command, err := BuildRemoteUpdateObservationCommand(t, correlationID)
	if err != nil {
		return RemoteUpdateObservation{}, err
	}
	raw, err := t.SSH.Exec(command, SSHExecOptions{Timeout: remoteUpdateExecTimeout})
	if err != nil {
		return RemoteUpdateObservation{}, err
	}
	return ParseRemoteUpdateObservation(raw, correlationID)
}

// Whether the install marker is absent or held by a dead owner
func MarkerIsClear(o RemoteUpdateObservation) bool { return o.Marker == "absent" || o.Marker == "dead" }

// Refuses to drain any serve while the install marker is not clear
func AssertManagedUpdatePreflightClear(t RemoteUpdateTarget, correlationID string) error {
	o, err := ObserveManagedRemoteUpdate(t, correlationID)
	if err != nil {
		return err
	}
	if !MarkerIsClear(o) {
		owner := ""
		if o.MarkerPID != 0 {
			owner = fmt.Sprintf(" (PID %d)", o.MarkerPID)
		}
		return fmt.Errorf("The remote install update marker is %s%s; refusing to drain its serves.", o.Marker, owner)
	}
	return nil
}

// Launches the detached remote updater and requires its acknowledgement
func LaunchManagedRemoteUpdate(t RemoteUpdateTarget, correlationID string) error {
	var command string
	var err error
	if t.Platform == "Windows" {
		command, err = BuildWindowsManagedUpdateLaunch(t, correlationID)
	} else {
		command, err = BuildPosixManagedUpdateLaunch(t, correlationID)
	}
	if err != nil {
		return err
	}
	output, err := t.SSH.Exec(command, SSHExecOptions{Timeout: remoteUpdateExecTimeout})
	if err != nil {
		return err
	}
	if t.Platform == "Windows" {
		line := lastNonEmptyLine(output)
		if line == "" {
			line = "null"
		}
		parsed, perr := jsjson.ParseOrdered(line)
		p := ordered(parsed)
		var pidValue any
		if p != nil {
			pidValue = p.Get("pid")
		}
		pid, ok := jsInteger(pidValue)
		if perr != nil || p == nil || p.Get("started") != true || !ok || pid <= 0 {
			return errors.New("Remote Windows update launcher did not acknowledge its detached child.")
		}
		return nil
	}
	if !strings.Contains(output, "MANAGED_UPDATE_STARTED") {
		return errors.New("Remote update launcher did not acknowledge its detached child.")
	}
	return nil
}

// Polls until a correlated terminal result with its receipt and a clear marker, or the update timeout
func WaitForManagedRemoteUpdate(t RemoteUpdateTarget, correlationID string) (RemoteUpdateProof, error) {
	deadline := time.Now().Add(defaultRemoteUpdateTimeout)
	var terminalSeenAt *time.Time
	for {
		o, err := ObserveManagedRemoteUpdate(t, correlationID)
		if err != nil {
			if !time.Now().Before(deadline) {
				return RemoteUpdateProof{}, err
			}
			time.Sleep(defaultRemoteUpdatePoll)
			continue
		}
		if o.Marker == "malformed" || o.Marker == "unavailable" {
			if !time.Now().Before(deadline) {
				return RemoteUpdateProof{}, fmt.Errorf("Remote update marker remained %s through the update timeout; services remain stopped and Desktop recorded them for safe recovery.", o.Marker)
			}
			time.Sleep(defaultRemoteUpdatePoll)
			continue
		}
		if o.CoordinatorReady != nil && o.Marker == "live" && o.MarkerPID != o.CoordinatorReady.PID {
			markerPID := "unknown"
			if o.MarkerPID != 0 {
				markerPID = fmt.Sprint(o.MarkerPID)
			}
			return RemoteUpdateProof{}, fmt.Errorf("Remote update marker PID %s did not match coordinator PID %d.", markerPID, o.CoordinatorReady.PID)
		}
		if o.ExitCode != nil {
			if terminalSeenAt == nil {
				now := time.Now()
				terminalSeenAt = &now
			}
			if o.Receipt != nil && MarkerIsClear(o) {
				return RemoteUpdateProof{ExitCode: *o.ExitCode, Receipt: o.Receipt}, nil
			}
			if o.Receipt == nil && MarkerIsClear(o) && time.Since(*terminalSeenAt) >= receiptGrace {
				return RemoteUpdateProof{}, errors.New("Remote update finished without a correlated durable receipt.")
			}
		}
		if o.CoordinatorReady != nil && o.Receipt != nil && MarkerIsClear(o) {
			code := 1
			if o.Receipt.Get("outcome") == "success" {
				code = 0
			}
			return RemoteUpdateProof{ExitCode: code, Receipt: o.Receipt}, nil
		}
		if !time.Now().Before(deadline) {
			if MarkerIsClear(o) {
				return RemoteUpdateProof{}, errors.New("Remote update did not publish a correlated terminal result before the timeout.")
			}
			return RemoteUpdateProof{}, errors.New("Remote update was still active at the timeout; services remain stopped and Desktop recorded them for safe recovery.")
		}
		time.Sleep(defaultRemoteUpdatePoll)
	}
}

// Preflight, launch, prove the launch, then wait for the correlated result
func ExecuteManagedRemoteUpdate(t RemoteUpdateTarget, correlationID string, onLaunchProved func() error) (RemoteUpdateProof, error) {
	if err := AssertManagedUpdatePreflightClear(t, correlationID); err != nil {
		return RemoteUpdateProof{}, err
	}
	if err := LaunchManagedRemoteUpdate(t, correlationID); err != nil {
		return RemoteUpdateProof{}, err
	}
	if err := onLaunchProved(); err != nil {
		return RemoteUpdateProof{}, err
	}
	return WaitForManagedRemoteUpdate(t, correlationID)
}

// Waits until the install marker is positively clear for this correlation before any serve restarts
func WaitForManagedRemoteClearance(t RemoteUpdateTarget, correlationID string, requireTerminal bool) (RemoteUpdateObservation, error) {
	deadline := time.Now().Add(defaultRemoteClearanceTimeout)
	lastState := "unavailable"
	observedLiveOwner := false
	for {
		o, err := ObserveManagedRemoteUpdate(t, correlationID)
		if err != nil {
			lastState = "unavailable"
		} else {
			lastState = o.Marker
			observedLiveOwner = observedLiveOwner || o.Marker == "live"
			if o.LaunchIntent == "malformed" || o.LaunchIntent == "unavailable" {
				lastState = "launch-intent-" + o.LaunchIntent
			} else if MarkerIsClear(o) && (o.LaunchIntent == "dead" || (!requireTerminal && o.LaunchIntent == "absent") || observedLiveOwner || o.ExitCode != nil || o.Receipt != nil) {
				return o, nil
			}
		}
		if !time.Now().Before(deadline) {
			return RemoteUpdateObservation{}, fmt.Errorf("Could not prove remote update clearance before the recovery timeout (marker: %s); services were not restarted and Desktop retained a durable recovery record.", lastState)
		}
		time.Sleep(defaultRemoteUpdatePoll)
	}
}

// Complete with no restore failures, abandon after success or the attempt budget, else retry next launch
func ManagedSSHRecoveryDisposition(attempts, maxAttempts, restoreFailures int, updateSucceeded bool) string {
	if restoreFailures == 0 {
		return "complete"
	}
	if maxAttempts == 0 {
		maxAttempts = MaxManagedSSHRecoveryAttempts
	}
	if updateSucceeded || attempts >= maxAttempts {
		return "abandon"
	}
	return "retry"
}

// A correlated exit zero is proof unless a receipt reports another outcome
func remoteUpdateSucceeded(exitCode *int, receipt *jsjson.OrderedObject) bool {
	return exitCode != nil && *exitCode == 0 && (receipt == nil || receipt.Get("outcome") == "success")
}

func resultOutcome(updateOK, restoreOK bool) string {
	switch {
	case updateOK && restoreOK:
		return "updated"
	case !updateOK && !restoreOK:
		return "update-and-restore-failed"
	case updateOK:
		return "restore-failed"
	}
	return "update-failed"
}

// ManagedSSHScope is one captured scope a transaction drains and restores
type ManagedSSHScope interface {
	ScopeProfile() string
}

// ManagedUpdateDeps are the drain, update, clearance and restore steps of one transaction
type ManagedUpdateDeps[T ManagedSSHScope] struct {
	ConnectionID          string
	CorrelationID         string
	Scopes                []T
	PreflightRemote       func() error
	DrainScope            func(scope T) error
	UpdateRemote          func() (RemoteUpdateProof, error)
	AwaitRestoreClearance func() error
	CloseTransports       func() error
	RestoreScope          func(scope T) error
	ReleaseGate           func()
	PrepareRecovery       func() error
	CompleteRecovery      func() error
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// Gate, drain every scope, update, wait for marker clearance, restore every scope and lift the gate
func RunManagedSSHUpdate[T ManagedSSHScope](d ManagedUpdateDeps[T]) ManagedConnectionUpdateResult {
	var proof *RemoteUpdateProof
	updateError, restorationBlocked := "", ""
	recoveryComplete, recoveryPrepared, restoreNeedsMarkerFence := true, false, false
	var restoreResults []ManagedUpdateScopeResult
	candidates := map[int]bool{}
	func() {
		if err := d.PreflightRemote(); err != nil {
			updateError = err.Error()
			return
		}
		if err := d.PrepareRecovery(); err != nil {
			updateError = err.Error()
			return
		}
		recoveryPrepared = true
		var drainErrors []string
		restoreNeedsMarkerFence = len(d.Scopes) > 0
		for i, scope := range d.Scopes {
			candidates[i] = true
			if err := d.DrainScope(scope); err != nil {
				drainErrors = append(drainErrors, scope.ScopeProfile()+": "+err.Error())
			}
		}
		if len(drainErrors) > 0 {
			updateError = "Could not safely drain every managed SSH scope (" + strings.Join(drainErrors, "; ") + ")."
			return
		}
		restoreNeedsMarkerFence = true
		p, err := d.UpdateRemote()
		if err != nil {
			updateError = err.Error()
			return
		}
		proof = &p
	}()
	if restoreNeedsMarkerFence {
		if err := d.AwaitRestoreClearance(); err != nil {
			restorationBlocked = err.Error()
			updateError = joinNonEmpty(updateError, restorationBlocked)
		}
	}
	if err := d.CloseTransports(); err != nil && updateError == "" {
		updateError = "Could not close the drained SSH transport: " + err.Error()
	}
	for i, scope := range d.Scopes {
		switch {
		case !candidates[i]:
			restoreResults = append(restoreResults, ManagedUpdateScopeResult{Profile: scope.ScopeProfile(), Restored: true})
		case restorationBlocked != "":
			restoreResults = append(restoreResults, ManagedUpdateScopeResult{Profile: scope.ScopeProfile(), Error: restorationBlocked})
		default:
			if err := d.RestoreScope(scope); err != nil {
				restoreResults = append(restoreResults, ManagedUpdateScopeResult{Profile: scope.ScopeProfile(), Error: err.Error()})
			} else {
				restoreResults = append(restoreResults, ManagedUpdateScopeResult{Profile: scope.ScopeProfile(), Restored: true})
			}
		}
	}
	func() {
		defer d.ReleaseGate()
		failures := 0
		for _, r := range restoreResults {
			if !r.Restored {
				failures++
			}
		}
		var exitCode *int
		var receipt *jsjson.OrderedObject
		if proof != nil {
			exitCode, receipt = &proof.ExitCode, proof.Receipt
		}
		disposition := ManagedSSHRecoveryDisposition(0, 0, failures, remoteUpdateSucceeded(exitCode, receipt))
		if recoveryPrepared && restorationBlocked == "" && disposition != "retry" {
			if err := d.CompleteRecovery(); err != nil {
				recoveryComplete = false
				if updateError == "" {
					updateError = "Could not clear the managed SSH recovery record: " + err.Error()
				}
			}
		}
	}()
	updateOK := proof != nil && proof.ExitCode == 0 && proof.Receipt.Get("outcome") == "success"
	restoreOK := recoveryComplete
	var restoreErrors []string
	for _, r := range restoreResults {
		if !r.Restored {
			restoreOK = false
			restoreErrors = append(restoreErrors, r.Error)
		}
	}
	outcome := resultOutcome(updateOK, restoreOK)
	result := ManagedConnectionUpdateResult{
		ConnectionID: d.ConnectionID, CorrelationID: d.CorrelationID, OK: updateOK && restoreOK, UpdateOK: updateOK, RestoreOK: restoreOK,
		Outcome: outcome, Scopes: restoreResults, Error: joinNonEmpty(append([]string{updateError}, restoreErrors...)...),
	}
	if result.Scopes == nil {
		result.Scopes = []ManagedUpdateScopeResult{}
	}
	var receipt *jsjson.OrderedObject
	if proof != nil {
		code := proof.ExitCode
		result.ExitCode, result.Receipt, receipt = &code, proof.Receipt, proof.Receipt
	}
	result.Owed, result.Message = managedUpdateMessage(outcome, restoreOK, receipt)
	return result
}

// Refused transaction result, a skip when it carries a reason
func RefusedManagedSSHUpdate(connectionID, correlationID, message, skipReason string) ManagedConnectionUpdateResult {
	if message == "" {
		message = "This connection is not managed by Desktop SSH."
	}
	return ManagedConnectionUpdateResult{ConnectionID: connectionID, CorrelationID: correlationID, RestoreOK: true, Outcome: "refused",
		Scopes: []ManagedUpdateScopeResult{}, Error: message, Message: message, SkipReason: skipReason}
}

// Skip reason and copy when a scope runs on a macOS remote whose serve cannot be stopped safely
func ManagedSSHDrainBlocker(scopes []*CapturedSSHScope) (string, string) {
	var blocked []string
	for _, scope := range scopes {
		if scope.State != nil && scope.State.RemotePlatform == "Darwin" {
			blocked = append(blocked, scope.Profile)
		}
	}
	if len(blocked) == 0 {
		return "", ""
	}
	return darwinDrainUnsupported, "Skipped: Desktop cannot safely stop its running Icebath serve on this macOS remote (" + strings.Join(blocked, ", ") + "). " +
		"Disconnect it, or run `icebath update` on the remote, then retry."
}

// Update all row of a managed SSH connection, a deliberate refusal reported as a skip
func ManagedSSHUpdateAllRow(base map[string]any, result ManagedConnectionUpdateResult) map[string]any {
	if result.SkipReason != "" {
		return jsSpread(base, map[string]any{"ok": false, "skipped": true, "reason": result.SkipReason, "detail": result.Message, "managed": result})
	}
	row := jsSpread(base, map[string]any{"ok": result.OK, "detail": result.Message, "managed": result})
	if !result.OK {
		row["error"] = result.Error
		if row["error"] == "" {
			row["error"] = result.Outcome
		}
	}
	return row
}

// Waits for every registered operation, rereading after each batch, false once a positive bound passes with some pending
func WaitForManagedUpdateOperations(operations func() []<-chan struct{}, timeout time.Duration) bool {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		pending := operations()
		if len(pending) == 0 {
			return true
		}
		done := make(chan struct{})
		go func() {
			for _, p := range pending {
				<-p
			}
			close(done)
		}()
		if timeout <= 0 {
			<-done
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		select {
		case <-done:
		case <-time.After(remaining):
			return false
		}
	}
}

// Retries a journaled recovery once the marker clears, recording a failed attempt or completing the journal
func RecoverManagedSSHScopes(scopes []ManagedSSHRecoveryScope, attempts int, awaitClearance func() (RemoteUpdateObservation, error), afterClearance func() error,
	restoreScope func(ManagedSSHRecoveryScope) error, completeRecovery func() error, recordFailedAttempt func(int) error) (int, string, []error, error) {
	clearance, err := awaitClearance()
	if err != nil {
		return 0, "", nil, err
	}
	if afterClearance != nil {
		if err := afterClearance(); err != nil {
			return 0, "", nil, err
		}
	}
	results := make([]error, len(scopes))
	var wg sync.WaitGroup
	for i, scope := range scopes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = restoreScope(scope)
		}()
	}
	wg.Wait()
	failures := 0
	for _, r := range results {
		if r != nil {
			failures++
		}
	}
	if failures > 0 {
		attempts++
	}
	disposition := ManagedSSHRecoveryDisposition(attempts, 0, failures, remoteUpdateSucceeded(clearance.ExitCode, clearance.Receipt))
	if disposition == "retry" {
		if err := recordFailedAttempt(attempts); err != nil {
			return attempts, disposition, results, err
		}
	} else if err := completeRecovery(); err != nil {
		return attempts, disposition, results, err
	}
	return attempts, disposition, results, nil
}
