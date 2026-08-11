// Package api implements Relata's versioned REST interface under /api/v1.
//
// Handlers here never expose internal detail to clients: stack traces, driver
// errors and file paths stay in the logs, while the response carries a stable
// machine-readable code and a message safe to show a user.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/search"
)

// Stable error codes. Clients may branch on these; the messages may change.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeValidationFailed  = "validation_failed"
	CodeUnauthorised      = "unauthorised"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeDuplicate         = "duplicate_content"
	CodeConflict          = "conflict"
	CodePayloadTooLarge   = "payload_too_large"
	CodeUnsupportedMedia  = "unsupported_media_type"
	CodeRateLimited       = "rate_limited"
	CodeInsufficientScope = "insufficient_scope"
	CodeCSRFFailed        = "csrf_check_failed"
	CodeDependencyDown    = "dependency_unavailable"
	CodeDependencyTimeout = "dependency_timeout"
	CodeClientClosed      = "client_closed_request"
	CodeModelInvalid      = "model_output_invalid"
	CodeFeatureDisabled   = "feature_disabled"
	CodeInternal          = "internal_error"
)

// ErrorResponse is the body returned for every failed API request.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the code, message and any field-level detail.
type ErrorBody struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Fields  []FieldError `json:"fields,omitempty"`
	// RequestID lets a user quote a reference when reporting a problem, and
	// lets an operator find the corresponding log line.
	RequestID string `json:"request_id,omitempty"`
	// Detail carries safe supplementary information, such as the ID of an
	// existing duplicate item.
	Detail map[string]string `json:"detail,omitempty"`
}

// FieldError describes one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// APIError is an error with an HTTP status and a stable code.
type APIError struct {
	Status  int
	Code    string
	Message string
	Fields  []FieldError
	Detail  map[string]string
	// Internal is logged but never sent to the client.
	Internal error
}

func (e *APIError) Error() string {
	if e.Internal != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Internal)
	}
	return e.Message
}

func (e *APIError) Unwrap() error { return e.Internal }

// Errorf builds an APIError.
func Errorf(status int, code, format string, args ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// BadRequest builds a 400.
func BadRequest(msg string, fields ...FieldError) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: CodeValidationFailed, Message: msg, Fields: fields}
}

