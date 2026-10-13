package actions

import (
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// PredefinedAction is a catalog entry seeded (disabled) for every datasource
// of the matching type.
type PredefinedAction struct {
	Name           string
	ActionType     string
	Description    string
	RiskLevel      models.RiskLevel
	RequiredScopes []string
	ParamsSchema   map[string]any
	ResultSchema   map[string]any
	PolicyKind     models.PolicyKind
}

// PredefinedActions returns the catalog for a datasource type.
func PredefinedActions(dsType models.DatasourceType) []PredefinedAction {
	switch dsType {
	case models.DatasourceGmail:
		return gmailActions()
	case models.DatasourceGoogleCalendar:
		return googleCalendarActions()
	case models.DatasourceGoogleDrive:
		return googleDriveActions()
	case models.DatasourceJira:
		return jiraActions()
	case models.DatasourceLinear:
		return linearActions()
	case models.DatasourceGithub:
		return githubActions()
	case models.DatasourcePostgres:
		return postgresActions()
	case models.DatasourceRest:
		return restActions()
	case models.DatasourceImap:
		return imapActions()
	}
	// Hubspot, FileUpload (and unknown types) have no predefined actions.
	return []PredefinedAction{}
}

// schema decodes a JSON object literal from the catalog; panics on a typo
// (caught by tests).
func schema(s string) map[string]any {
	v, err := util.DecodeJSON([]byte(s))
	if err != nil {
		panic("action catalog: invalid schema JSON: " + err.Error())
	}
	m, ok := v.(map[string]any)
	if !ok {
		panic("action catalog: schema is not an object")
	}
	return m
}

func gmailActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Create Draft",
			ActionType:     "create_draft",
			Description:    "Create a draft email without sending it. Low risk — the draft stays in the user's mailbox and can be deleted.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"gmail.compose"},
			ParamsSchema:   schema(`{"type": "object", "required": ["to", "subject", "body"], "properties": {"to": {"type": "string", "description": "Recipient email address"}, "subject": {"type": "string", "description": "Email subject line"}, "body": {"type": "string", "description": "Plain-text email body"}, "cc": {"type": "string", "description": "CC recipient(s)"}, "bcc": {"type": "string", "description": "BCC recipient(s)"}, "from": {"type": "string", "description": "Sender override (defaults to authenticated user)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"draft_id": {"type": "string"}, "message_id": {"type": "string"}, "thread_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Send Email",
			ActionType:     "send_email",
			Description:    "Send an email directly. High risk — the message is delivered immediately and cannot be recalled.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"gmail.send"},
			ParamsSchema:   schema(`{"type": "object", "required": ["to", "subject", "body"], "properties": {"to": {"type": "string", "description": "Recipient email address"}, "subject": {"type": "string", "description": "Email subject line"}, "body": {"type": "string", "description": "Plain-text email body"}, "cc": {"type": "string", "description": "CC recipient(s)"}, "bcc": {"type": "string", "description": "BCC recipient(s)"}, "from": {"type": "string", "description": "Sender override"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"message_id": {"type": "string"}, "thread_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Send Draft",
			ActionType:     "send_draft",
			Description:    "Send an existing draft. High risk — the draft is delivered immediately and the draft is consumed.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"gmail.send"},
			ParamsSchema:   schema(`{"type": "object", "required": ["draft_id"], "properties": {"draft_id": {"type": "string", "description": "ID of the draft to send"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"message_id": {"type": "string"}, "thread_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Reply",
			ActionType:     "reply",
			Description:    "Reply to an existing email thread. High risk — the reply is delivered immediately to all participants.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"gmail.send"},
			ParamsSchema:   schema(`{"type": "object", "required": ["thread_id", "body"], "properties": {"thread_id": {"type": "string", "description": "Thread to reply to"}, "message_id": {"type": "string", "description": "Specific message to reply to (defaults to last in thread)"}, "body": {"type": "string", "description": "Plain-text reply body"}, "cc": {"type": "string", "description": "Additional CC recipient(s)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"message_id": {"type": "string"}, "thread_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Forward",
			ActionType:     "forward",
			Description:    "Forward a message to another recipient. High risk — leaks email content to an external party.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"gmail.send"},
			ParamsSchema:   schema(`{"type": "object", "required": ["message_id", "to"], "properties": {"message_id": {"type": "string", "description": "ID of the message to forward"}, "to": {"type": "string", "description": "Recipient email address"}, "body": {"type": "string", "description": "Optional note prepended to the forwarded message"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"message_id": {"type": "string"}, "thread_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Add Label",
			ActionType:     "add_label",
			Description:    "Add a label to a message. Low risk — organizational only, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"gmail.modify"},
			ParamsSchema:   schema(`{"type": "object", "required": ["message_id", "label_id"], "properties": {"message_id": {"type": "string", "description": "ID of the message"}, "label_id": {"type": "string", "description": "ID of the label to add"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Remove Label",
			ActionType:     "remove_label",
			Description:    "Remove a label from a message. Low risk — organizational only, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"gmail.modify"},
			ParamsSchema:   schema(`{"type": "object", "required": ["message_id", "label_id"], "properties": {"message_id": {"type": "string", "description": "ID of the message"}, "label_id": {"type": "string", "description": "ID of the label to remove"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Mark as Read",
			ActionType:     "mark_read",
			Description:    "Mark a message as read. Low risk — fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"gmail.modify"},
			ParamsSchema:   schema(`{"type": "object", "required": ["message_id"], "properties": {"message_id": {"type": "string", "description": "ID of the message to mark as read"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Trash Message",
			ActionType:     "trash_message",
			Description:    "Move a message to trash. Medium risk — recoverable within 30 days but may disrupt workflows.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"gmail.modify"},
			ParamsSchema:   schema(`{"type": "object", "required": ["message_id"], "properties": {"message_id": {"type": "string", "description": "ID of the message to trash"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Create Label",
			ActionType:     "create_label",
			Description:    "Create a new Gmail label. Low risk — organizational, can be deleted later.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"gmail.labels"},
			ParamsSchema:   schema(`{"type": "object", "required": ["name"], "properties": {"name": {"type": "string", "description": "Name for the new label"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"label_id": {"type": "string"}, "name": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
	}
}

func googleCalendarActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Create Event",
			ActionType:     "create_event",
			Description:    "Create a calendar event. Medium risk — may send invitations to attendees.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"calendar.events"},
			ParamsSchema:   schema(`{"type": "object", "required": ["summary", "start", "end"], "properties": {"summary": {"type": "string", "description": "Event title"}, "description": {"type": "string", "description": "Event description"}, "location": {"type": "string", "description": "Event location"}, "start": {"type": "string", "format": "date-time", "description": "Start time (ISO 8601)"}, "end": {"type": "string", "format": "date-time", "description": "End time (ISO 8601)"}, "attendees": {"type": "array", "items": {"type": "string"}, "description": "Attendee email addresses"}, "calendar_id": {"type": "string", "description": "Calendar ID (defaults to primary)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"event_id": {"type": "string"}, "html_link": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Update Event",
			ActionType:     "update_event",
			Description:    "Update an existing calendar event. Medium risk — sends update notifications to attendees.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"calendar.events"},
			ParamsSchema:   schema(`{"type": "object", "required": ["event_id"], "properties": {"event_id": {"type": "string", "description": "ID of the event to update"}, "summary": {"type": "string", "description": "New event title"}, "description": {"type": "string", "description": "New event description"}, "location": {"type": "string", "description": "New location"}, "start": {"type": "string", "format": "date-time", "description": "New start time"}, "end": {"type": "string", "format": "date-time", "description": "New end time"}, "calendar_id": {"type": "string", "description": "Calendar ID (defaults to primary)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"event_id": {"type": "string"}, "html_link": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Delete Event",
			ActionType:     "delete_event",
			Description:    "Cancel and delete an event. High risk — sends cancellation to all attendees, hard to undo.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"calendar.events"},
			ParamsSchema:   schema(`{"type": "object", "required": ["event_id"], "properties": {"event_id": {"type": "string", "description": "ID of the event to delete"}, "calendar_id": {"type": "string", "description": "Calendar ID (defaults to primary)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "RSVP",
			ActionType:     "rsvp",
			Description:    "Respond to an event invitation. Low risk — only affects your own attendance status.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"calendar.events"},
			ParamsSchema:   schema(`{"type": "object", "required": ["event_id", "response"], "properties": {"event_id": {"type": "string", "description": "ID of the event"}, "response": {"type": "string", "enum": ["accepted", "tentative", "declined"], "description": "RSVP response"}, "calendar_id": {"type": "string", "description": "Calendar ID (defaults to primary)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Add Attendee",
			ActionType:     "add_attendee",
			Description:    "Add an attendee to an existing event. Medium risk — sends an invitation to the new attendee.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"calendar.events"},
			ParamsSchema:   schema(`{"type": "object", "required": ["event_id", "email"], "properties": {"event_id": {"type": "string", "description": "ID of the event"}, "email": {"type": "string", "description": "Email address of the attendee to add"}, "calendar_id": {"type": "string", "description": "Calendar ID (defaults to primary)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Quick Add",
			ActionType:     "quick_add",
			Description:    "Create an event from natural language text. Medium risk — Google interprets the text and may send invitations.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"calendar.events"},
			ParamsSchema:   schema(`{"type": "object", "required": ["text"], "properties": {"text": {"type": "string", "description": "Natural language event description (e.g. 'Lunch with Anna at noon tomorrow')"}, "calendar_id": {"type": "string", "description": "Calendar ID (defaults to primary)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"event_id": {"type": "string"}, "html_link": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
	}
}

func googleDriveActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Download File",
			ActionType:     "download_file",
			Description:    "Download a file's content. Low risk — read-only operation.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"drive.readonly"},
			ParamsSchema:   schema(`{"type": "object", "required": ["file_id"], "properties": {"file_id": {"type": "string", "description": "ID of the file to download"}, "mime_type": {"type": "string", "description": "Export MIME type for Google Docs (e.g. text/plain, application/pdf)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"content": {"type": "string", "description": "File content (text) or base64-encoded binary"}, "mime_type": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
	}
}

func jiraActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Create Issue",
			ActionType:     "create_issue",
			Description:    "Create a new Jira issue. Low risk — internal project tracking, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["project_key", "summary"], "properties": {"project_key": {"type": "string", "description": "Jira project key (e.g. PROJ)"}, "summary": {"type": "string", "description": "Issue title"}, "issue_type": {"type": "string", "description": "Issue type (Task, Bug, Story). Defaults to Task"}, "description": {"type": "string", "description": "Issue description"}, "priority": {"type": "string", "description": "Priority name (e.g. High, Medium, Low)"}, "labels": {"type": "array", "items": {"type": "string"}, "description": "Issue labels"}, "assignee_account_id": {"type": "string", "description": "Atlassian account ID of the assignee"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"issue_id": {"type": "string"}, "issue_key": {"type": "string"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Update Issue",
			ActionType:     "update_issue",
			Description:    "Update fields on an existing issue. Low risk — changes are tracked in the issue history.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}, "summary": {"type": "string", "description": "New summary"}, "description": {"type": "string", "description": "New description"}, "priority": {"type": "string", "description": "New priority"}, "labels": {"type": "array", "items": {"type": "string"}, "description": "New labels (replaces existing)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Transition Issue",
			ActionType:     "transition_issue",
			Description:    "Change the status of an issue (e.g. To Do -> In Progress). Low risk — reversible, tracked in history.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key", "transition_id"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}, "transition_id": {"type": "string", "description": "ID of the target transition"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Add Comment",
			ActionType:     "add_comment",
			Description:    "Add a comment to an issue. Low risk — visible to project watchers, deletable.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key", "body"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}, "body": {"type": "string", "description": "Comment text"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"comment_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Assign Issue",
			ActionType:     "assign_issue",
			Description:    "Assign an issue to a user. Low risk — sends a notification, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key", "account_id"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}, "account_id": {"type": "string", "description": "Atlassian account ID of the assignee"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Add Label",
			ActionType:     "add_label",
			Description:    "Add a label to an issue. Low risk — organizational, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key", "label"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}, "label": {"type": "string", "description": "Label to add"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Link Issues",
			ActionType:     "link_issues",
			Description:    "Create a link between two issues. Low risk — organizational, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["inward_issue", "outward_issue", "link_type"], "properties": {"inward_issue": {"type": "string", "description": "Issue key of the inward issue"}, "outward_issue": {"type": "string", "description": "Issue key of the outward issue"}, "link_type": {"type": "string", "description": "Link type name (e.g. 'Blocks', 'Relates to')"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Log Work",
			ActionType:     "log_work",
			Description:    "Log time spent on an issue. Medium risk — affects time tracking and reporting.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"write:jira-work"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key", "time_spent"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}, "time_spent": {"type": "string", "description": "Time in Jira format (e.g. '2h 30m')"}, "comment": {"type": "string", "description": "Work log comment"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"worklog_id": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Delete Issue",
			ActionType:     "delete_issue",
			Description:    "Permanently delete an issue. Critical risk — irreversible, all data and history is lost.",
			RiskLevel:      models.RiskCritical,
			RequiredScopes: []string{"manage:jira-project"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_key"], "properties": {"issue_key": {"type": "string", "description": "Issue key (e.g. PROJ-123)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
	}
}

func linearActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Create Issue",
			ActionType:     "create_issue",
			Description:    "Create a new Linear issue. Low risk — internal project tracking, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write"},
			ParamsSchema:   schema(`{"type": "object", "required": ["team_id", "title"], "properties": {"team_id": {"type": "string", "description": "Linear team UUID the issue belongs to (see the teams entity or any issue's team_id)"}, "title": {"type": "string", "description": "Issue title"}, "description": {"type": "string", "description": "Issue description (Markdown)"}, "priority": {"type": "integer", "description": "Priority: 0 none, 1 urgent, 2 high, 3 medium, 4 low"}, "assignee_id": {"type": "string", "description": "UUID of the user to assign"}, "project_id": {"type": "string", "description": "UUID of the project to file the issue under (see the projects entity or any issue's project_id)"}, "label_ids": {"type": "array", "items": {"type": "string"}, "description": "UUIDs of labels to apply"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"issue_id": {"type": "string"}, "identifier": {"type": "string"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Update Issue",
			ActionType:     "update_issue",
			Description:    "Update an existing Linear issue's fields. Low risk — changes are reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_id"], "properties": {"issue_id": {"type": "string", "description": "UUID or human identifier (e.g. ENG-123) of the issue to update"}, "title": {"type": "string", "description": "New issue title"}, "description": {"type": "string", "description": "New issue description (Markdown)"}, "status": {"type": "string", "description": "Workflow state name to move the issue to (e.g. Todo, In Progress, Done) \u2014 matches the status field returned when querying issues"}, "priority": {"type": "integer", "description": "Priority: 0 none, 1 urgent, 2 high, 3 medium, 4 low"}, "assignee_id": {"type": "string", "description": "UUID of the user to assign"}, "project_id": {"type": "string", "description": "UUID of the project to file the issue under (see the projects entity or any issue's project_id)"}, "label_ids": {"type": "array", "items": {"type": "string"}, "description": "UUIDs of labels to apply (replaces the existing set)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"issue_id": {"type": "string"}, "identifier": {"type": "string"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Create Project",
			ActionType:     "create_project",
			Description:    "Create a new Linear project under a team. Low risk — internal planning, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write"},
			ParamsSchema:   schema(`{"type": "object", "required": ["name", "team_id"], "properties": {"name": {"type": "string", "description": "Project name"}, "team_id": {"type": "string", "description": "UUID of the team the project belongs to (see the teams entity)"}, "description": {"type": "string", "description": "Project description (Markdown)"}, "lead_id": {"type": "string", "description": "UUID of the user to set as project lead"}, "state": {"type": "string", "description": "Project state: backlog, planned, started, paused, completed, canceled"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"project_id": {"type": "string"}, "name": {"type": "string"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Update Project",
			ActionType:     "update_project",
			Description:    "Update an existing Linear project's fields. Low risk — changes are reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"write"},
			ParamsSchema:   schema(`{"type": "object", "required": ["project_id"], "properties": {"project_id": {"type": "string", "description": "UUID of the project to update"}, "name": {"type": "string", "description": "New project name"}, "description": {"type": "string", "description": "New project description (Markdown)"}, "lead_id": {"type": "string", "description": "UUID of the user to set as project lead"}, "state": {"type": "string", "description": "Project state: backlog, planned, started, paused, completed, canceled"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"project_id": {"type": "string"}, "name": {"type": "string"}, "state": {"type": "string"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
	}
}

func githubActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Create Issue",
			ActionType:     "create_issue",
			Description:    "Open a new GitHub issue. Low risk — visible to collaborators, can be closed.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["title"], "properties": {"title": {"type": "string", "description": "Issue title"}, "body": {"type": "string", "description": "Issue body (Markdown)"}, "labels": {"type": "array", "items": {"type": "string"}, "description": "Label names"}, "assignees": {"type": "array", "items": {"type": "string"}, "description": "GitHub usernames to assign"}, "repository": {"type": "string", "description": "owner/repo (defaults to datasource config)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"issue_number": {"type": "integer"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Update Issue",
			ActionType:     "update_issue",
			Description:    "Edit an issue's title, body, or labels. Low risk — changes are tracked in the edit history.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_number"], "properties": {"issue_number": {"type": "integer", "description": "Issue number"}, "title": {"type": "string", "description": "New title"}, "body": {"type": "string", "description": "New body"}, "labels": {"type": "array", "items": {"type": "string"}, "description": "New labels (replaces existing)"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Close Issue",
			ActionType:     "close_issue",
			Description:    "Close an issue. Low risk — can be reopened.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_number"], "properties": {"issue_number": {"type": "integer", "description": "Issue number to close"}, "reason": {"type": "string", "enum": ["completed", "not_planned"], "description": "Close reason"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Add Comment",
			ActionType:     "add_comment",
			Description:    "Add a comment to an issue or pull request. Low risk — visible to collaborators, deletable.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_number", "body"], "properties": {"issue_number": {"type": "integer", "description": "Issue or PR number"}, "body": {"type": "string", "description": "Comment body (Markdown)"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"comment_id": {"type": "integer"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Add Label",
			ActionType:     "add_label",
			Description:    "Add a label to an issue or pull request. Low risk — organizational, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_number", "labels"], "properties": {"issue_number": {"type": "integer", "description": "Issue or PR number"}, "labels": {"type": "array", "items": {"type": "string"}, "description": "Label names to add"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Remove Label",
			ActionType:     "remove_label",
			Description:    "Remove a label from an issue or pull request. Low risk — organizational, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_number", "label"], "properties": {"issue_number": {"type": "integer", "description": "Issue or PR number"}, "label": {"type": "string", "description": "Label name to remove"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Assign Issue",
			ActionType:     "assign_issue",
			Description:    "Assign users to an issue. Low risk — sends notification, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["issue_number", "assignees"], "properties": {"issue_number": {"type": "integer", "description": "Issue number"}, "assignees": {"type": "array", "items": {"type": "string"}, "description": "GitHub usernames to assign"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Request Review",
			ActionType:     "request_review",
			Description:    "Request a review on a pull request. Low risk — sends a notification.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["pull_number", "reviewers"], "properties": {"pull_number": {"type": "integer", "description": "Pull request number"}, "reviewers": {"type": "array", "items": {"type": "string"}, "description": "GitHub usernames to request review from"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Create Pull Request",
			ActionType:     "create_pull_request",
			Description:    "Open a pull request. Medium risk — visible to collaborators, triggers CI pipelines.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["title", "head", "base"], "properties": {"title": {"type": "string", "description": "PR title"}, "body": {"type": "string", "description": "PR description (Markdown)"}, "head": {"type": "string", "description": "Branch to merge from"}, "base": {"type": "string", "description": "Branch to merge into"}, "draft": {"type": "boolean", "description": "Create as draft PR"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"pull_number": {"type": "integer"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Merge Pull Request",
			ActionType:     "merge_pull_request",
			Description:    "Merge a pull request. High risk — changes the codebase, hard to revert, may trigger deployments.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["pull_number"], "properties": {"pull_number": {"type": "integer", "description": "Pull request number"}, "merge_method": {"type": "string", "enum": ["merge", "squash", "rebase"], "description": "Merge strategy"}, "commit_message": {"type": "string", "description": "Custom merge commit message"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"sha": {"type": "string"}, "merged": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Create Release",
			ActionType:     "create_release",
			Description:    "Create a GitHub release. High risk — public-facing, may trigger deployment pipelines.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["tag_name"], "properties": {"tag_name": {"type": "string", "description": "Git tag for the release (e.g. v1.0.0)"}, "name": {"type": "string", "description": "Release title"}, "body": {"type": "string", "description": "Release notes (Markdown)"}, "draft": {"type": "boolean", "description": "Create as draft release"}, "prerelease": {"type": "boolean", "description": "Mark as pre-release"}, "target_commitish": {"type": "string", "description": "Branch or commit SHA to tag"}, "repository": {"type": "string", "description": "owner/repo"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"release_id": {"type": "integer"}, "url": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Get File Content",
			ActionType:     "get_file_content",
			Description:    "Read the content of a file at a specific path and branch. Low risk — read-only operation.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{"repo"},
			ParamsSchema:   schema(`{"type": "object", "required": ["path"], "properties": {"path": {"type": "string", "description": "File path relative to repo root (e.g. src/main.rs)"}, "branch": {"type": "string", "description": "Branch or ref (defaults to the repo's default branch)"}, "repository": {"type": "string", "description": "owner/repo (defaults to datasource config)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string", "description": "Decoded UTF-8 file content"}, "size": {"type": "integer"}, "sha": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
	}
}

func postgresActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Insert Row",
			ActionType:     "insert_row",
			Description:    "Insert a row into a table. Medium risk — adds data, reversible with a delete.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{"INSERT"},
			ParamsSchema:   schema(`{"type": "object", "required": ["table", "values"], "properties": {"table": {"type": "string", "description": "Table name"}, "values": {"type": "object", "description": "Column-value pairs to insert"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"rows_affected": {"type": "integer"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Update Rows",
			ActionType:     "update_rows",
			Description:    "Update rows matching a condition. High risk — modifies existing data, previous values may be lost.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"UPDATE"},
			ParamsSchema:   schema(`{"type": "object", "required": ["table", "set", "where"], "properties": {"table": {"type": "string", "description": "Table name"}, "set": {"type": "object", "description": "Column-value pairs to update"}, "where": {"type": "object", "description": "Filter conditions (column-value equality)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"rows_affected": {"type": "integer"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Delete Rows",
			ActionType:     "delete_rows",
			Description:    "Delete rows matching a condition. Critical risk — data is permanently lost and cannot be recovered.",
			RiskLevel:      models.RiskCritical,
			RequiredScopes: []string{"DELETE"},
			ParamsSchema:   schema(`{"type": "object", "required": ["table", "where"], "properties": {"table": {"type": "string", "description": "Table name"}, "where": {"type": "object", "description": "Filter conditions (column-value equality)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"rows_affected": {"type": "integer"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Call Function",
			ActionType:     "call_function",
			Description:    "Execute a stored function. High risk — behavior depends entirely on the function body.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{"EXECUTE"},
			ParamsSchema:   schema(`{"type": "object", "required": ["function_name"], "properties": {"function_name": {"type": "string", "description": "Name of the stored function"}, "args": {"type": "array", "items": {}, "description": "Positional arguments"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"result": {"description": "Function return value"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
	}
}

func imapActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "Send Email",
			ActionType:     "send_email",
			Description:    "Send an email via SMTP. High risk — delivered immediately, cannot be recalled.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["to", "subject", "body"], "properties": {"to": {"type": "string", "description": "Recipient email address"}, "subject": {"type": "string", "description": "Email subject line"}, "body": {"type": "string", "description": "Plain-text email body"}, "cc": {"type": "string", "description": "CC recipient(s)"}, "bcc": {"type": "string", "description": "BCC recipient(s)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"status": {"type": "string"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
		{
			Name:           "Mark as Read",
			ActionType:     "mark_read",
			Description:    "Add \\Seen flag to a message. Low risk — fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["uid"], "properties": {"uid": {"type": "string", "description": "Message UID"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Mark as Unread",
			ActionType:     "mark_unread",
			Description:    "Remove \\Seen flag from a message. Low risk — fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["uid"], "properties": {"uid": {"type": "string", "description": "Message UID"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Flag Message",
			ActionType:     "flag_message",
			Description:    "Add \\Flagged flag to a message. Low risk — organizational, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["uid"], "properties": {"uid": {"type": "string", "description": "Message UID"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Unflag Message",
			ActionType:     "unflag_message",
			Description:    "Remove \\Flagged flag from a message. Low risk — organizational, fully reversible.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["uid"], "properties": {"uid": {"type": "string", "description": "Message UID"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Move Message",
			ActionType:     "move_message",
			Description:    "Move a message to another mailbox. Low risk — reversible by moving back.",
			RiskLevel:      models.RiskLow,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["uid", "destination"], "properties": {"uid": {"type": "string", "description": "Message UID"}, "destination": {"type": "string", "description": "Target mailbox (e.g. Archive, Sent)"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "Delete Message",
			ActionType:     "delete_message",
			Description:    "Mark a message as deleted and expunge. Medium risk — may be unrecoverable depending on server.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["uid"], "properties": {"uid": {"type": "string", "description": "Message UID to delete"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"success": {"type": "boolean"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
	}
}

func restActions() []PredefinedAction {
	return []PredefinedAction{
		{
			Name:           "POST Request",
			ActionType:     "post_request",
			Description:    "Send a POST request to a configured endpoint. Medium risk — creates a resource, behavior depends on the API.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["path"], "properties": {"path": {"type": "string", "description": "Request path (appended to base URL)"}, "body": {"type": "object", "description": "JSON request body"}, "headers": {"type": "object", "description": "Additional request headers"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"status": {"type": "integer"}, "body": {"description": "Response body"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "PUT Request",
			ActionType:     "put_request",
			Description:    "Send a PUT request to replace a resource. Medium risk — overwrites existing data.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["path"], "properties": {"path": {"type": "string", "description": "Request path (appended to base URL)"}, "body": {"type": "object", "description": "JSON request body"}, "headers": {"type": "object", "description": "Additional request headers"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"status": {"type": "integer"}, "body": {"description": "Response body"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "PATCH Request",
			ActionType:     "patch_request",
			Description:    "Send a PATCH request for partial update. Medium risk — modifies existing data.",
			RiskLevel:      models.RiskMedium,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["path"], "properties": {"path": {"type": "string", "description": "Request path (appended to base URL)"}, "body": {"type": "object", "description": "JSON request body"}, "headers": {"type": "object", "description": "Additional request headers"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"status": {"type": "integer"}, "body": {"description": "Response body"}}}`),
			PolicyKind:     models.PolicyAuto,
		},
		{
			Name:           "DELETE Request",
			ActionType:     "delete_request",
			Description:    "Send a DELETE request. High risk — removes a resource, may be irreversible.",
			RiskLevel:      models.RiskHigh,
			RequiredScopes: []string{},
			ParamsSchema:   schema(`{"type": "object", "required": ["path"], "properties": {"path": {"type": "string", "description": "Request path (appended to base URL)"}, "headers": {"type": "object", "description": "Additional request headers"}}}`),
			ResultSchema:   schema(`{"type": "object", "properties": {"status": {"type": "integer"}, "body": {"description": "Response body"}}}`),
			PolicyKind:     models.PolicyAlwaysApprove,
		},
	}
}
