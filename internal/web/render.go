// Package web serves Relata's server-rendered HTML interface.
//
// Every value interpolated into a page passes through html/template's
// contextual auto-escaping. Stored content — titles, excerpts, entity names,
// model output — is treated as untrusted text and is never marked safe. There
// is deliberately no helper in this package that produces template.HTML from a
// stored string, because such a helper is how stored XSS gets introduced.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// StaticFS exposes the embedded static assets for mounting, rooted at the
// assets themselves rather than at the directory containing them.
//
// The embed directive captures paths as "static/app.css", while the router
// strips the "/static/" prefix before looking a file up. Without this the two
// cancel out the wrong way and every asset 404s — which the browser reports
// not as a missing stylesheet but as corrupted content, because the 404 body
// arrives as text/plain under X-Content-Type-Options: nosniff.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Only reachable if the embed directive above stops matching, which is
		// a build-time mistake rather than a runtime condition.
		panic("static assets are not embedded: " + err.Error())
	}
	return sub
}

// Renderer holds the parsed templates.
type Renderer struct {
	templates map[string]*template.Template
}

// pages are the content templates, each combined with the layout.
var pages = []string{
	"login.html",
	"dashboard.html",
	"items.html",
	"add_text.html",
	"upload.html",
	"item_detail.html",
	"search.html",
	"review.html",
	"entities.html",
	"entity_detail.html",
	"jobs.html",
	"settings.html",
	"error.html",
	"neighbourhood.html",
	"tokens.html",
}

// publicPages are served to anyone. They use their own layout, which has no
// navigation, no workspace context and nothing that reads the database — so a
// signed-out visitor cannot be shown stored content by a template mistake.
var publicPages = []string{
	"public_landing.html",
	"public_how.html",
	"public_api.html",
}

// partials are fragments rendered on their own for HTMX swaps.
var partials = []string{
	"partial_search_results.html",
	"partial_relationship.html",
	"partial_jobs_table.html",
	"partial_answer.html",
}

// NewRenderer parses every template at startup, so a template error is a
// start-up failure rather than a broken page at request time.
func NewRenderer() (*Renderer, error) {
	r := &Renderer{templates: map[string]*template.Template{}}

	for _, page := range pages {
		t, err := template.New("layout.html").Funcs(funcs()).ParseFS(templateFS,
			"templates/layout.html", "templates/"+page, "templates/partial_*.html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		r.templates[page] = t
	}

	for _, page := range publicPages {
		t, err := template.New("public_layout.html").Funcs(funcs()).ParseFS(templateFS,
			"templates/public_layout.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("parse public template %s: %w", page, err)
		}
		r.templates[page] = t
	}

	for _, p := range partials {
		t, err := template.New(p).Funcs(funcs()).ParseFS(templateFS, "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse partial %s: %w", p, err)
		}
		r.templates[p] = t
	}
	return r, nil
}

// Render writes a full page.
//
// The page is rendered into a buffer first: a template error midway through
// would otherwise leave a half-written page with a 200 status, which looks to a
// user like data loss.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, page string, status int, data any) {
	t, ok := r.templates[page]
	if !ok {
		observability.LoggerFrom(req.Context()).Error("unknown template requested", "template", page)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		observability.LoggerFrom(req.Context()).Error("template rendering failed",
			"template", page, "error", err.Error())
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// RenderPublic writes a page from the public layout.
//
// It is a separate method rather than a flag on Render so that a public page
// cannot accidentally be rendered into the application layout, which expects a
// principal and a workspace.
func (r *Renderer) RenderPublic(w http.ResponseWriter, req *http.Request, page string, status int, data any) {
	t, ok := r.templates[page]
	if !ok {
		observability.LoggerFrom(req.Context()).Error("unknown public template requested", "template", page)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "public_layout.html", data); err != nil {
		observability.LoggerFrom(req.Context()).Error("public template rendering failed",
			"template", page, "error", err.Error())
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// RenderPartial writes a fragment, for HTMX swaps.
func (r *Renderer) RenderPartial(w http.ResponseWriter, req *http.Request, name string, data any) {
	t, ok := r.templates[name]
	if !ok {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		observability.LoggerFrom(req.Context()).Error("partial rendering failed",
			"template", name, "error", err.Error())
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// funcs are the template helpers.
//
// None of them returns template.HTML from stored content. `percent` and
// `strengthLabel` format numbers; `truncate` shortens text; all output remains
// a plain string that the template engine escapes on insertion.
func funcs() template.FuncMap {
	return template.FuncMap{
		"percent": func(v float64) string {
			return fmt.Sprintf("%.0f%%", v*100)
		},
		"score2": func(v float64) string {
			return fmt.Sprintf("%.2f", v)
		},
		// strengthLabel turns a score into words, so a user is not left to
		// interpret a bare decimal as if it were a probability.
		"strengthLabel": func(v float64) string {
			switch {
			case v >= 0.75:
				return "strong"
			case v >= 0.5:
				return "moderate"
			case v >= 0.3:
				return "weak"
			default:
				return "very weak"
			}
		},
		"provenanceLabel": provenanceLabel,
		"relTypeLabel":    relTypeLabel,
		"statusLabel":     statusLabel,
		"truncate": func(n int, s string) string {
			s = strings.Join(strings.Fields(s), " ")
			if len(s) <= n {
				return s
			}
			for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
				n--
			}
			return s[:n] + "…"
		},
		"datetime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2 Jan 2006, 15:04")
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2 Jan 2006")
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d min ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d hours ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%d days ago", int(d.Hours()/24))
			}
		},
		"bytes": func(n int64) string {
			switch {
			case n < 1024:
				return fmt.Sprintf("%d B", n)
			case n < 1024*1024:
				return fmt.Sprintf("%.1f KB", float64(n)/1024)
			default:
				return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
			}
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"gt":  func(a, b int) bool { return a > b },
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
		"dict": func(values ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(values); i += 2 {
				if k, ok := values[i].(string); ok {
					m[k] = values[i+1]
				}
			}
			return m
		},
		"relTypes":    func() []domain.RelationshipType { return domain.ValidRelationshipTypes },
		"sourceTypes": func() []domain.SourceType { return domain.ValidSourceTypes },
		"entityTypes": func() []domain.EntityType { return domain.ValidEntityTypes },
	}
}

func provenanceLabel(p domain.Provenance) string {
	switch p {
	case domain.ProvExplicit:
		return "stated by a person"
	case domain.ProvDeterministic:
		return "derived from matching evidence"
	case domain.ProvModelInferred:
		return "suggested by analysis — not verified"
	case domain.ProvUserConfirmed:
		return "confirmed by a person"
	case domain.ProvUserRejected:
		return "rejected by a person"
	}
	return string(p)
}

func relTypeLabel(t domain.RelationshipType) string {
	return strings.ReplaceAll(string(t), "_", " ")
}

func statusLabel(s domain.ProcessingStatus) string {
	switch s {
	case domain.StatusPending:
		return "Waiting"
	case domain.StatusParsing:
		return "Extracting text"
	case domain.StatusChunking:
		return "Dividing into sections"
	case domain.StatusEmbedding:
		return "Generating embeddings"
	case domain.StatusAnalysing:
		return "Extracting entities and claims"
	case domain.StatusRelating:
		return "Finding relationships"
	case domain.StatusReady:
		return "Ready"
	case domain.StatusPartial:
		return "Ready, with warnings"
	case domain.StatusFailed:
		return "Failed"
	case domain.StatusTombstoned:
		return "Deleted"
	}
	return string(s)
}