// WriteError sends an error response, mapping domain errors onto HTTP.
//
// Unmapped errors become a generic 500: the internal detail is logged with the
// request ID but never returned, so a database error cannot leak schema or
// connection information to a client.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := observability.RequestID(r.Context())
	log := observability.LoggerFrom(r.Context())

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = mapDomainError(err)
	}
	if apiErr.RequestIDSuppressed() {
		reqID = ""
	}

	if apiErr.Status >= 500 {
		log.Error("request failed",
			"code", apiErr.Code, "path", r.URL.Path, "method", r.Method,
			"error", err.Error())
	} else {
		log.Info("request rejected",
			"code", apiErr.Code, "status", apiErr.Status, "path", r.URL.Path, "method", r.Method,
			"reason", apiErr.Message)
	}

	body := ErrorResponse{Error: ErrorBody{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Fields:    apiErr.Fields,
		RequestID: reqID,
		Detail:    apiErr.Detail,
	}}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(apiErr.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// RequestIDSuppressed reports whether the request ID should be withheld. It
// never is at present, but the hook keeps the decision in one place.
func (e *APIError) RequestIDSuppressed() bool { return false }

// mapDomainError converts a domain or service error into an API error.
func mapDomainError(err error) *APIError {
	var ve *ingest.ValidationError
	if errors.As(err, &ve) {
		return &APIError{
			Status: http.StatusBadRequest, Code: CodeValidationFailed,
			Message: "The request could not be accepted.",
			Fields:  []FieldError{{Field: ve.Field, Message: ve.Message}},
		}
	}

	var de *ingest.DuplicateError
	if errors.As(err, &de) {
		return &APIError{
			Status: http.StatusConflict, Code: CodeDuplicate,
			Message: "An item with identical content already exists. " +
				"Send allow_duplicate=true to store it anyway.",
			Detail: map[string]string{
				"existing_item_id": de.ExistingID,
				"existing_title":   de.ExistingTitle,
			},
		}
	}

	switch {
	case errors.Is(err, domain.ErrNotFound):
		return &APIError{Status: http.StatusNotFound, Code: CodeNotFound,
			Message: "The requested item does not exist."}

	case errors.Is(err, domain.ErrForbidden):
		return &APIError{Status: http.StatusForbidden, Code: CodeForbidden,
			Message: "You do not have permission to perform this action."}

	// A cross-workspace access attempt is reported as "not found" rather than
	// "forbidden", so that a probe cannot confirm an ID exists elsewhere.
	case errors.Is(err, domain.ErrWorkspaceMiss):
		return &APIError{Status: http.StatusNotFound, Code: CodeNotFound,
			Message: "The requested item does not exist."}

	case errors.Is(err, domain.ErrDuplicate):
		return &APIError{Status: http.StatusConflict, Code: CodeDuplicate,
			Message: "That content already exists."}

	case errors.Is(err, domain.ErrConflict):
		return &APIError{Status: http.StatusConflict, Code: CodeConflict,
			Message: "The record changed while your request was being processed. Please try again."}

	case errors.Is(err, domain.ErrTooLarge):
		return &APIError{Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge,
			Message: "The request body is larger than the configured limit."}

	case errors.Is(err, domain.ErrUnsupported):
		return &APIError{Status: http.StatusUnsupportedMediaType, Code: CodeUnsupportedMedia,
			Message: "That content type is not supported."}

	case errors.Is(err, domain.ErrInvalidInput):
		return &APIError{Status: http.StatusBadRequest, Code: CodeInvalidRequest,
			Message: firstLine(err.Error())}

	case errors.Is(err, domain.ErrRateLimited), errors.Is(err, auth.ErrTooManyAttempts):
		return &APIError{Status: http.StatusTooManyRequests, Code: CodeRateLimited,
			Message: "Too many attempts. Please wait and try again."}

	case errors.Is(err, auth.ErrNoSession), errors.Is(err, auth.ErrInvalidCredentials):
		return &APIError{Status: http.StatusUnauthorized, Code: CodeUnauthorised,
			Message: "Authentication is required."}

	case errors.Is(err, analysis.ErrInvalidOutput):
		return &APIError{Status: http.StatusBadGateway, Code: CodeModelInvalid,
			Message: "The language model returned a response Relata could not validate. Nothing was stored."}

	case errors.Is(err, analysis.ErrDisabled), errors.Is(err, search.ErrAnswerDisabled):
		return &APIError{Status: http.StatusServiceUnavailable, Code: CodeFeatureDisabled,
			Message: "This feature requires a language model, which is not configured."}

	case errors.Is(err, domain.ErrUnavailable):
		return &APIError{Status: http.StatusServiceUnavailable, Code: CodeDependencyDown,
			Message: "A required service is unavailable. Please try again shortly."}

	// A provider that ran out of time is a dependency problem, not an
	// unexplained internal failure. Reporting it as one leaves a user with no
	// idea that the fix is a bigger timeout or a smaller model.
	case errors.Is(err, context.DeadlineExceeded):
		return &APIError{Status: http.StatusGatewayTimeout, Code: CodeDependencyTimeout,
			Message: "A language model or storage call took longer than the configured timeout. " +
				"Try a smaller request, or raise RELATA_ANALYSIS_TIMEOUT and RELATA_HTTP_WRITE_TIMEOUT."}

	case errors.Is(err, context.Canceled):
		return &APIError{Status: 499, Code: CodeClientClosed,
			Message: "The request was cancelled before it completed."}
	}

	return &APIError{
		Status: http.StatusInternalServerError, Code: CodeInternal,
		Message:  "An unexpected error occurred. The request reference below can be quoted when reporting it.",
		Internal: err,
	}
}

// WriteJSON sends a successful JSON response.
//
// The response is encoded into a buffer first, so that a marshalling failure is
// reported as a 500 rather than truncating a body whose 200 status has already
// been written.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")

	if v == nil {
		w.WriteHeader(status)
		return
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		slog.Default().Error("could not encode response body", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"The response could not be encoded."}}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// DecodeJSON reads a JSON request body strictly.
//
// Unknown fields are rejected so a client sending a misspelled or unsupported
// field is told, rather than having it silently ignored; trailing content is
// rejected so two concatenated documents cannot be smuggled through; and the
// body is size-limited before parsing.
func DecodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" {
		base, _, _ := strings.Cut(ct, ";")
		if strings.TrimSpace(base) != "application/json" {
			return &APIError{
				Status: http.StatusUnsupportedMediaType, Code: CodeUnsupportedMedia,
				Message: "The request body must be application/json.",
			}
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err, maxBytes)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return BadRequest("The request body must contain exactly one JSON object.")
	}
	return nil
}

// decodeError turns a JSON decoding failure into a specific, safe message.
func decodeError(err error, maxBytes int64) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var maxErr *http.MaxBytesError

	switch {
	case errors.As(err, &syntaxErr):
		return BadRequest(fmt.Sprintf("The request body is not valid JSON (at position %d).", syntaxErr.Offset))

	case errors.Is(err, io.ErrUnexpectedEOF):
		return BadRequest("The request body ended unexpectedly and is not valid JSON.")

	case errors.As(err, &typeErr):
		return BadRequest("A field has the wrong type.", FieldError{
			Field:   typeErr.Field,
			Message: fmt.Sprintf("expected %s", typeErr.Type.String()),
		})

	case errors.Is(err, io.EOF):
		return BadRequest("The request body is empty.")

	case errors.As(err, &maxErr):
		return &APIError{
			Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge,
			Message: fmt.Sprintf("The request body must be no larger than %d bytes.", maxBytes),
		}

	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return BadRequest("The request contains a field Relata does not accept.", FieldError{
			Field:   field,
			Message: "unknown field",
		})
	}
	return BadRequest("The request body could not be read.")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
