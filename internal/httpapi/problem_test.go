package httpapi

// The problem body's field ORDER, held against `openapi/v1.yaml` at run time.
//
// `problem.go`'s header says `problem.Problem` is hand-written in the document's
// field order so `encoding/json` renders it correctly, and that if this package
// ever marshals a GENERATED struct instead, oapi-codegen's alphabetical order
// comes back and every client parses it identically anyway — which is exactly
// why it would ship. `TestTheProblemBodyIsInTheDocumentOrder` is what stops that,
// and this file is it. `grep -rn TestTheProblemBodyIsInTheDocumentOrder` used to
// return this header's own claim and nothing else.
//
// # WHY THE EXPECTED ORDER IS NOT WRITTEN DOWN HERE
//
// The obvious assertion is a literal:
//
//	[]string{"type", "title", "status", "detail", "instance", "code", "trace_id"}
//
// It passes today and it is worth nothing. It is a second copy of the struct's
// own field order: a document that reordered its properties would leave it green,
// and a struct that was reordered to match such a document edit would go red for
// a reason this test has nothing to do with. So the expected order is READ from
// `openapi/v1.yaml` at run time, the way `routes_test.go` reads the same document
// for its own drift checks, and the only thing written down in this file is the
// rule that ties the two together.
//
// # WHY THE ORDER IS READ FROM THE BYTES AND NOT FROM A DECODED VALUE
//
// `json.Unmarshal` into a `map[string]any` cannot answer this question at all.
// Go maps are unordered and `encoding/json` SORTS map keys on the way out, so a
// map round trip normalises precisely the thing under test: the assertion would
// pass against any order whatsoever, and it would pass against the alphabetical
// order that is the failure this exists to catch. `json.Decoder.Token` streams
// tokens in the order the bytes carry them, which is the only reading of a
// marshalled object that keeps the answer.
//
// # AND WHY EVERY CASE GOES THROUGH THE WRITER
//
// `problem.Problem` is a struct, and `json.Marshal`ing one this test built itself
// would prove the STRUCT's order rather than the WRITER's. The claim in
// `problem.go` is about what goes on the wire, so the cases below drive
// `writeProblemError` — and `writeStatus`, which marshals the same struct on the
// 413 path — over a recorder, and assert on the bytes those produced.
//
// # PLANTED, TWICE, BEFORE BELIEVED
//
// AGENTS.md's rule is that a guard nobody has seen go red is a guard nobody can
// rely on, so both halves of this one were planted and run:
//
//   - `problem.Problem`'s fields sorted alphabetically, which is what
//     oapi-codegen writes and therefore the trap `problem.go` names. This test
//     reports the document's order against `code, detail, errors, instance,
//     status, title, trace_id, type`.
//   - `openapi/v1.yaml`'s `Problem.properties` with `code` moved above `detail`,
//     which is a document edit that changes no route and no schema. This test
//     goes red on it, which is what proves the expected order is the DOCUMENT's
//     and not a literal typed into this file.
//
// Both were reverted; the tree this commit lands is the green one.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cafaye/darkroom/internal/problem"
)

// openapiPath is relative to this package's directory, for the same measured
// reason `routes_test.go` states: the test's working directory is the package
// directory and not the module root.
//
// A second copy rather than a shared one, and the duplication is deliberate: the
// constant `routes_test.go` declares belongs to the EXTERNAL test package, and an
// internal test file cannot see it. Moving this file out to `httpapi_test` would
// mean driving the router rather than `writeProblemError`, which is a different
// test — see the header.
const openapiPath = "../../openapi/v1.yaml"

// problemSchema is what `openapi/v1.yaml` says about `components.schemas.Problem`,
// in the document's own order.
type problemSchema struct {
	// properties is `properties:` in the order the document writes them. THE order
	// under test: a YAML mapping node keeps it, and `map[string]…` does not.
	properties []string

	// required is `required:`, which is a list and therefore ordered in the
	// document too. It is not the order the body is expected to render in — the
	// document's `properties` order is — but the two are checked against each
	// other, because a document where they disagree has two answers to "what order
	// is this" and this test would be silently picking one.
	required []string
}

