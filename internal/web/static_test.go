package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// assetHandler mirrors how the router mounts the embedded assets: the request
// path has "/static/" stripped, and the file system is expected to be rooted at
// the assets themselves.
func assetHandler() http.Handler {
	return http.StripPrefix("/static/", http.FileServerFS(StaticFS()))
}

// Static assets must actually be served. A 404 here does not look like a
// missing file in the browser: the 404 body arrives as text/plain under
// X-Content-Type-Options: nosniff, so the browser refuses it and reports
// corrupted content, which sends you looking for an encoding bug instead.
func TestStaticAssetsAreServed(t *testing.T) {
	h := assetHandler()

	for _, asset := range []struct{ path, wantType string }{
		{"/static/app.css", "text/css"},
		{"/static/public.css", "text/css"},
		{"/static/app.js", "javascript"},
		{"/static/favicon.svg", "image/svg+xml"},
	} {
		t.Run(asset.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, asset.path, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — the asset is not reachable at the path "+
					"the templates reference", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, asset.wantType) {
				t.Fatalf("Content-Type = %q, want something containing %q", ct, asset.wantType)
			}
			if w.Body.Len() == 0 {
				t.Fatal("the asset is empty")
			}
		})
	}
}

// Path traversal must not escape the asset root.
func TestStaticAssetsRefuseTraversal(t *testing.T) {
	h := assetHandler()
	for _, p := range []string{
		"/static/../render.go",
		"/static/../../go.mod",
		"/static/..%2f..%2fgo.mod",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code == http.StatusOK {
			t.Errorf("%s was served with 200; it must not escape the asset root", p)
		}
	}
}
