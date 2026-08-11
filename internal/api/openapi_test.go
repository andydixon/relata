package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The OpenAPI specification is written by hand, so it can drift from the code.
// This test compares it against the routes the router actually registers, which
// is the thing the specification claims to describe.
//
// The route list below is checked against router.go itself, so a route added
// there without a corresponding entry here also fails.

// specPath is the specification under test.
const specPath = "../../docs/openapi.yaml"

// routerPath is the file whose route registrations are the source of truth.
const routerPath = "router.go"

// apiRoutePattern matches the API routes registered in the router.
var apiRoutePattern = regexp.MustCompile(`mux\.Handle\("(GET|POST|PATCH|DELETE|PUT) (/api/v1[^"]*|/health/[^"]*)"`)

// specPathPattern matches top-level path keys in the specification.
var specPathPattern = regexp.MustCompile(`(?m)^  (/(?:api/v1|health)[^:]*):`)

// specMethodPattern matches an HTTP method under a path.
var specMethodPattern = regexp.MustCompile(`(?m)^    (get|post|patch|delete|put):`)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// routerRoutes returns "METHOD /path" for every API route the router registers.
func routerRoutes(t *testing.T) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, m := range apiRoutePattern.FindAllStringSubmatch(readFile(t, routerPath), -1) {
		out[m[1]+" "+m[2]] = true
	}
	if len(out) == 0 {
		t.Fatal("no routes were found in router.go; the matcher needs updating")
	}
	return out
}

// specRoutes returns "METHOD /path" for every operation in the specification.
func specRoutes(t *testing.T) map[string]bool {
	t.Helper()
	spec := readFile(t, specPath)

	// Split the document at each path key, so methods are attributed correctly.
	locs := specPathPattern.FindAllStringSubmatchIndex(spec, -1)
	out := map[string]bool{}

	for i, loc := range locs {
		path := spec[loc[2]:loc[3]]
		end := len(spec)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		for _, m := range specMethodPattern.FindAllStringSubmatch(spec[loc[1]:end], -1) {
			out[strings.ToUpper(m[1])+" "+path] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("no operations were found in the specification; the matcher needs updating")
	}
	return out
}

func TestOpenAPISpecMatchesRegisteredRoutes(t *testing.T) {
	router := routerRoutes(t)
	spec := specRoutes(t)

	var missing, extra []string
	for route := range router {
		if !spec[route] {
			missing = append(missing, route)
		}
	}
	for route := range spec {
		if !router[route] {
			extra = append(extra, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("these routes exist but are not documented in %s:\n  %s",
			specPath, strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("these routes are documented in %s but are not registered:\n  %s",
			specPath, strings.Join(extra, "\n  "))
	}
}

// The specification enumerates several closed sets that also exist as Go
// constants. A value added in one place and not the other would make the
// specification wrong in a way clients would act on.
func TestOpenAPIEnumsMatchTheDomain(t *testing.T) {
	spec := readFile(t, specPath)

	cases := []struct {
		name   string
		values []string
	}{
		{"relationship types", []string{
			"similar_to", "mentions", "involves", "supports", "contradicts", "updates",
			"supersedes", "provides_context_for", "possibly_explains", "occurred_before",
			"occurred_after", "derived_from", "same_entity_as",
		}},
		{"provenance", []string{
			"explicit", "deterministic", "model_inferred", "user_confirmed", "user_rejected",
		}},
		{"entity types", []string{
			"person", "organisation", "location", "product", "event", "date", "subject", "concept",
		}},
		{"source types", []string{
			"note", "document", "json", "email", "transcript", "markdown",
		}},
		{"job types", []string{
			"parse_item", "chunk_item", "generate_embeddings", "extract_entities",
			"extract_claims", "discover_relationships", "reprocess_item",
			"delete_item_data", "reembed_workspace",
		}},
		{"error codes", []string{
			CodeInvalidRequest, CodeValidationFailed, CodeUnauthorised, CodeForbidden,
			CodeNotFound, CodeDuplicate, CodeConflict, CodePayloadTooLarge,
			CodeUnsupportedMedia, CodeRateLimited, CodeCSRFFailed, CodeDependencyDown,
			CodeModelInvalid, CodeFeatureDisabled, CodeInternal,
		}},
		{"signal types", []string{
			"semantic_similarity", "shared_entity", "alias_entity", "shared_concept",
			"temporal_proximity", "shared_metadata", "claim_support", "claim_contradiction",
			"model_classification", "user_confirmation", "user_rejection", "content_hash_match",
		}},
	}

	for _, c := range cases {
		for _, v := range c.values {
			if !strings.Contains(spec, v) {
				t.Errorf("%s: %q is not documented in the specification", c.name, v)
			}
		}
	}
}

// The specification must not describe scores as probabilities, since the whole
// design rests on their not being probabilities.
func TestOpenAPIDescribesScoresHonestly(t *testing.T) {
	spec := readFile(t, specPath)

	for _, required := range []string{
		"NOT a calibrated probability",
		"Not a probability",
		"not a fact",
	} {
		if !strings.Contains(spec, required) {
			t.Errorf("the specification does not state %q", required)
		}
	}
	// A claim that would be untrue.
	for _, forbidden := range []string{"probability that", "confidence interval", "calibrated probability that"} {
		if strings.Contains(strings.ToLower(spec), strings.ToLower(forbidden)) {
			t.Errorf("the specification contains the misleading phrase %q", forbidden)
		}
	}
}
