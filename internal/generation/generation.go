package generation

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// What a service makes
type Kind string

const (
	Image Kind = "image"
	Video Kind = "video"
)

// Setting that chooses the service of the kind
func (k Kind) choice() string {
	if k == Video {
		return generationconfig.VideoProvider
	}
	return generationconfig.ImageProvider
}

// Setting that names the model of the kind
func (k Kind) model() string {
	if k == Video {
		return generationconfig.VideoModel
	}
	return generationconfig.ImageModel
}

// A service that draws pictures, makes videos or both, what a generation plugin registers
type Service struct {
	// As image_gen.provider and video_gen.provider name it
	Name  string
	Title string
	// Whether it can be used with the settings, its key or sign-in there
	Ready func(cfg *config.Config) bool
	// How a person sets it up
	Setup  Setup
	Images *Images
	Videos *Videos
}

// What a person sets up to use a service
type Setup struct {
	// Price and needs in a few words, such as paid
	Badge string
	// Setting of the key it asks for, empty when it asks for none
	Key string
	// What to ask for the key and where to get one
	Prompt, Link string
	// Kind of model provider whose key or sign-in it uses, connected in /providers; empty for none
	Provider string
}

// Pictures of a service
type Images struct {
	// What it draws and how, a line for the picker
	About string
	// Settings of the service for pictures, such as its model
	Settings []string
	// Setting of its own that names the model it draws with, where a model picked for it is kept; Image model when empty
	Model string
	// Models it draws with, as the picker lists them, and the one it uses when no setting names one
	Models  func(ctx context.Context, cfg *config.Config) []Model
	Default func(ctx context.Context, cfg *config.Config) string
	// What the model it would draw with takes, which shapes the arguments of image_generate
	Takes    func(cfg *config.Config) ImageTakes
	Generate func(ctx context.Context, job *Job, request ImageRequest) Result
}

// Videos of a service
type Videos struct {
	About    string
	Settings []string
	// Setting of its own that names the model it makes videos with, where a model picked for it is kept; Video model when empty
	Model   string
	Models  func(ctx context.Context, cfg *config.Config) []Model
	Default func(ctx context.Context, cfg *config.Config) string
	// What the model it would make videos with takes, which shapes the arguments of video_generate
	Takes    func(cfg *config.Config) VideoTakes
	Generate func(ctx context.Context, job *Job, request VideoRequest) Result
}

// One model of a service as the pickers list it
type Model struct {
	ID        string
	Title     string
	Speed     string
	Strengths string
	Price     string
	// What it makes videos from, text and image; its lengths in seconds, zero when not known
	Modalities  []string
	MinDuration int
	MaxDuration int
}

// What the picture model of a service takes
type ImageTakes struct {
	// Edits or restyles the pictures it is handed, besides drawing from text
	Edits bool
	// Most pictures it takes in all, the main one and its references
	References int
	// Takes the high resolution pass of image_generate's upscale
	Upscale bool
	// Creative controls it takes, of Controls
	Controls []string
	// The pictures it is handed give the look only, they are not edited
	Style bool
	// What the pickers say of the service as the settings have it, such as what its storage keeps
	Notes []string
}

// Creative controls a picture model may take, in the order image_generate lists them
var Controls = []string{"creativity", "thinking", "resolution", "intensity", "complexity", "movement"}

// What the video model of a service takes
type VideoTakes struct {
	// What it makes videos from: text, image
	Modalities  []string
	Aspects     []string
	Resolutions []string
	MinDuration int
	MaxDuration int
	// Takes the audio toggle, makes audio with every video whatever is asked, takes a negative prompt, a seed, the upscale pass
	Audio, AudioAlways, Negative, Seed, Upscale bool
	// Most reference pictures it takes
	References int
	// Lines the description of video_generate and the pickers add for it, such as how its videos chain into its own edit tools
	Notes []string
}

// One image_generate call as a service is asked it
type ImageRequest struct {
	Prompt string
	// landscape, square or portrait
	Aspect string
	// Picture to edit or restyle and the references after it: URLs, data URLs or paths of this machine
	Image      string
	References []string
	// High resolution pass asked for, nil for the model's default
	Upscale *bool
	// Creative controls the call gave that the service takes, by name
	Controls map[string]any
	// Model image_gen.model names, empty when it names none
	Model string
}

// Pictures a call hands a service, the main one first
func (r ImageRequest) Sources(limit int) []string {
	var sources []string
	if image := strings.TrimSpace(r.Image); image != "" {
		sources = append(sources, image)
	}
	for _, ref := range r.References {
		if ref = strings.TrimSpace(ref); ref != "" {
			sources = append(sources, ref)
		}
	}
	if limit >= 0 && len(sources) > limit {
		sources = sources[:limit]
	}
	return sources
}

// One video_generate call as a service is asked it
type VideoRequest struct {
	Prompt string
	// Model video_gen.model names, else the service's default
	Model string
	// Still to animate, and reference pictures: URLs, data URLs or paths of this machine
	Image      string
	References []string
	// Seconds asked for, nil for the model's default
	Duration *int
	// Aspect ratio and resolution asked for, 16:9 and 720p when the call named none
	Aspect     string
	Resolution string
	Negative   string
	Audio      *bool
	Seed       *int
	Upscale    *bool
}

