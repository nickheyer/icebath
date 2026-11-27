package speech

import (
	"slices"

	"github.com/nickheyer/icebath/internal/packages"
)

// Environment of a program speech runs for the profile: its own, without the credentials programs may not see, extra on top
func (j *Job) Environ(extra map[string]string) []string {
	env := j.Speech.terminal.ChildEnv(j.Config, extra)
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return out
}

// Variables of the profile's environment by name, those set
func (j *Job) Passed(names []string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		if value, ok := j.Config.LookupEnv(name); ok {
			out["_ICEBATH_FORCE_"+name] = value
		}
	}
	return out
}

// Interpreter of a provisioned Python set of a local engine, the refusal naming the install command when it is not provisioned
func (j *Job) Python(extra string) (string, error) {
	if python, ok := j.Speech.packages.ExtraPython(extra); ok {
		return python, nil
	}
	if reason, known := j.Speech.packages.ExtraUnsupported(extra); known && reason != "" {
		return "", Absent("the " + extra + " engine cannot run on this machine: " + reason)
	}
	return "", Absent("the " + extra + " extra is not provisioned; run `" + packages.InstallHint(extra) + "`")
}

// Whether a provisioned Python set is there
func (j *Job) Provisioned(extra string) bool {
	_, ok := j.Speech.packages.ExtraPython(extra)
	return ok
}