// readProblemSchema reads `components.schemas.Problem` out of the document.
//
// # WHY `yaml.Node` AND NOT A MAP
//
// `yaml.Unmarshal` into a struct with a `map[string]…` field is the shape
// `routes_test.go` uses, and it is the right shape there because a route has no
// order. Here the order IS the assertion, and a map has already discarded it by
// the time the test looks — the same reason `encoding/json`'s map decoding cannot
// be used on the way in here either.
//
// # AND WHY IT FAILS LOUDLY RATHER THAN RETURNING AN EMPTY SCHEMA
//
// Every comparison below is between the document's order and the body's, so a
// parser that found nothing would make all of them pass vacuously. Each of the
// three failures it can have — no `Problem` schema, no `properties`, no
// `required` — names itself.
func readProblemSchema(t *testing.T) problemSchema {
	t.Helper()

	raw, err := os.ReadFile(openapiPath)
	if err != nil {
		t.Fatalf("cannot read %s: %v", openapiPath, err)
	}

	var parsed yaml.Node
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("cannot parse %s: %v", openapiPath, err)
	}

	// A `yaml.Node` target receives the DOCUMENT node, whose single child is the
	// root mapping. Stepping to it explicitly rather than assuming either shape is
	// what the "found node kind 1" failure below would otherwise be about.
	document := rootMapping(t, &parsed)

	schema := child(t, document, "components")
	schema = child(t, schema, "schemas")
	schema = child(t, schema, "Problem")

	// A `$ref` here would mean the shape lives elsewhere in the document and every
	// walk below is looking at a pointer rather than at the schema. It is not
	// refused as impossible — a future core version could introduce one — it is
	// refused because following it is not this test's job, and pretending to have
	// read a shape it never opened would be the worst of the three answers.
	if schema.Kind == yaml.ScalarNode && strings.HasPrefix(schema.Value, "$ref") {
		t.Fatalf("%s resolves components.schemas.Problem to %q; this test reads the schema "+
			"inline and would be asserting against a pointer rather than the shape",
			openapiPath, schema.Value)
	}

	properties := child(t, schema, "properties")
	out := problemSchema{properties: yamlKeysInOrder(properties)}
	if len(out.properties) == 0 {
		t.Fatalf("%s declares components.schemas.Problem with no properties, so every "+
			"order comparison below passes vacuously", openapiPath)
	}

	required := child(t, schema, "required")
	for _, name := range required.Content {
		out.required = append(out.required, name.Value)
	}
	if len(out.required) == 0 {
		t.Fatalf("%s declares components.schemas.Problem with no required properties, so "+
			"there is no set of keys a body must carry and the order check is not "+
			"checking that anything arrived", openapiPath)
	}

	// The document must give ONE answer. Every required name has to be a property,
	// and the `required` list has to read in the same sequence as the `properties`
	// order filtered to the required names — otherwise this file is choosing
	// between two lists the document spells differently, and the choice would be
	// invisible in the assertion.
	var filtered []string
	for _, name := range out.properties {
		if slices.Contains(out.required, name) {
			filtered = append(filtered, name)
		}
	}
	if !slices.Equal(filtered, out.required) {
		t.Fatalf("%s's Problem.required is %v but its Problem.properties in order "+
			"is %v, so the required names read in property order as %v. The document "+
			"declares two orders and this test refuses to pick one silently.",
			openapiPath, out.required, out.properties, filtered)
	}

	return out
}

// rootMapping is the document's root mapping node.
//
// `yaml.Unmarshal` into a `yaml.Node` yields the DOCUMENT node — kind 1 — and the
// root mapping is its one child. Both shapes are accepted rather than only this
// one, because a future yaml.v3 handing back the mapping directly would otherwise
// make this fail with "expected a mapping, found node kind 1", which says nothing
// about the document.
func rootMapping(t *testing.T, node *yaml.Node) *yaml.Node {
	t.Helper()

	if node.Kind == yaml.MappingNode {
		return node
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 && node.Content[0].Kind == yaml.MappingNode {
		return node.Content[0]
	}
	t.Fatalf("%s parsed to node kind %d, which is neither a mapping nor a document "+
		"holding one, so nothing below could be read out of it", openapiPath, node.Kind)
	return nil
}

// child is the value at `key` in a YAML mapping node.
//
// Fails rather than returning nil, so a caller cannot walk a nil pointer and get
// a panic that names a line in this file instead of the document.
func child(t *testing.T, node *yaml.Node, key string) *yaml.Node {
	t.Helper()

	if node.Kind != yaml.MappingNode {
		t.Fatalf("%s: expected a mapping while looking for %q, found node kind %d", openapiPath, key, node.Kind)
	}
	// Mapping content is key, value, key, value — so `i` walks keys and `i+1` the
	// value under it. Stepping by two rather than one is what makes this the value
	// and not the next key.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	t.Fatalf("%s has no %q at this level, so the walk below is looking at the wrong node", openapiPath, key)
	return nil
}

// yamlKeysInOrder is a YAML mapping node's keys, in the order the document writes
// them.
func yamlKeysInOrder(node *yaml.Node) []string {
	out := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		out = append(out, node.Content[i].Value)
	}
	return out
}

