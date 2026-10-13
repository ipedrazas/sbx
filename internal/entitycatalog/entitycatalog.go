// Package entitycatalog lists the entities a datasource type exposes (e.g.
// Linear: issues, projects, teams). A dataset binds to one via
// source_config.resource_type. Types with a single view return an empty list,
// so the GUI shows no entity selector.
package entitycatalog

import "github.com/ipedrazas/sbx/internal/models"

// EntityDescriptor describes one selectable entity.
type EntityDescriptor struct {
	// Stable key written into source_config.resource_type.
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// action_types that operate on this entity (informational grouping only).
	Actions []string `json:"actions"`
	// The entity a dataset gets when resource_type is absent.
	Default bool `json:"default"`
}

// Entities returns the entities ds exposes; empty when it has a single view.
func Entities(dsType models.DatasourceType) []EntityDescriptor {
	switch dsType {
	case models.DatasourceLinear:
		return []EntityDescriptor{
			{
				Key:         "issues",
				Label:       "Issues",
				Description: "Work items: title, status, assignee, priority, team, project.",
				Actions:     []string{"create_issue", "update_issue"},
				Default:     true,
			},
			{
				Key:         "projects",
				Label:       "Projects",
				Description: "Projects: name, state, progress, lead, and owning teams.",
				Actions:     []string{"create_project", "update_project"},
			},
			{
				Key:         "teams",
				Label:       "Teams",
				Description: "Teams: id (UUID), key, and name — needed to create issues and projects.",
				Actions:     []string{},
			},
		}
	}
	return []EntityDescriptor{}
}
