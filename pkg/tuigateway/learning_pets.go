package tuigateway

import (
	"encoding/json"
	"errors"
)

type LearningNodeKind string

func (LearningNodeKind) Values() []string { return []string{"skill", "memory"} }

// One learned skill or memory chunk of the learning graph
type LearningGraphNode struct {
	ID           string           `json:"id"`
	Label        string           `json:"label"`
	Kind         LearningNodeKind `json:"kind"`
	Timestamp    *float64         `json:"timestamp"`
	Category     string           `json:"category"`
	UseCount     int64            `json:"useCount"`
	State        string           `json:"state"`
	CreatedBy    *string          `json:"createdBy"`
	Pinned       bool             `json:"pinned"`
	MemorySource string           `json:"memorySource,omitzero"`
}

type LearningGraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type LearningGraphCluster struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

// One memory chunk the graph shows, its first line as the title and the start of its text
type LearningMemoryCard struct {
	Source      string   `json:"source"`
	Timestamp   *float64 `json:"timestamp"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Fingerprint string   `json:"fingerprint"`
}

// Learning graph of the profile, the stats an open object of counts and top categories
type LearningGraphResult struct {
	Nodes    []LearningGraphNode    `json:"nodes"`
	Edges    []LearningGraphEdge    `json:"edges"`
	Clusters []LearningGraphCluster `json:"clusters"`
	Memory   []LearningMemoryCard   `json:"memory"`
	Stats    map[string]any         `json:"stats"`
}

type LearningGraphParams struct{}

type LearningFramesParams struct {
	Cols   int64 `json:"cols,omitzero"`
	Rows   int64 `json:"rows,omitzero"`
	Frames int64 `json:"frames,omitzero"`
}

// One painted run of a chart row, sent as text, style key, alpha and an optional hex override
type LearningRun struct {
	Text  string
	Style string
	Alpha float64
	Hex   string
}

func (r *LearningRun) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil || len(parts) < 2 || len(parts) > 4 {
		return errors.New("wants a run of text, style, alpha and an optional hex")
	}
	run := LearningRun{Alpha: 1}
	if json.Unmarshal(parts[0], &run.Text) != nil || json.Unmarshal(parts[1], &run.Style) != nil {
		return errors.New("wants the run text and style as text")
	}
	if len(parts) > 2 && json.Unmarshal(parts[2], &run.Alpha) != nil {
		return errors.New("wants the run alpha as a number")
	}
	if len(parts) > 3 && string(parts[3]) != "null" && json.Unmarshal(parts[3], &run.Hex) != nil {
		return errors.New("wants the run hex as text or null")
	}
	*r = run
	return nil
}

func (r LearningRun) MarshalJSON() ([]byte, error) {
	parts := []any{r.Text, r.Style, r.Alpha}
	if r.Hex != "" {
		parts = append(parts, r.Hex)
	}
	return json.Marshal(parts)
}

// One frame of the timeline at a reveal, its chart rows and the label cards it placed
type LearningFrame struct {
	Reveal  float64          `json:"reveal"`
	Date    string           `json:"date"`
	Visible int64            `json:"visible"`
	Grid    [][]LearningRun  `json:"grid"`
	Labels  []map[string]any `json:"labels"`
}

type LearningLegendItem struct {
	Glyph string `json:"glyph"`
	Label string `json:"label"`
	Style string `json:"style,omitzero"`
	Color string `json:"color,omitzero"`
}

type LearningNodeRow struct {
	ID        string `json:"id"`
	Glyph     string `json:"glyph"`
	Label     string `json:"label"`
	FullLabel string `json:"fullLabel"`
	Meta      string `json:"meta"`
	Body      string `json:"body"`
	Style     string `json:"style"`
}

// One slice of the timeline with its learned skills and memories
type LearningBucketRow struct {
	Index    int64             `json:"index"`
	Label    string            `json:"label"`
	Date     string            `json:"date"`
	Skills   int64             `json:"skills"`
	Memories int64             `json:"memories"`
	Total    int64             `json:"total"`
	Category string            `json:"category,omitzero"`
	Color    string            `json:"color,omitzero"`
	Nodes    []LearningNodeRow `json:"nodes"`
}

type LearningAxis struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Pre-rendered timeline the journey window walks without asking again
type LearningFramesResult struct {
	Frames     []LearningFrame      `json:"frames"`
	Legend     []LearningLegendItem `json:"legend"`
	Categories []LearningLegendItem `json:"categories"`
	Buckets    []LearningBucketRow  `json:"buckets"`
	Summary    []string             `json:"summary"`
	Axis       LearningAxis         `json:"axis"`
	Count      int64                `json:"count"`
	Cols       int64                `json:"cols"`
	Rows       int64                `json:"rows"`
}

type LearningNodeParams struct {
	ID string `json:"id"`
}

type LearningEditParams struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// Content of a node for an edit prefill, a failure carrying its reason in message
type LearningDetailResult struct {
	Ok      bool             `json:"ok"`
	Message string           `json:"message,omitzero"`
	Kind    LearningNodeKind `json:"kind,omitzero"`
	ID      string           `json:"id,omitzero"`
	Label   string           `json:"label,omitzero"`
	Content *string          `json:"content,omitzero"`
}

type LearningMutationResult struct {
	Ok      bool   `json:"ok"`
	Message string `json:"message,omitzero"`
}

// Learning graph of the profile, the skill and memory nodes with their links and stats
var LearningGraph = NewMethod[LearningGraphParams, LearningGraphResult]("learning.graph")

// Pre-renders the journey timeline so the window walks it locally
var LearningFrames = NewMethod[LearningFramesParams, LearningFramesResult]("learning.frames")

// Content of a skill or memory node for an edit prefill
var LearningDetail = NewMethod[LearningNodeParams, LearningDetailResult]("learning.detail")

// Archives a skill or removes a memory chunk
var LearningDelete = NewMethod[LearningNodeParams, LearningMutationResult]("learning.delete")

// Rewrites the content of a skill or memory node
var LearningEdit = NewMethod[LearningEditParams, LearningMutationResult]("learning.edit")

type PetInfoParams struct {
	Profile       *string `json:"profile,omitzero"`
	KnownRevision string  `json:"knownRevision,omitzero"`
}

// Spritesheet and frame geometry of a pet, every field absent while the pet display is off
type PetSprite struct {
	Slug                string           `json:"slug,omitzero"`
	DisplayName         string           `json:"displayName,omitzero"`
	Mime                string           `json:"mime,omitzero"`
	SpritesheetBase64   string           `json:"spritesheetBase64,omitzero"`
	SpritesheetRevision string           `json:"spritesheetRevision,omitzero"`
	FrameW              int64            `json:"frameW,omitzero"`
	FrameH              int64            `json:"frameH,omitzero"`
	FramesPerState      int64            `json:"framesPerState,omitzero"`
	FramesByState       map[string]int64 `json:"framesByState,omitzero"`
	FramesByRow         map[string]int64 `json:"framesByRow,omitzero"`
	LoopMs              int64            `json:"loopMs,omitzero"`
	Scale               float64          `json:"scale,omitzero"`
	StateRows           []string         `json:"stateRows,omitzero"`
}

// Active pet for sprite renderers, the spritesheet left out when the caller already holds its revision
type PetInfoResult struct {
	Enabled bool `json:"enabled"`
	PetSprite
	SpritesheetUnchanged bool `json:"spritesheetUnchanged,omitzero"`
}

// Cheap metadata of the active pet, enabled false and nothing else while the display is off
type PetInfoMetaResult struct {
	Enabled             bool    `json:"enabled"`
	Slug                string  `json:"slug,omitzero"`
	DisplayName         string  `json:"displayName,omitzero"`
	Scale               float64 `json:"scale,omitzero"`
	SpritesheetRevision string  `json:"spritesheetRevision,omitzero"`
}

type PetCellsParams struct {
	Profile  *string `json:"profile,omitzero"`
	State    string  `json:"state,omitzero"`
	Cols     int64   `json:"cols,omitzero"`
	Graphics bool    `json:"graphics,omitzero"`
}

// Frames of one pet state, half-block cells of eight colour channels or the kitty transmit escapes
type PetCellFrames struct {
	Cells [][][][8]int64
	Kitty []string
}

func (f *PetCellFrames) UnmarshalJSON(data []byte) error {
	var kitty []string
	if err := json.Unmarshal(data, &kitty); err == nil {
		*f = PetCellFrames{Kitty: kitty}
		return nil
	}
	var cells [][][][8]int64
	if err := json.Unmarshal(data, &cells); err != nil {
		return errors.New("wants frames of half-block cells or kitty escapes")
	}
	*f = PetCellFrames{Cells: cells}
	return nil
}

func (f PetCellFrames) MarshalJSON() ([]byte, error) {
	if f.Kitty != nil {
		return json.Marshal(f.Kitty)
	}
	if f.Cells == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(f.Cells)
}

// Frames of one pet state for a cell renderer, the kitty fields set when the graphics path was taken
type PetCellsResult struct {
	Enabled     bool           `json:"enabled"`
	Slug        string         `json:"slug,omitzero"`
	DisplayName string         `json:"displayName,omitzero"`
	State       string         `json:"state,omitzero"`
	Cols        int64          `json:"cols,omitzero"`
	FrameMs     float64        `json:"frameMs,omitzero"`
	Frames      *PetCellFrames `json:"frames,omitzero"`
	Scale       float64        `json:"scale,omitzero"`
	Graphics    string         `json:"graphics,omitzero"`
	ImageID     int64          `json:"imageId,omitzero"`
	Color       string         `json:"color,omitzero"`
	Rows        int64          `json:"rows,omitzero"`
	Placeholder []string       `json:"placeholder,omitzero"`
}

type PetGalleryParams struct {
	Profile   *string `json:"profile,omitzero"`
	LocalOnly bool    `json:"localOnly,omitzero"`
}

type PetGalleryEntry struct {
	Slug           string `json:"slug"`
	DisplayName    string `json:"displayName"`
	Installed      bool   `json:"installed"`
	SpritesheetURL string `json:"spritesheetUrl,omitzero"`
	Curated        bool   `json:"curated,omitzero"`
	Generated      bool   `json:"generated,omitzero"`
}

type PetGalleryResult struct {
	Enabled bool              `json:"enabled"`
	Active  string            `json:"active,omitzero"`
	Pets    []PetGalleryEntry `json:"pets,omitzero"`
}

type PetSlugParams struct {
	Profile *string `json:"profile,omitzero"`
	Slug    string  `json:"slug"`
}

type PetSlugResult struct {
	Ok          bool   `json:"ok"`
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName,omitzero"`
}

