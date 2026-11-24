package tuigateway

// One folder a project covers
type ProjectFolderRow struct {
	Path  string `json:"path"`
	Label string `json:"label,omitzero"`
	// The folder sessions and worktrees of the project start in
	Primary bool `json:"primary,omitzero"`
}

// One project as the projects screen shows it
type ProjectRow struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
	// Kanban board bound to the project, empty for none
	Board    string `json:"board,omitzero"`
	Archived bool   `json:"archived,omitzero"`
	// The project new sessions work in
	Active  bool               `json:"active,omitzero"`
	Folders []ProjectFolderRow `json:"folders"`
}

type ProjectsListParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Every project of the profile, archived ones included
type ProjectsListResult struct {
	Projects []ProjectRow `json:"projects"`
}

// One change to the projects of the profile: create, rename, describe, add-folder, remove-folder, set-primary, use, clear, archive, restore or bind-board
type ProjectsChangeParams struct {
	Profile *string `json:"profile,omitzero"`
	Verb    string  `json:"verb"`
	// Id or slug of the project the change is for, empty for create and clear
	Project string `json:"project,omitzero"`
	// Name of a new project or the new name of a renamed one
	Name        string `json:"name,omitzero"`
	Description string `json:"description,omitzero"`
	// Folders of a new project, the first one its primary
	Folders []string `json:"folders,omitzero"`
	// Folder an add-folder, remove-folder or set-primary change is for
	Folder string `json:"folder,omitzero"`
	Label  string `json:"label,omitzero"`
	// Makes an added folder the primary one
	Primary bool `json:"primary,omitzero"`
	// Kanban board to bind, empty unbinding the one bound now
	Board string `json:"board,omitzero"`
	// Makes a new project the one new sessions work in
	Use bool `json:"use,omitzero"`
}

// Every project after the change, and the id of the one it made or changed
type ProjectsChangeResult struct {
	Project  string       `json:"project,omitzero"`
	Projects []ProjectRow `json:"projects"`
}

// Projects of the profile for the projects screen
var ProjectsList = NewMethod[ProjectsListParams, ProjectsListResult]("projects.list")

// One change to the projects of the profile, answered with the projects after it
var ProjectsChange = NewMethod[ProjectsChangeParams, ProjectsChangeResult]("projects.change")
