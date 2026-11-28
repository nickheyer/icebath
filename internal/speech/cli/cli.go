// The voice command: the microphones of the computer the server runs on, by the names the Microphone setting takes, so a person who sets
// it from the command line sees what to set it to
package cli

import (
	"cmp"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := speechv1connect.NewSpeechServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:     "voice",
		GroupID: cli.GroupAbilities,
		Short:   "List the microphones the wake word can listen with",
		Long: "The wake word listens with the microphone its Microphone setting names. microphones lists the microphones of this computer by " +
			"the names that setting takes, with the one in use marked.",
		Example: "  icebath voice microphones\n  icebath config set " + speechconfig.WakeInput + " \"USB Microphone\"",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "icebath profile to act for, instead of the one in use")
	root.AddCommand(&cobra.Command{
		Use:     "microphones",
		Aliases: []string{"mics"},
		Short:   "List the microphones of this computer",
		Long: "Lists the microphones of the computer the icebath server runs on, with the one the wake word listens with marked. The name of " +
			"each is what the Microphone setting takes: icebath config set " + speechconfig.WakeInput + " <name>.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return microphones(cmd, service, profile) },
	})
	return []*cobra.Command{root}
}

// Lists the microphones with the one the Microphone setting of the profile names marked, the default one while it names none
func microphones(cmd *cobra.Command, service speechv1connect.SpeechServiceClient, profile string) error {
	c := cli.NewConsole(cmd)
	listed, err := service.Microphones(cmd.Context(), &speechv1.MicrophonesRequest{})
	if err != nil {
		return err
	}
	if failure := listed.GetError(); failure != "" {
		return c.Fail("No microphone could be listed. " + strs.Full(failure))
	}
	found := listed.GetMicrophones()
	if len(found) == 0 {
		c.Say("No microphone was found on this computer.")
		return nil
	}
	// What the setting holds: nothing or a switch for the default microphone, a device number, or a name
	held := ""
	if voice, err := service.Voice(cmd.Context(), &speechv1.VoiceRequest{Profile: named(cmd, profile)}); err == nil {
		held = strings.TrimSpace(voice.GetWake().GetInputDevice())
	}
	if strings.EqualFold(held, "true") || strings.EqualFold(held, "false") {
		held = ""
	}
	_, numbered := strconv.Atoi(held)
	used, fits := inUse(found, held)
	var rows [][]string
	for i, m := range found {
		mark := ""
		if i == used && (held == "" || numbered != nil) {
			mark = c.Paint("●", cli.Green)
		}
		about := m.GetHost()
		if m.GetDefault() {
			about = strings.TrimPrefix(about+" · the default one", " · ")
		}
		rows = append(rows, []string{mark, m.GetValue(), about})
	}
	c.Table([]string{"", "Name", "Sound system"}, rows)
	switch {
	case held != "" && numbered == nil:
		c.Blank()
		c.Say("The Microphone setting holds the device number " + held + ", which this list can't mark. Set it to a name above to see it marked.")
	case held != "" && fits == 0:
		c.Blank()
		c.Warning("The Microphone setting names “" + held + "”, which is none of these. The wake word can't listen until it names one of them.")
	case held != "" && used < 0:
		c.Blank()
		c.Warning("The Microphone setting names “" + held + "”, which fits " + strconv.Itoa(fits) + " of these. Set it to one whole name above.")
	}
	return nil
}

// Place of the microphone a setting names among those listed, as the listener finds it, and how many the name fits: the default one for
// no name, else the one whose name and sound system hold every word of the setting in order, the one named exactly among several; -1 for
// none and for a name that fits several
func inUse(found []*speechv1.Microphone, held string) (int, int) {
	if held == "" {
		for i, m := range found {
			if m.GetDefault() {
				return i, 1
			}
		}
		return -1, 0
	}
	words := strings.Fields(strings.ToLower(held))
	var hits, exact []int
	for i, m := range found {
		rest, all := strings.ToLower(m.GetName()+", "+m.GetHost()), true
		for _, word := range words {
			at := strings.Index(rest, word)
			if at < 0 {
				all = false
				break
			}
			rest = rest[at+len(word):]
		}
		if !all {
			continue
		}
		hits = append(hits, i)
		if strings.EqualFold(m.GetName(), held) {
			exact = append(exact, i)
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], 1
	case len(exact) == 1:
		return exact[0], len(hits)
	}
	return -1, len(hits)
}

// Profile a command acts for: the one its --profile names, else the one -p picked for icebath, else the one in use
func named(cmd *cobra.Command, flag string) string {
	return cmp.Or(flag, picked.Picked(cmd.Context()))
}
