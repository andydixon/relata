package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Templates are parsed at startup, so a syntax error is a start-up failure
// rather than a broken page. This makes it a test failure instead.
func TestEveryTemplateParses(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("templates do not parse: %v", err)
	}
	for _, name := range append(append([]string{}, pages...), publicPages...) {
		if r.templates[name] == nil {
			t.Errorf("template %q was not registered", name)
		}
	}
	for _, name := range partials {
		if r.templates[name] == nil {
			t.Errorf("partial %q was not registered", name)
		}
	}
}

// A public page must render from the public layout, which has no navigation and
// no workspace context — and must not require a principal to do it.
func TestPublicPagesRenderWithoutAPrincipal(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}

	data := struct {
		publicBase
		Weights     []publicWeightRow
		CallbackURL string
	}{
		publicBase: publicBase{
			Title: "Test", Version: "test", SignedIn: false,
			OAuthEnabled: true, ProviderName: "Blackwall",
		},
		Weights:     publicWeightRows(),
		CallbackURL: "https://relata.example/auth/oauth/callback",
	}

	for _, page := range publicPages {
		t.Run(page, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			r.RenderPublic(w, req, page, 200, data)

			if w.Code != 200 {
				t.Fatalf("status = %d; the page failed to render", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, "<!DOCTYPE html>") {
				t.Fatal("no document was produced")
			}
			// The application chrome belongs to signed-in pages only.
			for _, leak := range []string{`href="/review"`, `href="/jobs"`, `action="/logout"`} {
				if strings.Contains(body, leak) {
					t.Errorf("public page contains application navigation (%s)", leak)
				}
			}
		})
	}
}

// The weight table is rendered from the live map, so it cannot drift from the
// scoring model — and it must be in a fixed order or the page changes on every
// reload for no reason.
func TestPublicWeightRowsAreCompleteAndOrdered(t *testing.T) {
	first := publicWeightRows()
	if len(first) == 0 {
		t.Fatal("no weights were produced")
	}
	for _, row := range first {
		if row.Means == "" {
			t.Errorf("signal %q has no explanation; the public page would show a blank cell", row.Signal)
		}
	}
	for i := 0; i < 5; i++ {
		again := publicWeightRows()
		for j := range first {
			if again[j].Signal != first[j].Signal {
				t.Fatal("weight order is not stable between calls")
			}
		}
	}
}
