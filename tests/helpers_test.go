//go:build integration

package tests

import (
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/search"
)

// search_Query builds a search query with expansion enabled, as the interface
// issues them.
func search_Query(text string) search.Query {
	return search.Query{Text: text, Expand: true, Limit: 20}
}

// search_AnswerRequest builds an answer request for a question.
func search_AnswerRequest(question string) search.AnswerRequest {
	return search.AnswerRequest{
		Question: question,
		Query:    search.Query{Expand: true},
	}
}

// ids_New returns a fresh identifier.
func ids_New() string { return ids.New() }

// resultItemIDs lists the item IDs in a search response.
func resultItemIDs(resp *search.Response) []string {
	out := make([]string, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, r.Item.ID)
	}
	return out
}

// containsResult reports whether a response includes an item.
func containsResult(resp *search.Response, itemID string) bool {
	for _, id := range resultItemIDs(resp) {
		if id == itemID {
			return true
		}
	}
	return false
}

// authHash produces an Argon2id hash for a test account.
func authHash(password string) (string, error) { return auth.HashPassword(password) }
