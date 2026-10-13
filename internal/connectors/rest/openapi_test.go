package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
)

const petsYAML = `
openapi: 3.0.0
info: {title: Pets, version: "1"}
paths:
  /pets:
    get:
      operationId: listPets
      summary: List pets
      parameters:
        - {name: page, in: query, schema: {type: integer}}
        - {name: per_page, in: query, schema: {type: integer}}
        - {name: status, in: query, schema: {type: string}}
        - {name: X-Trace, in: header, schema: {type: string}}
        - {name: blob, in: query, content: {application/json: {schema: {type: object}}}}
        - $ref: '#/components/parameters/Foo'
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items: {$ref: '#/components/schemas/Pet'}
                  total: {type: integer}
    post:
      responses:
        default:
          description: created
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Pet'}
    put:
      responses: {}
  /pets/{id}:
    $ref: '#/components/pathItems/Pet'
  /owners:
    get:
      parameters:
        - {name: offset, in: query, schema: {type: integer}}
        - {name: limit, in: query, schema: {type: integer}}
      responses:
        200:
          content:
            text/plain:
              schema:
                type: array
                items:
                  type: object
                  properties:
                    name: {type: string}
components:
  schemas:
    Pet:
      type: object
      properties:
        id: {type: integer, description: Pet id}
        name: {type: string}
        weight: {type: number}
        born: {type: string, format: date}
        updated: {type: string, format: date-time}
        vaccinated: {type: boolean}
        tags: {type: array, items: {type: string}}
        owner: {$ref: '#/components/schemas/Owner'}
        misc: {oneOf: [{type: string}]}
    Owner:
      $ref: '#/components/schemas/Pet'
`

func TestParseSpecYAML(t *testing.T) {
	eps, err := ParseSpecBody(petsYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("%d endpoints", len(eps))
	}
	get := eps[0]
	if get.Path != "/pets" || get.Method != "GET" || *get.OperationID != "listPets" || *get.Summary != "List pets" || get.Description != nil {
		t.Fatalf("%+v", get)
	}
	d := get.Draft
	if d.ListPath == nil || *d.ListPath != "/data" {
		t.Fatal(d.ListPath)
	}
	var names []string
	for _, f := range d.Schema.Fields {
		names = append(names, f.Name+":"+string(f.FieldType))
	}
	want := "id:integer,name:string,weight:float,born:date_time,updated:date_time,vaccinated:boolean,tags:json,owner:json,misc:json"
	if strings.Join(names, ",") != want {
		t.Fatalf("\n got %s\nwant %s", strings.Join(names, ","), want)
	}
	if *d.Schema.Fields[0].Description != "Pet id" || d.Schema.Fields[7].Description != nil {
		t.Fatal("descriptions")
	}
	if d.Pagination != (Pagination{Kind: PaginationPage, PageParam: "page", SizeParam: "per_page", StartPage: 1}) {
		t.Fatalf("%+v", d.Pagination)
	}
	if len(d.FilterMapping) != 3 || d.FilterMapping["status"].Param != "status" || d.FilterMapping["status"].Operators[0] != "equals" {
		t.Fatalf("%+v", d.FilterMapping)
	}
	if _, ok := d.FilterMapping["blob"]; ok {
		t.Fatal("content-typed param should be skipped")
	}

	post := eps[1]
	// Pet has exactly one array property (tags), so — like the Rust heuristic —
	// the list is taken to be /tags, whose string items yield no fields.
	if post.Method != "POST" || *post.Draft.ListPath != "/tags" || len(post.Draft.Schema.Fields) != 0 || post.Draft.Pagination.Kind != PaginationNone {
		t.Fatalf("%+v", post.Draft)
	}

	owners := eps[2]
	if owners.Path != "/owners" || owners.Draft.ListPath != nil || len(owners.Draft.Schema.Fields) != 1 {
		t.Fatalf("%+v", owners.Draft)
	}
	if owners.Draft.Pagination != (Pagination{Kind: PaginationOffset, LimitParam: "limit", OffsetParam: "offset"}) {
		t.Fatalf("%+v", owners.Draft.Pagination)
	}

	b, _ := json.Marshal(get)
	if !strings.Contains(string(b), `"operation_id":"listPets"`) || !strings.Contains(string(b), `"description":null`) {
		t.Fatal(string(b))
	}
}

