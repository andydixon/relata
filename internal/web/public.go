package web

import (
	"net/http"

	"github.com/andydixon/relata/internal/api"
	"github.com/andydixon/relata/internal/relationships"
)

// publicBase carries what every public page needs.
//
// It deliberately does not embed `base`: the shared page context reads the
// pending-review count from the database, and a page served to an anonymous
// visitor must touch no workspace data at all.
type publicBase struct {
	Title        string
	Version      string
	SignedIn     bool
	OAuthEnabled bool
	LocalEnabled bool
	ProviderName string
	RequestID    string
}

func (h *Handlers) newPublicBase(r *http.Request, title string) publicBase {
	return publicBase{
		Title:        title,
		Version:      h.d.Version,
		SignedIn:     api.PrincipalFrom(r.Context()) != nil,
		OAuthEnabled: h.oauthEnabled(),
		LocalEnabled: h.localLoginEnabled(),
		ProviderName: h.providerName(),
	}
}

// LandingPage is the only dual-purpose route.
//
// Signed in, it is the dashboard. Signed out, it is a description of what
// Relata does — served rather than redirected, so the front page of the site is
// not a login form.
func (h *Handlers) LandingPage(w http.ResponseWriter, r *http.Request) {
	if api.PrincipalFrom(r.Context()) != nil {
		h.Dashboard(w, r)
		return
	}

	data := struct {
		publicBase
		Weights []publicWeightRow
	}{
		publicBase: h.newPublicBase(r, "Relata — a self-organising semantic document database"),
		Weights:    publicWeightRows(),
	}
	h.d.Renderer.RenderPublic(w, r, "public_landing.html", http.StatusOK, data)
}

// HowItWorksPage explains the pipeline and the scoring, without any data.
func (h *Handlers) HowItWorksPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		publicBase
		Weights []publicWeightRow
	}{
		publicBase: h.newPublicBase(r, "How Relata works"),
		Weights:    publicWeightRows(),
	}
	h.d.Renderer.RenderPublic(w, r, "public_how.html", http.StatusOK, data)
}

// PublicAPIPage describes the API and how to authenticate against it.
func (h *Handlers) PublicAPIPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		publicBase
		CallbackURL string
	}{
		publicBase:  h.newPublicBase(r, "The Relata API"),
		CallbackURL: h.callbackURL(),
	}
	h.d.Renderer.RenderPublic(w, r, "public_api.html", http.StatusOK, data)
}

// publicWeightRow is one signal weight, shown publicly because the scoring model is
// part of what the product claims rather than something to be discovered.
type publicWeightRow struct {
	Signal string
	Weight float64
	Means  string
}

func publicWeightRows() []publicWeightRow {
	explain := map[string]string{
		"content_hash_match":  "Byte-identical content.",
		"semantic_similarity": "How alike the wording is, rebased so that ordinary similarity counts for little.",
		"shared_entity":       "An exact entity match, discounted by how common that entity is.",
		"claim_support":       "Two statements agree about the same subject.",
		"claim_contradiction": "Two statements disagree about the same subject.",
		"model_classification": "A language model's opinion. One signal, weighted below the storage " +
			"threshold, so it can never create a relationship on its own.",
		"shared_concept":     "Overlapping concept vocabulary.",
		"alias_entity":       "A match through an alias a person confirmed. Unconfirmed candidates count for nothing.",
		"temporal_proximity": "Created close together in time.",
		"shared_metadata":    "Matching metadata you supplied.",
		"user_confirmation":  "A person confirmed it. This dominates everything else.",
		"user_rejection":     "A person rejected it. This drives the score to zero.",
	}

	rows := make([]publicWeightRow, 0, len(relationships.Weights))
	for _, sig := range relationships.WeightOrder() {
		rows = append(rows, publicWeightRow{
			Signal: string(sig),
			Weight: relationships.Weights[sig],
			Means:  explain[string(sig)],
		})
	}
	return rows
}