type PetRenameParams struct {
	Profile *string `json:"profile,omitzero"`
	Slug    string  `json:"slug"`
	Name    string  `json:"name"`
}

type PetExportResult struct {
	Ok        bool   `json:"ok"`
	Filename  string `json:"filename"`
	ZipBase64 string `json:"zipBase64"`
}

type PetOkResult struct {
	Ok bool `json:"ok"`
}

// Scale to persist as the user typed it, text that is no number answered 4004
type PetScaleParams struct {
	Profile *string `json:"profile,omitzero"`
	Scale   string  `json:"scale"`
}

type PetScaleResult struct {
	Ok    bool    `json:"ok"`
	Scale float64 `json:"scale"`
}

type PetCancelParams struct {
	Profile *string `json:"profile,omitzero"`
	Token   string  `json:"token,omitzero"`
}

type PetGenerateParams struct {
	Profile        *string `json:"profile,omitzero"`
	Prompt         string  `json:"prompt,omitzero"`
	ReferenceImage string  `json:"referenceImage,omitzero"`
	Count          int64   `json:"count,omitzero"`
	Style          string  `json:"style,omitzero"`
	Provider       string  `json:"provider,omitzero"`
}

type PetDraft struct {
	Index   int64  `json:"index"`
	DataURI string `json:"dataUri"`
}

