package actions

import (
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
)

var catalogTypes = []models.DatasourceType{
	models.DatasourceGmail, models.DatasourceGoogleCalendar, models.DatasourceGoogleDrive, models.DatasourceJira,
	models.DatasourceLinear, models.DatasourceGithub, models.DatasourcePostgres, models.DatasourceRest, models.DatasourceImap,
}

func TestAllTypesReturnNonEmptyLists(t *testing.T) {
	for _, ty := range catalogTypes {
		if len(PredefinedActions(ty)) == 0 {
			t.Errorf("%s returned no predefined actions", ty)
		}
	}
	for _, ty := range []models.DatasourceType{models.DatasourceHubspot, models.DatasourceFileUpload, "unknown"} {
		if a := PredefinedActions(ty); a == nil || len(a) != 0 {
			t.Errorf("%s: expected empty list", ty)
		}
	}
}

func TestExpectedCountsPerType(t *testing.T) {
	want := map[models.DatasourceType]int{
		models.DatasourceGmail: 10, models.DatasourceGoogleCalendar: 6, models.DatasourceGoogleDrive: 1,
		models.DatasourceJira: 9, models.DatasourceLinear: 4, models.DatasourceGithub: 12,
		models.DatasourcePostgres: 4, models.DatasourceRest: 4, models.DatasourceImap: 7,
	}
	for ty, n := range want {
		if got := len(PredefinedActions(ty)); got != n {
			t.Errorf("%s: got %d want %d", ty, got, n)
		}
	}
}

func TestActionTypesUniqueWithinEachDatasource(t *testing.T) {
	for _, ty := range catalogTypes {
		seen := map[string]bool{}
		for _, a := range PredefinedActions(ty) {
			if seen[a.ActionType] {
				t.Errorf("%s has duplicate action_type %q", ty, a.ActionType)
			}
			seen[a.ActionType] = true
		}
	}
}

func TestAllEntriesHaveDescriptionAndValidSchemas(t *testing.T) {
	for _, ty := range catalogTypes {
		for _, a := range PredefinedActions(ty) {
			if a.Description == "" || a.Name == "" {
				t.Errorf("%s/%s has empty description/name", ty, a.ActionType)
			}
			if a.ParamsSchema == nil || a.ResultSchema == nil {
				t.Errorf("%s/%s schema is not an object", ty, a.ActionType)
			}
			if a.RequiredScopes == nil {
				t.Errorf("%s/%s required_scopes must be non-nil", ty, a.ActionType)
			}
		}
	}
}

func TestCatalogSpotChecks(t *testing.T) {
	var markRead PredefinedAction
	for _, a := range PredefinedActions(models.DatasourceImap) {
		if a.ActionType == "mark_read" {
			markRead = a
		}
	}
	if markRead.Description != `Add \Seen flag to a message. Low risk — fully reversible.` {
		t.Fatal(markRead.Description)
	}
	for _, a := range PredefinedActions(models.DatasourceJira) {
		if a.ActionType == "delete_issue" && (a.RiskLevel != models.RiskCritical || a.PolicyKind != models.PolicyAlwaysApprove) {
			t.Fatalf("%+v", a)
		}
	}
}

func TestSchemaPanicsOnBadInput(t *testing.T) {
	for _, s := range []string{"{", "[]"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("schema(%q) should panic", s)
				}
			}()
			schema(s)
		}()
	}
}
