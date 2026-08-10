package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// --- JSON decoding ---------------------------------------------------------

type testBody struct {
	Title string `json:"title"`
	Count int    `json:"count"`
}

func postJSON(body string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return httptest.NewRecorder(), r
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	w, r := postJSON(`{"title":"a","count":1,"is_admin":true}`)

	var dst testBody
	err := DecodeJSON(w, r, 1<<20, &dst)
	if err == nil {
		t.Fatal("an unknown field was silently ignored")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("wrong error type: %T", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Code != CodeValidationFailed {
		t.Fatalf("got %d/%s, want 400/%s", apiErr.Status, apiErr.Code, CodeValidationFailed)
	}
	if len(apiErr.Fields) == 0 || apiErr.Fields[0].Field != "is_admin" {
		t.Fatalf("the offending field was not identified: %+v", apiErr.Fields)
	}
}

func TestDecodeJSONRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"truncated":     `{"title":"a"`,
		"not json":      `title=a`,
		"array":         `[1,2,3]`,
		"empty":         ``,
		"two documents": `{"title":"a"}{"title":"b"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w, r := postJSON(body)
			var dst testBody
			if err := DecodeJSON(w, r, 1<<20, &dst); err == nil {
				t.Fatalf("malformed body %q was accepted", body)
			}
		})
	}
}

func TestDecodeJSONEnforcesSizeLimit(t *testing.T) {
	big := `{"title":"` + strings.Repeat("x", 5000) + `"}`
	w, r := postJSON(big)

	var dst testBody
	err := DecodeJSON(w, r, 1024, &dst)
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body not rejected with 413: %v", err)
	}
}

func TestDecodeJSONRequiresJSONContentType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(`{"title":"a"}`))
	r.Header.Set("Content-Type", "text/html")
	w := httptest.NewRecorder()

	var dst testBody
	err := DecodeJSON(w, r, 1<<20, &dst)
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Status != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content type accepted: %v", err)
	}
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- Error responses -------------------------------------------------------

func TestErrorResponsesNeverLeakInternalDetail(t *testing.T) {
	// An internal error must not surface the underlying message, which could
	// contain a connection string, a file path or schema detail.
	internal := Errorf(http.StatusInternalServerError, CodeInternal, "An unexpected error occurred.")
	internal.Internal = errNamed("mongodb://user:hunter2@db:27017 connection refused: /var/lib/secret")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
	w := httptest.NewRecorder()
	WriteError(w, r, internal)

	body := w.Body.String()
	for _, forbidden := range []string{"hunter2", "mongodb://", "/var/lib/secret", "goroutine"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the response leaked internal detail %q:\n%s", forbidden, body)
		}
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestErrorResponsesUseStableCodes(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{domain.ErrNotFound, http.StatusNotFound, CodeNotFound},
		{domain.ErrForbidden, http.StatusForbidden, CodeForbidden},
		{domain.ErrDuplicate, http.StatusConflict, CodeDuplicate},
		{domain.ErrTooLarge, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{domain.ErrUnsupported, http.StatusUnsupportedMediaType, CodeUnsupportedMedia},
		// A cross-workspace reference must look identical to a missing record,
		// so that probing cannot confirm an ID exists elsewhere.
		{domain.ErrWorkspaceMiss, http.StatusNotFound, CodeNotFound},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/items/x", nil)
		w := httptest.NewRecorder()
		WriteError(w, r, c.err)

		if w.Code != c.wantStatus {
			t.Errorf("%v → status %d, want %d", c.err, w.Code, c.wantStatus)
		}
		var resp ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if resp.Error.Code != c.wantCode {
			t.Errorf("%v → code %q, want %q", c.err, resp.Error.Code, c.wantCode)
		}
	}
}

func TestWorkspaceMismatchIsIndistinguishableFromNotFound(t *testing.T) {
	render := func(err error) string {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/items/x", nil)
		w := httptest.NewRecorder()
		WriteError(w, r, err)
		return w.Body.String()
	}
	if render(domain.ErrNotFound) != render(domain.ErrWorkspaceMiss) {
		t.Fatal("a cross-workspace access is distinguishable from a missing record, " +
			"which lets an attacker confirm that an ID exists in another workspace")
	}
}

type namedErr string

func (e namedErr) Error() string { return string(e) }
func errNamed(s string) error    { return namedErr(s) }

// --- Security headers ------------------------------------------------------

func TestSecurityHeadersAreRestrictive(t *testing.T) {
	h := SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q: %s", want, csp)
		}
	}
	// Inline script and remote origins must not be permitted: they are how a
	// stored payload would execute or exfiltrate.
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "*", "http:"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP permits %q: %s", forbidden, csp)
		}
	}

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
		"Cache-Control":          "no-store",
	}
	for header, want := range checks {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// --- Request IDs -----------------------------------------------------------

func TestRequestIDRejectsClientSuppliedGarbage(t *testing.T) {
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// A crafted value must not end up in log fields or the response header.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "not-a-uuid\ninjected=evil")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	got := w.Header().Get("X-Request-ID")
	if strings.Contains(got, "injected") || strings.Contains(got, "\n") {
		t.Fatalf("a crafted request ID was echoed back: %q", got)
	}
	if len(got) != 36 {
		t.Fatalf("no valid request ID was generated: %q", got)
	}
}

func TestRequestIDAcceptsAValidUUID(t *testing.T) {
	const valid = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", valid)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("X-Request-ID"); got != valid {
		t.Fatalf("a valid client request ID was discarded: %q", got)
	}
}

// --- Body limits -----------------------------------------------------------

func TestMaxBodyRejectsOversizedRequests(t *testing.T) {
	h := MaxBody(100)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 500)))
	r.ContentLength = 500
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

// --- CSRF ------------------------------------------------------------------

func csrfHandler(session *domain.Session) http.Handler {
	c := CSRF{TrustedOrigins: []string{"http://127.0.0.1:8080"}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if session != nil {
			r = r.WithContext(withSession(r.Context(), session))
		}
		c.Middleware()(inner).ServeHTTP(w, r)
	})
}

func TestCSRFAllowsSafeMethods(t *testing.T) {
	sess := &domain.Session{CSRFToken: "the-token"}
	h := csrfHandler(sess)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(method, "/items", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s was refused: %d", method, w.Code)
		}
	}
}

func TestCSRFBlocksStateChangeWithoutToken(t *testing.T) {
	sess := &domain.Session{CSRFToken: "the-token"}
	h := csrfHandler(sess)

	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a POST without a CSRF token returned %d, want 403", w.Code)
	}
}

func TestCSRFBlocksWrongToken(t *testing.T) {
	sess := &domain.Session{CSRFToken: "the-token"}
	h := csrfHandler(sess)

	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("X-CSRF-Token", "the-wrong-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a POST with the wrong CSRF token returned %d, want 403", w.Code)
	}
}

func TestCSRFBlocksForeignOrigin(t *testing.T) {
	sess := &domain.Session{CSRFToken: "the-token"}
	h := csrfHandler(sess)

	// Even with the correct token, a request claiming another origin is refused.
	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	r.Header.Set("X-CSRF-Token", "the-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin POST returned %d, want 403", w.Code)
	}
}

func TestCSRFAcceptsCorrectTokenAndOrigin(t *testing.T) {
	sess := &domain.Session{CSRFToken: "the-token"}
	h := csrfHandler(sess)

	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("X-CSRF-Token", "the-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("a valid request returned %d, want 200", w.Code)
	}
}

func TestCSRFRequiresASession(t *testing.T) {
	h := csrfHandler(nil)

	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("X-CSRF-Token", "anything")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated POST returned %d, want 401", w.Code)
	}
}

func TestCSRFDerivesOriginFromReferer(t *testing.T) {
	sess := &domain.Session{CSRFToken: "the-token"}
	h := csrfHandler(sess)

	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	r.Header.Set("Referer", "https://evil.example.com/some/page?x=1")
	r.Header.Set("X-CSRF-Token", "the-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a foreign Referer returned %d, want 403", w.Code)
	}
}

// --- Authorisation ---------------------------------------------------------

func TestRequireWriteRejectsViewers(t *testing.T) {
	h := RequireWrite()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := map[domain.Role]int{
		domain.RoleViewer: http.StatusForbidden,
		domain.RoleEditor: http.StatusOK,
		domain.RoleAdmin:  http.StatusOK,
	}
	for role, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/items", nil)
		r = r.WithContext(WithPrincipal(r.Context(), &domain.Principal{
			UserID: "u", WorkspaceID: "ws", Role: role,
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("role %s got %d, want %d", role, w.Code, want)
		}
	}
}

func TestRequireWriteRejectsAnonymous(t *testing.T) {
	h := RequireWrite()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/items", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("an anonymous write returned %d, want 403", w.Code)
	}
}

// --- Panic recovery --------------------------------------------------------

func TestRecoverConvertsPanicToCleanError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("a secret value hunter2 appeared in the panic")
	}), RequestID(), Recover())

	r := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
	r = r.WithContext(observability.WithLogger(r.Context(), logger))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("the panic value reached the client:\n%s", w.Body.String())
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Fatalf("the panic value reached the logs:\n%s", logs.String())
	}
}

func TestSearchQueryRejectsNegativeOffsetAsZero(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test&offset=-7", nil)
	q, err := searchQueryFromRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if q.Offset != 0 {
		t.Fatalf("offset = %d, want 0; a negative value can panic slice paging", q.Offset)
	}
}

// --- Pagination ------------------------------------------------------------

func TestPaginationIsBounded(t *testing.T) {
	cases := []struct {
		query           string
		wantLimit       int
		wantOffsetAtLst int
	}{
		{"", 20, 0},
		{"?limit=5&offset=10", 5, 10},
		{"?limit=99999", 200, 0}, // clamped to the maximum
		{"?limit=-1", 20, 0},     // falls back to the default
		{"?offset=-5", 20, 0},    // negative offsets are clamped
		{"?limit=abc", 20, 0},    // unparseable values fall back
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/items"+c.query, nil)
		p := pageFromQuery(r)
		if p.Limit != c.wantLimit {
			t.Errorf("%q → limit %d, want %d", c.query, p.Limit, c.wantLimit)
		}
		if p.Offset != c.wantOffsetAtLst {
			t.Errorf("%q → offset %d, want %d", c.query, p.Offset, c.wantOffsetAtLst)
		}
	}
}

// --- Query filter validation ----------------------------------------------

// A filter value that MongoDB would treat as an operator must be rejected as
// an invalid enum long before it reaches a query.
func TestFiltersRejectOperatorShapedValues(t *testing.T) {
	hostile := []string{
		`$ne`, `{"$ne":null}`, `$where`, `$gt`, `[$ne]`, `{"$regex":".*"}`,
	}
	for _, v := range hostile {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/items?source_type="+v, nil)
		if _, err := itemFilterFromQuery(r); err == nil {
			t.Errorf("source_type=%q was accepted", v)
		}

		r = httptest.NewRequest(http.MethodGet, "/api/v1/items?type="+v, nil)
		if _, err := relFilterFromQuery(r); err == nil {
			t.Errorf("relationship type=%q was accepted", v)
		}
	}
}

func TestFiltersAcceptValidValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/items?source_type=note,document&status=ready&created_from=2025-01-01", nil)
	f, err := itemFilterFromQuery(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.SourceTypes) != 2 {
		t.Fatalf("expected 2 source types, got %d", len(f.SourceTypes))
	}
	if f.CreatedFrom == nil || f.CreatedFrom.Year() != 2025 {
		t.Fatal("created_from was not parsed")
	}
}

func TestContentHashFilterRejectsNonHex(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/items?content_hash=../../etc/passwd", nil)
	if _, err := itemFilterFromQuery(r); err == nil {
		t.Fatal("a non-hexadecimal content hash was accepted")
	}
}

func TestEntityIDFiltersMustBeUUIDs(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/search?entity_id=not-a-uuid,3f2504e0-4f89-41d3-9a0c-0305e82c3301", nil)
	q, err := searchQueryFromRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.EntityIDs) != 1 {
		t.Fatalf("expected only the valid UUID to survive, got %v", q.EntityIDs)
	}
}

func TestSearchQueryLengthIsBounded(t *testing.T) {
	long := strings.Repeat("a", 3000)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+long, nil)
	if _, err := searchQueryFromRequest(r); err == nil {
		t.Fatal("an unbounded query string was accepted")
	}
}

// --- Trusted origins -------------------------------------------------------

func TestTrustedOriginsIncludeBaseURLAndListenAddress(t *testing.T) {
	cfg := &config.Config{}
	cfg.HTTP.BaseURL = "https://relata.example.org"
	cfg.HTTP.Addr = "127.0.0.1:8080"

	origins := trustedOrigins(cfg)
	want := []string{"https://relata.example.org", "http://127.0.0.1:8080", "http://localhost:8080"}
	for _, w := range want {
		if !containsStr(origins, w) {
			t.Errorf("origin %q is missing from %v", w, origins)
		}
	}
	// Nothing else may be trusted.
	for _, o := range origins {
		if strings.Contains(o, "evil") || o == "*" {
			t.Errorf("unexpected trusted origin %q", o)
		}
	}
}

// A provider that ran out of time must be reported as a dependency timeout.
// It previously surfaced as an opaque internal error, leaving a user with no
// indication that the fix is a longer timeout or a smaller model.
func TestProviderTimeoutIsReportedAsAGatewayTimeout(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/search/answer", nil)
	w := httptest.NewRecorder()
	WriteError(w, r, fmt.Errorf("call the model: %w", context.DeadlineExceeded))

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Code != CodeDependencyTimeout {
		t.Fatalf("code = %q, want %q", resp.Error.Code, CodeDependencyTimeout)
	}
	// The message must tell the user what to do about it.
	if !strings.Contains(resp.Error.Message, "RELATA_ANALYSIS_TIMEOUT") {
		t.Fatalf("the message gives no remedy: %q", resp.Error.Message)
	}
}

func TestCancelledRequestIsNotAnInternalError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	w := httptest.NewRecorder()
	WriteError(w, r, fmt.Errorf("search: %w", context.Canceled))

	if w.Code == http.StatusInternalServerError {
		t.Fatal("a client disconnect was recorded as an internal error")
	}
}

// --- Fuzzing ---------------------------------------------------------------

// Request decoding is the outermost trust boundary for the JSON API.
func FuzzDecodeJSON(f *testing.F) {
	f.Add(`{"title":"a","count":1}`)
	f.Add(`{"title":"a","unknown":true}`)
	f.Add(`{"a":1}{"b":2}`)
	f.Add("\x00\xff")
	f.Add(`{"title":` + strings.Repeat("[", 200))

	f.Fuzz(func(t *testing.T, body string) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		var dst testBody
		err := DecodeJSON(w, r, 1<<16, &dst)
		if err == nil {
			return
		}
		// Every rejection must be a typed API error with a client-safe status,
		// never a bare error that would become an opaque 500.
		var apiErr *APIError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("decoding %q produced an untyped error: %v", body, err)
		}
		if apiErr.Status < 400 || apiErr.Status >= 500 {
			t.Fatalf("decoding %q produced status %d; malformed input is a client error",
				body, apiErr.Status)
		}
	})
}

// Query parameters are attacker-controlled and become database filter values.
func FuzzQueryFilters(f *testing.F) {
	f.Add("note", "ready", "2025-01-01", "abc123")
	f.Add(`{"$ne":null}`, "$where", "not-a-date", "../etc")

	f.Fuzz(func(t *testing.T, sourceType, status, from, hash string) {
		q := url.Values{}
		q.Set("source_type", sourceType)
		q.Set("status", status)
		q.Set("created_from", from)
		q.Set("content_hash", hash)

		r := httptest.NewRequest(http.MethodGet, "/api/v1/items?"+q.Encode(), nil)
		filter, err := itemFilterFromQuery(r)
		if err != nil {
			return // rejected, which is the safe outcome
		}
		// Anything accepted must be a value from a closed set, never arbitrary
		// text that could reach a query as an operator.
		for _, s := range filter.SourceTypes {
			if !s.Valid() {
				t.Fatalf("accepted an invalid source type %q", s)
			}
		}
		if filter.ContentHash != "" && !isHex(filter.ContentHash) {
			t.Fatalf("accepted a non-hexadecimal content hash %q", filter.ContentHash)
		}
	})
}