func TestParseSpecJSONKeepsOrder(t *testing.T) {
	spec := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{
	  "/z":{"get":{"responses":{"201":{"content":{"application/json":{"schema":{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"integer"}}}}}}}}},
	  "/a":{"get":{"parameters":[{"name":"offset","in":"query","schema":{}},{"name":"per_page","in":"query","schema":{}}],
	    "responses":{"404":{},"2XX":{}}}}}}`
	eps, err := ParseSpecBody(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0].Path != "/z" || eps[1].Path != "/a" {
		t.Fatalf("%+v", eps)
	}
	if f := eps[0].Draft.Schema.Fields; len(f) != 2 || f[0].Name != "b" || f[1].Name != "a" {
		t.Fatalf("%+v", f)
	}
	if eps[1].Draft.Pagination.LimitParam != "per_page" || len(eps[1].Draft.Schema.Fields) != 0 {
		t.Fatalf("%+v", eps[1].Draft)
	}
}

func TestExtractItemSchemaEdgeCases(t *testing.T) {
	spec := `
openapi: 3.0.0
info: {title: t, version: "1"}
paths:
  /two-arrays:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
                properties:
                  a: {type: array}
                  b: {type: array}
  /ref-resp:
    get:
      responses:
        "200": {$ref: '#/components/responses/X'}
  /no-content:
    get:
      responses:
        "200": {description: x}
  /no-schema:
    get:
      responses:
        "200": {content: {application/json: {}}}
  /bad-ref:
    get:
      responses:
        "200": {content: {application/json: {schema: {$ref: 'other.yaml#/X'}}}}
  /scalar:
    get:
      responses:
        "200": {content: {application/json: {schema: {type: string}}}}
  /array-no-items:
    get:
      responses:
        "200": {content: {application/json: {schema: {type: array}}}}
  /array-of-scalars:
    get:
      responses:
        "200": {content: {application/json: {schema: {type: array, items: {type: string}}}}}
`
	eps, err := ParseSpecBody(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 8 {
		t.Fatal(len(eps))
	}
	if len(eps[0].Draft.Schema.Fields) != 2 || eps[0].Draft.ListPath != nil {
		t.Fatalf("two arrays: %+v", eps[0].Draft)
	}
	for _, e := range eps[1:] {
		if len(e.Draft.Schema.Fields) != 0 || e.Draft.ListPath != nil || e.Draft.Schema.Fields == nil {
			t.Fatalf("%s: %+v", e.Path, e.Draft)
		}
	}
}

func TestSpecErrors(t *testing.T) {
	var se *SpecError
	_, err := ParseSpec(context.Background(), Spec{Kind: "openapi", Inline: strp("  ")})
	if !errors.As(err, &se) || se.Kind != SpecEmptySource || err.Error() != "spec is empty or has no 'url' / 'inline' source" {
		t.Fatal(err)
	}
	for _, body := range []string{`{"swagger":"2.0","info":{}}`, `[1,2]`, "a: [", `{"openapi":"3.0.0"}`, ""} {
		_, err := ParseSpecBody(body)
		if !errors.As(err, &se) || se.Kind != SpecParse || !strings.HasPrefix(err.Error(), "spec parse failed: ") {
			t.Fatalf("%q: %v", body, err)
		}
	}
}

func TestParseSpecFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(petsYAML))
	}))
	defer srv.Close()
	eps, err := ParseSpec(context.Background(), Spec{Kind: "openapi", URL: strp(srv.URL + "/spec.yaml")})
	if err != nil || len(eps) != 3 {
		t.Fatal(eps, err)
	}
	var se *SpecError
	_, err = ParseSpec(context.Background(), Spec{Kind: "openapi", URL: strp(srv.URL + "/missing")})
	if !errors.As(err, &se) || se.Kind != SpecFetch || err.Error() != "spec fetch failed: HTTP 404 Not Found" {
		t.Fatal(err)
	}
	_, err = ParseSpec(context.Background(), Spec{Kind: "openapi", URL: strp("http://127.0.0.1:1/x")})
	if !errors.As(err, &se) || se.Kind != SpecFetch {
		t.Fatal(err)
	}
	_, err = ParseSpec(context.Background(), Spec{Kind: "openapi", URL: strp("::bad")})
	if !errors.As(err, &se) || se.Kind != SpecFetch {
		t.Fatal(err)
	}
	// Inline wins over URL.
	eps, err = ParseSpec(context.Background(), Spec{Kind: "openapi", URL: strp("::bad"), Inline: strp(petsYAML)})
	if err != nil || len(eps) != 3 {
		t.Fatal(err)
	}
	_ = connector.FieldJSON
}