// bodyKeys is a JSON object's top-level keys, in the order the BYTES carry them.
//
// `json.Decoder.Token` rather than `json.Unmarshal`, for the reason in the header:
// tokens stream in the order they were written, and a map would have sorted them.
func bodyKeys(t *testing.T, body []byte) []string {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, body)
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		t.Fatalf("the body is not a JSON object, so it has no key order: %s", body)
	}

	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("reading a key: %v\n%s", err, body)
		}
		name, ok := token.(string)
		if !ok {
			t.Fatalf("expected a key, found %v in %s", token, body)
		}
		keys = append(keys, name)

		// The value is stepped over rather than decoded. The question is the ORDER
		// of the names, and `errors[]` is the one nested value in the envelope, so
		// reading depth-aware is enough and a nested key cannot be mistaken for a
		// top-level one.
		if err := skipOneValue(decoder); err != nil {
			t.Fatalf("reading the value of %q: %v\n%s", name, err, body)
		}
	}

	if closing, err := decoder.Token(); err != nil {
		t.Fatalf("reading the closing brace: %v\n%s", err, body)
	} else if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		t.Fatalf("the body did not close where a top-level object should: %s", body)
	}
	return keys
}

// skipOneValue consumes exactly one JSON value, whatever its shape.
//
// A value is either a scalar — one token — or an object or array, which opens
// with a delimiter and closes with the matching one after however many tokens are
// inside it. Depth is counted rather than the shape matched, so `errors[]`'s
// objects are stepped over with the same code that would step over a nested one.
func skipOneValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}

	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// TestTheProblemBodyIsInTheDocumentOrder is `problem.go`'s claim, asserted.
//
// The expected keys are the document's `required` list plus whatever else the
// case's own error carries, read out of `openapi/v1.yaml` above. Nothing about
// the order is written down in this file.
//
// The three cases are the three renderings there are, and the 422 and the 500 are
// the pair that makes the `errors` decision deliberate rather than convenient:
// `errors` is `omitempty` and is NOT in the document's `required`, so the 422 is
// expected to carry it in document position (last, after `trace_id`) and the 500
// is expected not to carry it at all. Demanding `errors` everywhere would fail
// on the 500; ignoring it would let the struct drop it from the 422 and still
// pass. The 413 is the third rendering because `writeStatus` marshals the same
// struct through a different function, and a property that only the one writer
// had would be worth knowing about.
func TestTheProblemBodyIsInTheDocumentOrder(t *testing.T) {
	schema := readProblemSchema(t)

	// Built from the document, never from `problem.Problem`'s own field list, so
	// that the two are compared rather than one being a copy of the other.
	withErrors := append(slices.Clone(schema.required), "errors")

	for _, row := range []struct {
		name     string
		err      *problem.Error
		wantKeys []string
		status   int
	}{
		{
			name:     "422 carries errors, last",
			err:      problem.InvalidFields("the checksum did not match", problem.NewField("checksum", "mismatch")),
			wantKeys: withErrors,
			status:   http.StatusUnprocessableEntity,
		},
		{
			name:     "500 omits errors entirely",
			err:      problem.Internal("the request could not be completed"),
			wantKeys: slices.Clone(schema.required),
			status:   http.StatusInternalServerError,
		},
		{
			// `writeStatus`, not `writeProblemError`: 413 is the one status the
			// registry does not carry (`payload_too_large` is not a core code), so
			// this envelope is rendered by the other writer and must render the same
			// way. `errors` is present because `problemPayloadTooLarge` builds a
			// field error — an `omitempty` key that this case proves is not merely
			// dropped on every path.
			name:     "413 written by writeStatus",
			err:      problemPayloadTooLarge(4096),
			wantKeys: withErrors,
			status:   http.StatusRequestEntityTooLarge,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)

			if row.status == http.StatusRequestEntityTooLarge {
				writeStatus(recorder, request, row.status, row.err)
			} else {
				writeProblemError(recorder, request, row.err)
			}

			if recorder.Code != row.status {
				t.Fatalf("the writer answered %d, want %d: %s", recorder.Code, row.status, recorder.Body.Bytes())
			}

			got := bodyKeys(t, recorder.Body.Bytes())

			// Two failures, on purpose. A missing required key and a wrong order are
			// different defects: the first is a body that is not the envelope at all,
			// and reporting it as "the order is wrong" would send a reader to compare
			// two sequences when the real answer is that one has fewer keys.
			for _, name := range row.wantKeys {
				if !slices.Contains(got, name) {
					t.Errorf("the body has no %q, which the document requires and this case "+
						"expects; it renders %v", name, got)
				}
			}

			if !slices.Equal(got, row.wantKeys) {
				t.Errorf("the body's keys are in the wrong order.\n\n got:  %v\n want: %v\n\n"+
					"the document declares components.schemas.Problem in %v, so a body in any "+
					"other order means something is marshalling a struct whose field order is "+
					"not the document's — which is what a GENERATED struct gives you, because "+
					"oapi-codegen writes fields alphabetically.\n\nbody: %s",
					got, row.wantKeys, schema.properties, recorder.Body.Bytes())
			}
		})
	}
}