type PetGenerateResult struct {
	Ok     bool       `json:"ok"`
	Token  string     `json:"token"`
	Drafts []PetDraft `json:"drafts"`
}

type PetHatchParams struct {
	Profile     *string `json:"profile,omitzero"`
	Token       string  `json:"token"`
	Name        string  `json:"name"`
	CancelToken string  `json:"cancelToken,omitzero"`
	Index       *int64  `json:"index,omitzero"`
	Description string  `json:"description,omitzero"`
	Prompt      string  `json:"prompt,omitzero"`
	Style       string  `json:"style,omitzero"`
	Provider    string  `json:"provider,omitzero"`
}

// Hatched pet installed but not active, its sprite empty when it could not be loaded back
type PetHatchResult struct {
	Ok          bool      `json:"ok"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"displayName"`
	Warnings    []string  `json:"warnings"`
	Pet         PetSprite `json:"pet"`
}

// Draft generation progress, a token and count first and then one frame per finished draft
type PetGenerateProgressPayload struct {
	Token   string `json:"token"`
	Count   int64  `json:"count"`
	Index   *int64 `json:"index,omitzero"`
	DataURI string `json:"dataUri,omitzero"`
}

// Hatch progress, an event with its detail or a drawn row as state, done and total
type PetHatchProgressPayload struct {
	Event  string `json:"event"`
	Detail string `json:"detail,omitzero"`
	State  string `json:"state,omitzero"`
	Done   string `json:"done,omitzero"`
	Total  string `json:"total,omitzero"`
}

