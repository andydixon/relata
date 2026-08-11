package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The readiness probe is unauthenticated. MongoDB's driver errors embed the
// topology — host, port, and, when the connection string carries them,
// credentials — so the raw error must never reach the response body.
func TestReadinessDoesNotEchoDependencyErrorText(t *testing.T) {
	secret := "mongodb://relata:sup3rs3cr3t@db.internal:27017/relata?authSource=admin"
	h := &HealthChecker{
		Mongo: func(context.Context) error {
			return fmt.Errorf("server selection error: current topology: { Servers: [{ Addr: %s }] }", secret)
		},
		Version: "test",
		Started: time.Now(),
	}

	w := httptest.NewRecorder()
	h.Ready(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	body := w.Body.String()

	for _, leak := range []string{"sup3rs3cr3t", "db.internal", "27017", "authSource", "topology"} {
		if strings.Contains(body, leak) {
			t.Fatalf("readiness response leaks %q from the driver error:\n%s", leak, body)
		}
	}

	// It must still report the failure clearly.
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d when a required dependency is down",
			w.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(body, "unavailable") {
		t.Fatalf("readiness hid the failure entirely:\n%s", body)
	}
}
