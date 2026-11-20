package provider

import (
	"reflect"
	"strings"
	"testing"
)

type pbPlain struct {
	ProviderDefaults
	name string
}

func (p *pbPlain) Name() string { return p.name }

func newPbPlain(name string) *pbPlain {
	p := &pbPlain{name: name}
	p.Self = p
	return p
}

type pbLabelled struct {
	ProviderDefaults
}

func (p *pbLabelled) Name() string        { return "labelled" }
func (p *pbLabelled) DisplayName() string { return "Labelled Backend" }

type pbNameless struct{ ProviderDefaults }

type pbCatalog struct {
	CatalogProviderDefaults
	name string
}

func (p *pbCatalog) Name() string { return p.name }

func newPbCatalog(name string) *pbCatalog {
	p := &pbCatalog{name: name}
	p.Self = p
	return p
}

type pbCatalogModels struct {
	CatalogProviderDefaults
	models []map[string]any
}

func (p *pbCatalogModels) Name() string                 { return "catalogued" }
func (p *pbCatalogModels) ListModels() []map[string]any { return p.models }

type pbCatalogNameless struct{ CatalogProviderDefaults }

// Display name defaults to the provider name
func TestProviderBaseDisplayNameDefaultsToName(t *testing.T) {
	if got := newPbPlain("browser-use").DisplayName(); got != "browser-use" {
		t.Fatalf("display name %q", got)
	}
}

// Setup schema is the name badge tag and env vars picker row built from the display name
func TestProviderBaseSetupSchemaDefault(t *testing.T) {
	want := map[string]any{"name": "browser-use", "badge": "", "tag": "", "env_vars": []any{}}
	if got := newPbPlain("browser-use").GetSetupSchema(); !reflect.DeepEqual(got, want) {
		t.Fatalf("schema %#v", got)
	}
}

// Setup schema reads an overridden display name through the bound provider
func TestProviderBaseSetupSchemaUsesOverriddenDisplayName(t *testing.T) {
	p := &pbLabelled{}
	p.Self = p
	if got := p.GetSetupSchema()["name"]; got != "Labelled Backend" {
		t.Fatalf("schema name %v", got)
	}
}

// Name stays abstract so a type embedding only the defaults is not a provider
func TestProviderBaseNameIsAbstract(t *testing.T) {
	base := reflect.TypeFor[ProviderBase]()
	if reflect.TypeFor[*pbNameless]().Implements(base) || reflect.TypeFor[ProviderDefaults]().Implements(base) {
		t.Fatal("defaults alone satisfy ProviderBase")
	}
	if !reflect.TypeFor[*pbPlain]().Implements(base) {
		t.Fatal("a provider defining Name does not satisfy ProviderBase")
	}
	var _ ProviderBase = newPbPlain("x")
}

// Defaults never bound to their provider panic on first use
func TestProviderBaseUnboundDefaultsPanic(t *testing.T) {
	for name, call := range map[string]func(){
		"display": func() { _ = (&pbPlain{name: "x"}).DisplayName() },
		"schema":  func() { _ = (&pbPlain{name: "x"}).GetSetupSchema() },
		"catalog": func() { _ = (&pbCatalog{name: "x"}).DisplayName() },
		"model":   func() { _ = (&pbCatalog{name: "x"}).DefaultModel() },
	} {
		func() {
			defer func() {
				if rec := recover(); rec == nil || !strings.Contains(rec.(string), "is not bound to its provider") {
					t.Errorf("%s: recovered %v", name, rec)
				}
			}()
			call()
		}()
	}
}

// Catalog providers show the title cased name
func TestCatalogProviderBaseDisplayNameTitleCased(t *testing.T) {
	for name, want := range map[string]string{"openai": "Openai", "fal-ai": "Fal-Ai", "my_tts": "My_Tts", "elevenlabs2x": "Elevenlabs2X"} {
		if got := newPbCatalog(name).DisplayName(); got != want {
			t.Errorf("%s title %q want %q", name, got, want)
		}
	}
}

// Catalog setup schema names the provider by its title cased display name
func TestCatalogProviderBaseSetupSchemaUsesTitle(t *testing.T) {
	want := map[string]any{"name": "Openai", "badge": "", "tag": "", "env_vars": []any{}}
	if got := newPbCatalog("openai").GetSetupSchema(); !reflect.DeepEqual(got, want) {
		t.Fatalf("schema %#v", got)
	}
}

// Catalog providers are available by default with an empty catalog and no default model
func TestCatalogProviderBaseAvailabilityAndEmptyCatalog(t *testing.T) {
	p := newPbCatalog("openai")
	if !p.IsAvailable() {
		t.Fatal("not available by default")
	}
	if models := p.ListModels(); models == nil || len(models) != 0 {
		t.Fatalf("models %#v", models)
	}
	if got := p.DefaultModel(); got != "" {
		t.Fatalf("default model %q", got)
	}
}

// Default model is the id of the first entry of an overridden catalog
func TestCatalogProviderBaseDefaultModelReadsOverriddenCatalog(t *testing.T) {
	p := &pbCatalogModels{models: []map[string]any{{"id": "sonic-2", "display": "Sonic 2"}, {"id": "sonic-1"}}}
	p.Self = p
	if got := p.DefaultModel(); got != "sonic-2" {
		t.Fatalf("default model %q", got)
	}
	p.models = []map[string]any{{"display": "no id"}}
	if got := p.DefaultModel(); got != "" {
		t.Fatalf("default model without id %q", got)
	}
}

// Name stays abstract for catalog providers too
func TestCatalogProviderBaseNameIsAbstract(t *testing.T) {
	base := reflect.TypeFor[CatalogProviderBase]()
	if reflect.TypeFor[*pbCatalogNameless]().Implements(base) || reflect.TypeFor[CatalogProviderDefaults]().Implements(base) {
		t.Fatal("catalog defaults alone satisfy CatalogProviderBase")
	}
	var _ CatalogProviderBase = newPbCatalog("x")
	var _ CatalogProviderBase = &pbCatalogModels{}
}