// Active pet for sprite renderers, its spritesheet as base64 with frame geometry and state rows
var PetInfo = NewMethod[PetInfoParams, PetInfoResult]("pet.info")

// Metadata of the active pet that tells a renderer whether its spritesheet changed
var PetInfoMeta = NewMethod[ProfileParams, PetInfoMetaResult]("pet.info.meta")

// Half-block cell frames or a kitty placement of one pet state
var PetCells = NewMethod[PetCellsParams, PetCellsResult]("pet.cells")

// Pet gallery with the local install state, the remote manifest skipped for localOnly
var PetGallery = NewMethod[PetGalleryParams, PetGalleryResult]("pet.gallery")

// Installs when needed and activates a pet
var PetSelect = NewMethod[PetSlugParams, PetSlugResult]("pet.select")

// Uninstalls a pet, turning the display off when it was active
var PetRemove = NewMethod[PetSlugParams, PetSlugResult]("pet.remove")

// Renames a pet and realigns its slug, following the active slug in config
var PetRename = NewMethod[PetRenameParams, PetSlugResult]("pet.rename")

// Exports an installed pet as a re-importable zip
var PetExport = NewMethod[PetSlugParams, PetExportResult]("pet.export")

// Turns the pet display off
var PetDisable = NewMethod[ProfileParams, PetOkResult]("pet.disable")

// Turns the pet display on, the configured pet kept
var PetEnable = NewMethod[ProfileParams, PetOkResult]("pet.enable")

// Persists the pet scale clamped to the renderer bounds
var PetScale = NewMethod[PetScaleParams, PetScaleResult]("pet.scale")

// Stops a pet generate or hatch in flight by its token
var PetCancel = NewMethod[PetCancelParams, PetOkResult]("pet.cancel")

// Base look drafts of a new pet, each also streamed as pet.generate.progress
var PetGenerate = NewMethod[PetGenerateParams, PetGenerateResult]("pet.generate")

// Turns a base draft into a full spritesheet pet, progress streamed as pet.hatch.progress
var PetHatch = NewMethod[PetHatchParams, PetHatchResult]("pet.hatch")

// The active pet or its spritesheet changed
var PetChangedEvent = NewEvent[PetInfoMetaResult]("pet.changed")

// Progress of a draft generation
var PetGenerateProgressEvent = NewEvent[PetGenerateProgressPayload]("pet.generate.progress")

// Progress of a hatch drawing its rows
var PetHatchProgressEvent = NewEvent[PetHatchProgressPayload]("pet.hatch.progress")