// What a service is handed to make one picture or video
type Job struct {
	Config *config.Config
	// Told of the tokens a call billed by them took and the route it went to; nil when nobody counts them
	Used func(route provider.Route, usage provider.Usage)
}

// Records the tokens a call took when it reported any
func (j *Job) Record(route provider.Route, report any) {
	if j.Used == nil {
		return
	}
	if usage, ok := Usage(report); ok {
		j.Used(route, usage)
	}
}

type Registrant interface {
	Services() []Service
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Generation
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Every service, in the order they were registered
func (r *Registry) Services() []Service {
	var services []Service
	for _, registrant := range r.registrants {
		services = append(services, registrant.Services()...)
	}
	return services
}

// Services that make the kind
func (r *Registry) Making(kind Kind) []Service {
	return slices.DeleteFunc(r.Services(), func(s Service) bool { return !s.Makes(kind) })
}

// Service of the name that makes the kind
func (r *Registry) Service(kind Kind, name string) (Service, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, service := range r.Making(kind) {
		if service.Name == name {
			return service, true
		}
	}
	return Service{}, false
}

// Host the registry is nested in, through which plugins reach every service
func (r *Registry) Host() *Generation {
	return r.host
}

// Whether the service makes the kind
func (s Service) Makes(kind Kind) bool {
	if kind == Video {
		return s.Videos != nil
	}
	return s.Images != nil
}

// What it makes of the kind and how, a line for the pickers
func (s Service) About(kind Kind) string {
	switch {
	case kind == Video && s.Videos != nil:
		return s.Videos.About
	case kind == Image && s.Images != nil:
		return s.Images.About
	}
	return ""
}

// Setting a model picked for the service's kind is kept in: its own, else the kind's
func (s Service) ModelSetting(kind Kind) string {
	switch {
	case kind == Video && s.Videos != nil && s.Videos.Model != "":
		return s.Videos.Model
	case kind == Image && s.Images != nil && s.Images.Model != "":
		return s.Images.Model
	}
	return kind.model()
}

// Whether it can be used with the settings
func (s Service) Set(cfg *config.Config) bool {
	return s.Ready != nil && s.Ready(cfg)
}

// Service chosen for the kind, lower cased, empty when none is
func Chosen(cfg *config.Config, kind Kind) string {
	return strings.ToLower(strings.TrimSpace(cfg.GetString(kind.choice())))
}

// Model the settings name for the kind, empty when they name none
func Named(cfg *config.Config, kind Kind) string {
	return strings.TrimSpace(cfg.GetString(kind.model()))
}

// Service that makes the kind with the settings: the one chosen, set up or not, else the first set up; an error the model reads when the
// one chosen is not registered or none is set up
func (r *Registry) Active(cfg *config.Config, kind Kind) (Service, error) {
	if chosen := Chosen(cfg, kind); chosen != "" {
		if service, ok := r.Service(kind, chosen); ok {
			return service, nil
		}
		return Service{}, &Unregistered{Kind: kind, Name: chosen}
	}
	for _, service := range r.Making(kind) {
		if service.Set(cfg) {
			return service, nil
		}
	}
	if kind == Video {
		return Service{}, fmt.Errorf("No video generation backend is configured. Pick one with /videogen (%s).", r.names(kind))
	}
	return Service{}, fmt.Errorf("Image generation is unavailable: no image service is set up. Pick one with /imagegen (%s).", r.names(kind))
}

// Service chosen in the settings that no plugin registers
type Unregistered struct {
	Kind Kind
	Name string
}

func (e *Unregistered) Error() string {
	if e.Kind == Video {
		return fmt.Sprintf("video_gen.provider='%s' is set but no plugin registered that name. Pick an installed video generation service with /videogen.", e.Name)
	}
	return fmt.Sprintf("image_gen.provider='%s' is set but no plugin registered that name. Pick an installed image generation service with /imagegen.", e.Name)
}

// Names of the services of the kind as a list for a sentence
func (r *Registry) names(kind Kind) string {
	var names []string
	for _, service := range r.Making(kind) {
		names = append(names, service.Title)
	}
	if len(names) == 0 {
		return "none is installed"
	}
	return strings.Join(names, ", ")
}

// Image and video services of the plugins, and the cache what they make is kept in
type Generation struct {
	Registry
}

func New(cfg *config.Config) *Generation {
	g := &Generation{}
	options := func(kind Kind, automatic string) func() []config.Option {
		return func() []config.Option {
			out := []config.Option{{Value: "", Title: "Automatic", About: automatic}}
			for _, service := range g.Making(kind) {
				out = append(out, config.Option{Value: service.Name, Title: service.Title, About: service.About(kind)})
			}
			return out
		}
	}
	generationconfig.Register(cfg, options(Image, "The first service that is set up"), options(Video, "The first service that is set up"))
	return g
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (g *Generation) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = g
	}
	g.Registry.Register(registrant)
}
