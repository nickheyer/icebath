package app

import (
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Voice that a search of Settings finds: Speech to text, Text to speech and Wake word, the page of each service and of each
// program of yours under its part, and each listener of the wake word in hand on the listeners. A setting of a service is found on that
// service's page, the most specific page that shows it
func voicePages(kit *tui.Kit, service speechv1connect.SpeechServiceClient, sp *speech.Speech, name string) []tui.Page {
	wake := "Wake word"
	pages := []tui.Page{
		{Title: recognizers.title, About: recognizers.about, Words: "recognition stt", Argument: AtRecognition, Keys: []string{speechconfig.STT + "."}},
		{Title: voices.title, About: voices.about, Words: "speech tts voice", Argument: AtSpeech, Keys: []string{speechconfig.TTS + "."}},
		{Title: wake, About: "Say a phrase to start talking, hands-free", Argument: AtWake, Keys: []string{speechconfig.Wake + "."}},
	}
	// Listing the services looks at what is installed on this computer, which takes time, so they come as last read while read again
	pages = append(pages, tui.Remembered("voice:"+name, func() []tui.Page { return servicePages(kit, service, name) })...)
	for _, e := range sp.Engines() {
		pages = append(pages, tui.Page{Title: e.Title, Trail: []string{wake, "Listener"}, About: e.About, Words: e.Name, Argument: AtWake + ":" + e.Name})
	}
	return pages
}

// Pages of each service and each program of yours under its part, nil when they could not be read
func servicePages(kit *tui.Kit, service speechv1connect.SpeechServiceClient, name string) []tui.Page {
	pages := []tui.Page{}
	if services, err := service.Services(kit.Env.Ctx, &speechv1.ServicesRequest{Profile: name}); err == nil {
		for _, part := range []struct {
			k        kind
			at       string
			services []*speechv1.Service
		}{{recognizers, AtRecognition, services.GetRecognizers()}, {voices, AtSpeech, services.GetVoices()}} {
			for _, s := range part.services {
				page := tui.Page{Title: s.GetTitle(), Trail: []string{part.k.title}, About: s.GetAbout(), Words: s.GetName(), Argument: part.at + ":" + s.GetName()}
				switch {
				case s.GetProgram():
					page.Keys = []string{part.k.table + "." + s.GetName() + "."}
				case s.GetSection() != "":
					page.Keys = []string{s.GetSection() + "."}
				}
				pages = append(pages, page)
			}
		}
		return pages
	}
	return nil
}
