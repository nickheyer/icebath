package speech

import (
	"context"
	"os/exec"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// State words of a service, the same on every page and in every sentence; Needs and a program, such as Needs espeak-ng, besides
const (
	InUse        = "In use"
	Ready        = "Ready"
	Free         = "Free"
	NotInstalled = "Not installed"
	NeedsKey     = "Needs an API key"
	NeedsSignIn  = "Needs a sign-in"
	CantRun      = "Can't run here"
)

// Kind of thing that fixes what keeps a part of voice from working
type Lack int

const (
	// Nothing a page of Voice does
	LacksNothing Lack = iota
	// Its Python set, which its page installs
	LacksInstall
	// An API key, which its page takes
	LacksKey
	// A sign-in at a provider
	LacksSignIn
	// A program of the system, which Install says how to install
	LacksProgram
	// A build for this computer, so another service is needed
	LacksBuild
	// A setting of its own, set on its page
	LacksSetup
	// The switch that turns it on
	LacksSwitch
	// A service: the one chosen does not exist, or none is chosen and none is ready
	LacksService
)

// What keeps a part of voice from working: what is wrong in a person's words, and the kind of thing that fixes it
type Hindrance struct {
	// A clause, such as Local Whisper is not installed
	Cause string
	Lack  Lack
	// Service it is about, empty for none in particular
	Service string
	// Command that installs the program of the system it lacks
	Install string
}

// Cause as a sentence of its own
func (h *Hindrance) Sentence() string {
	if h == nil || h.Cause == "" {
		return ""
	}
	return strs.Capital(h.Cause) + "."
}

// What to do about it, for a sentence that can't open the place it is done in and names that place instead
func (h *Hindrance) Remedy(place string) string {
	switch h.Lack {
	case LacksInstall:
		return "Install it in " + place + "."
	case LacksKey:
		return "Give it an API key in " + place + "."
	case LacksSignIn:
		return "Sign in to it from " + place + "."
	case LacksProgram:
		return "Install it with " + h.Install + "."
	case LacksBuild:
		return "Pick another service in " + place + "."
	case LacksSetup:
		return "Set it up in " + place + "."
	case LacksSwitch:
		return "Turn it on in " + place + "."
	case LacksService:
		return "Set one up in " + place + "."
	}
	return ""
}

// How a voice or recognizer stands for a profile: the state its row shows and what keeps it from running
type Standing struct {
	State string
	// Nil while nothing keeps it from running
	Hindrance *Hindrance
	// What the service says it lacks in its own words, empty when it lacks nothing
	Missing string
}

// A voice or recognizer as its standing reads it: its setup and why it says it can't run now
type offer struct {
	name, title string
	local       bool
	setup       Setup
	missing     func() string
}

func (v Voice) offer(j *Job) offer {
	return offer{name: v.Name, title: v.Title, local: v.Local, setup: v.Setup, missing: func() string {
		if v.ready(j) {
			return ""
		}
		return v.Title + " can't speak with the settings it has now."
	}}
}

func (r Recognizer) offer(ctx context.Context, j *Job) offer {
	return offer{name: r.Name, title: r.Title, local: r.Local, setup: r.Setup, missing: func() string { return r.missing(ctx, j) }}
}

// How the voice stands for the profile
func (j *Job) VoiceStanding(v Voice) Standing {
	return j.standing(v.offer(j))
}

// How the recognizer stands for the profile
func (j *Job) RecognizerStanding(ctx context.Context, r Recognizer) Standing {
	return j.standing(r.offer(ctx, j))
}

// The one answer to how a service stands: what its setup lacks first, a Python set not installed or a program of the system, then whether
// it says it can run, and when it can't, what its setup says it takes: an API key, a sign-in, what it names it needs, else a build for this
// computer
func (j *Job) standing(o offer) Standing {
	hinder := func(state, cause string, lack Lack) Standing {
		return Standing{State: state, Hindrance: &Hindrance{Cause: o.title + " " + cause, Lack: lack, Service: o.name}}
	}
	setup := o.setup
	if setup.Extra != "" && !j.Provisioned(setup.Extra) {
		if reason, _ := j.Speech.packages.ExtraUnsupported(setup.Extra); reason != "" {
			s := hinder(CantRun, "can't run on this computer", LacksBuild)
			s.Missing = reason
			return s
		}
		return hinder(NotInstalled, "is not installed", LacksInstall)
	}
	if absent := AbsentPrograms(setup.Programs); len(absent) > 0 {
		names := make([]string, len(absent))
		for i, p := range absent {
			names[i] = p.Name
		}
		said := strs.And(names)
		s := hinder("Needs "+said, "needs "+said, LacksProgram)
		s.Hindrance.Install = absent[0].Install
		return s
	}
	missing := o.missing()
	if missing == "" {
		if len(setup.Keys) == 0 && setup.SignIn == "" && free(setup.Badge) {
			return Standing{State: Free}
		}
		return Standing{State: Ready}
	}
	var s Standing
	switch {
	case len(setup.Keys) > 0:
		s = hinder(NeedsKey, "needs an API key", LacksKey)
	case setup.SignIn != "":
		s = hinder(NeedsSignIn, "needs a sign-in", LacksSignIn)
	case setup.Needs != "":
		s = hinder("Needs "+setup.Needs, "needs "+setup.Needs, LacksSetup)
	default:
		s = hinder(CantRun, "can't run on this computer", LacksBuild)
	}
	s.Missing = missing
	return s
}

// Whether a badge says the service costs nothing
func free(badge string) bool {
	return strings.Contains(badge, "free") && !strings.Contains(badge, "tier")
}

// A program of the system a service needs, and the command that installs it
type Program struct {
	Name    string
	Install string
}

// Other programs that do the work of one a service names
var stands = map[string][]string{"espeak-ng": {"espeak"}}

// Programs among those named that this machine lacks, each with the command that installs it
func AbsentPrograms(programs []string) []Program {
	var out []Program
	for _, program := range programs {
		found := false
		for _, name := range append([]string{program}, stands[program]...) {
			if _, err := exec.LookPath(name); err == nil {
				found = true
				break
			}
		}
		if !found {
			out = append(out, Program{Name: program, Install: systemInstall(program)})
		}
	}
	return out
}

// Command that installs a program from the system's packages
func systemInstall(program string) string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install " + program
	case "windows":
		return "choco install " + program + " -y"
	}
	return "sudo apt install -y " + program
}

// The one rule of a value kept both for one service and for every service: the service's own while it holds one, else the one for every
// service. A service's own value at its zero is what its setting's zero words call Same as Voice
func Either[T comparable](own, general T) T {
	var zero T
	if own != zero {
		return own
	}
	return general
}
