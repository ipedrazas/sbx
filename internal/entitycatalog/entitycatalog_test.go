package entitycatalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
)

func TestLinearExposesThreeEntitiesWithOneDefault(t *testing.T) {
	e := Entities(models.DatasourceLinear)
	if len(e) != 3 {
		t.Fatalf("len = %d", len(e))
	}
	defaults := 0
	keys := map[string]bool{}
	for _, d := range e {
		if d.Default {
			defaults++
		}
		keys[d.Key] = true
	}
	if defaults != 1 || !keys["projects"] || !keys["teams"] {
		t.Fatalf("unexpected entities: %+v", e)
	}
}

func TestSingleViewTypesExposeNoEntities(t *testing.T) {
	for _, ty := range []models.DatasourceType{models.DatasourceGmail, models.DatasourcePostgres} {
		e := Entities(ty)
		if e == nil || len(e) != 0 {
			t.Fatalf("%s: %v", ty, e)
		}
	}
}

func TestJSONShape(t *testing.T) {
	b, _ := json.Marshal(Entities(models.DatasourceLinear))
	s := string(b)
	if !strings.Contains(s, `"actions":[]`) || !strings.Contains(s, `"key":"issues"`) || !strings.Contains(s, `"default":true`) {
		t.Fatal(s)
	}
	b, _ = json.Marshal(Entities(models.DatasourceGmail))
	if string(b) != "[]" {
		t.Fatal(string(b))
	}
}
