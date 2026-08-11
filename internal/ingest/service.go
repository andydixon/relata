// Package ingest accepts information into Relata and drives it through the
// processing pipeline.
//
// The governing rule is that the original is preserved. Whatever a user
// supplies is stored verbatim — bytes for uploads, text for pastes — before any
// parsing is attempted. If parsing later fails, the failure is recorded as a
// warning on the item and the original remains retrievable and reprocessable.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/parsing"
)

// Request is a single item to ingest.
type Request struct {
	Title    string
	Content  []byte
	Filename string
	// DeclaredMIME is what the client claimed. It is a hint only; the real type
	// is determined from the content.
	DeclaredMIME string
	SourceType   domain.SourceType
	Metadata     map[string]string
	// CreatedAt is the user-asserted creation time of the information itself.
	CreatedAt *time.Time
	// AllowDuplicate must be set explicitly to store content that already
	// exists byte-for-byte.
	AllowDuplicate bool
}

// Result describes what ingestion produced.
type Result struct {
	Item *domain.Item
	// Duplicate is the existing item when identical content was found.
	Duplicate *domain.Item
	// Stored is false when an exact duplicate was refused.
	Stored bool
	JobID  string
}

// Service ingests items.
type Service struct {
	items   domain.ItemRepository
	jobs    domain.JobRepository
	cfg     *config.Config
	audit   *audit.Recorder
	metrics *observability.Metrics
	// admit bounds concurrent ingestion so a burst of uploads cannot exhaust
	// memory: each in-flight request holds its content in memory.
	admit chan struct{}
}

// NewService builds the ingestion service.
func NewService(items domain.ItemRepository, jobRepo domain.JobRepository, cfg *config.Config,
	rec *audit.Recorder, m *observability.Metrics) *Service {
	n := cfg.Uploads.MaxConcurrentIngest
	if n <= 0 {
		n = 4
	}
	return &Service{
		items:   items,
		jobs:    jobRepo,
		cfg:     cfg,
		audit:   rec,
		metrics: m,
		admit:   make(chan struct{}, n),
	}
}

// ValidationError describes why an ingestion request was rejected.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }
func (e *ValidationError) Unwrap() error { return domain.ErrInvalidInput }

// DuplicateError reports that identical content already exists.
type DuplicateError struct {
	ExistingID    string
	ExistingTitle string
}

func (e *DuplicateError) Error() string {
	if e.ExistingID == "" {
		// A race we lost, where the winning item was no longer readable.
		return "identical content already exists"
	}
	return fmt.Sprintf("identical content already exists as item %s (%q)", e.ExistingID, e.ExistingTitle)
}
func (e *DuplicateError) Unwrap() error { return domain.ErrDuplicate }

// Ingest validates, stores and queues an item for processing.
func (s *Service) Ingest(ctx context.Context, p *domain.Principal, req Request) (*Result, error) {
	select {
	case s.admit <- struct{}{}:
		defer func() { <-s.admit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if p == nil || p.WorkspaceID == "" {
		return nil, domain.ErrWorkspaceMiss
	}
	if !p.Role.CanWrite() {
		return nil, domain.ErrForbidden
	}

	item, err := s.build(ctx, p, req)
	if err != nil {
		s.countIngest(string(req.SourceType), "rejected")
		return nil, err
	}

	// Exact duplicate detection, before anything is written.
	existing, err := s.items.FindByContentHash(ctx, p.WorkspaceID, item.ContentHash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		if !req.AllowDuplicate {
			s.countIngest(string(req.SourceType), "duplicate")
			return &Result{Duplicate: existing, Stored: false},
				&DuplicateError{ExistingID: existing.ID, ExistingTitle: existing.Title}
		}
		// The user asked for it explicitly; record the link rather than hide it.
		item.DuplicateOf = existing.ID
	}

	// Persist the original bytes for binary formats before the item row, so a
	// row never claims a file that does not exist.
	if item.MIMEType == "application/pdf" || isDOCX(item.MIMEType) {
		path, err := s.storeOriginal(p.WorkspaceID, item.ID, req.Filename, req.Content)
		if err != nil {
			return nil, fmt.Errorf("store original file: %w", err)
		}
		item.OriginalStoredPath = path
	}

	// discard removes the stored original when the item will not be created.
	discard := func() {
		if item.OriginalStoredPath != "" {
			_ = os.Remove(item.OriginalStoredPath)
			item.OriginalStoredPath = ""
		}
	}

	if err := s.items.Create(ctx, item); err != nil {
		// A concurrent request stored identical content between the check above
		// and this insert: the unique index on content_hash caught what the
		// check could not.
		if !errors.Is(err, domain.ErrDuplicate) {
			discard()
			return nil, err
		}

		winner, lookupErr := s.items.FindByContentHash(ctx, p.WorkspaceID, item.ContentHash)
		if lookupErr != nil || winner == nil {
			// The winner is not readable — deleted in the interim, or the
			// lookup failed. Still a duplicate, just without a name to give.
			discard()
			s.countIngest(string(req.SourceType), "duplicate")
			return &Result{Stored: false}, &DuplicateError{}
		}

		if req.AllowDuplicate {
			// The caller asked for a second copy to be kept. Losing the race
			// does not change that answer — it only reveals which item this one
			// is a copy of. Recording that link puts the item outside the
			// uniqueness constraint, so the retry succeeds.
			item.DuplicateOf = winner.ID
			if err := s.items.Create(ctx, item); err != nil {
				discard()
				return nil, err
			}
		} else {
			discard()
			s.countIngest(string(req.SourceType), "duplicate")
			return &Result{Duplicate: winner, Stored: false},
				&DuplicateError{ExistingID: winner.ID, ExistingTitle: winner.Title}
		}
	}

	job := &domain.Job{
		ID:             ids.New(),
		WorkspaceID:    p.WorkspaceID,
		Type:           domain.JobParseItem,
		Status:         domain.JobPending,
		ItemID:         item.ID,
		Priority:       10,
		MaxAttempts:    s.cfg.Workers.MaxAttempts,
		IdempotencyKey: ids.Deterministic(p.WorkspaceID, item.ID, string(domain.JobParseItem), "initial"),
	}
	if err := s.jobs.Enqueue(ctx, job); err != nil {
		// The item exists and is recoverable; surface the queueing failure
		// rather than pretending processing has started.
		return &Result{Item: item, Stored: true}, fmt.Errorf("queue item for processing: %w", err)
	}

	s.audit.Record(ctx, p, domain.AuditItemCreated, "item", item.ID, "success", map[string]string{
		"source_type": string(item.SourceType),
		"mime_type":   item.MIMEType,
		"size_bytes":  fmt.Sprint(item.SizeBytes),
		"duplicate":   boolStr(item.DuplicateOf != ""),
	})
	s.countIngest(string(item.SourceType), "accepted")

	return &Result{Item: item, Stored: true, JobID: job.ID, Duplicate: existing}, nil
}

func (s *Service) countIngest(sourceType, outcome string) {
	if s.metrics != nil {
		if sourceType == "" {
			sourceType = "unknown"
		}
		s.metrics.IngestItems.WithLabelValues(sourceType, outcome).Inc()
	}
}

// build validates a request and constructs the item record.
func (s *Service) build(ctx context.Context, p *domain.Principal, req Request) (*domain.Item, error) {
	lim := s.cfg.Uploads

	if len(req.Content) == 0 {
		return nil, &ValidationError{Field: "content", Message: "no content was supplied"}
	}
	if int64(len(req.Content)) > lim.MaxUploadBytes {
		return nil, &ValidationError{
			Field:   "content",
			Message: fmt.Sprintf("content is %d bytes, larger than the %d byte limit", len(req.Content), lim.MaxUploadBytes),
		}
	}

	// Determine the real type from the bytes. A client's declared type and the
	// filename are hints; neither is trusted.
	sniff, err := parsing.Sniff(req.Content, req.DeclaredMIME, req.Filename)
	switch {
	case errors.Is(err, parsing.ErrArchiveRejected):
		return nil, &ValidationError{
			Field:   "content",
			Message: "archive files are not accepted. Please upload the documents individually.",
		}
	case errors.Is(err, parsing.ErrExecutableRejected):
		return nil, &ValidationError{
			Field:   "content",
			Message: "executable content is not accepted.",
		}
	case err != nil:
		return nil, &ValidationError{
			Field: "content",
			Message: fmt.Sprintf("this file type is not supported. Relata accepts plain text, Markdown, JSON, PDF and DOCX (detected: %s)",
				sniff.MIME),
		}
	}
	if !parsing.Supported(sniff.MIME) {
		return nil, &ValidationError{
			Field:   "content",
			Message: "this file type is not supported. Relata accepts plain text, Markdown, JSON, PDF and DOCX.",
		}
	}

	title, err := s.cleanTitle(req.Title, req.Filename)
	if err != nil {
		return nil, err
	}
	meta, err := s.cleanMetadata(req.Metadata)
	if err != nil {
		return nil, err
	}

	sourceType := req.SourceType
	if sourceType == "" {
		sourceType = inferSourceType(sniff.MIME)
	}
	if !sourceType.Valid() {
		return nil, &ValidationError{Field: "source_type", Message: "unrecognised source type"}
	}

	now := time.Now().UTC()
	created := now
	if req.CreatedAt != nil {
		created = req.CreatedAt.UTC()
		// A creation date far in the future is almost certainly a mistake and
		// would distort every temporal signal computed from it.
		if created.After(now.Add(24 * time.Hour)) {
			return nil, &ValidationError{Field: "created_at", Message: "the creation date cannot be in the future"}
		}
	}

	sum := sha256.Sum256(req.Content)
	item := &domain.Item{
		ID:          ids.New(),
		WorkspaceID: p.WorkspaceID,
		Title:       title,
		SourceType:  sourceType,
		MIMEType:    sniff.MIME,
		SizeBytes:   int64(len(req.Content)),
		ContentHash: hex.EncodeToString(sum[:]),
		Metadata:    meta,
		CreatedAt:   created,
		IngestedAt:  now,
		UpdatedAt:   now,
		Status:      domain.StatusPending,
		CreatedBy:   p.UserID,
	}

	if fn := SanitiseFilename(req.Filename); fn != "" {
		item.OriginalFilename = fn
	}

	// Text-shaped content is kept inline; binary content is kept on disk.
	if isTextual(sniff.MIME) {
		if !utf8.Valid(req.Content) {
			return nil, &ValidationError{Field: "content", Message: "text content is not valid UTF-8"}
		}
		item.OriginalText = string(req.Content)
	}

	if sniff.Declared != "" && sniff.FromContent && sniff.Declared != sniff.MIME {
		// Worth recording: it may be innocent, or it may be an attempt to
		// smuggle one format past a check for another.
		item.Warnings = append(item.Warnings, domain.ParseWarning{
			Code:    "mime_mismatch",
			Message: "The file's actual type differs from the type the browser reported. Relata used the type detected from the file's contents.",
			Detail:  fmt.Sprintf("declared=%s detected=%s", sniff.Declared, sniff.MIME),
		})
	}

	observability.LoggerFrom(ctx).Info("item accepted for ingestion",
		"item_id", item.ID, "mime_type", item.MIMEType, "size_bytes", item.SizeBytes,
		"source_type", string(item.SourceType))

	return item, nil
}

func (s *Service) cleanTitle(title, filename string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		t = strings.TrimSpace(SanitiseFilename(filename))
	}
	if t == "" {
		t = "Untitled item"
	}
	t = stripControl(t)
	if utf8.RuneCountInString(t) > s.cfg.Uploads.MaxTitleLen {
		return "", &ValidationError{
			Field:   "title",
			Message: fmt.Sprintf("title must be at most %d characters", s.cfg.Uploads.MaxTitleLen),
		}
	}
	return t, nil
}

// metadataKeyPattern restricts metadata keys to a safe, predictable shape.
//
// This is not cosmetic: keys become part of stored documents and are displayed
// and compared. Restricting them keeps a key from resembling a MongoDB operator
// or carrying markup.
var metadataKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,63}$`)

func (s *Service) cleanMetadata(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	lim := s.cfg.Uploads
	if len(in) > lim.MaxMetadataKeys {
		return nil, &ValidationError{
			Field:   "metadata",
			Message: fmt.Sprintf("at most %d metadata entries are allowed", lim.MaxMetadataKeys),
		}
	}

	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if !metadataKeyPattern.MatchString(key) {
			return nil, &ValidationError{
				Field:   "metadata",
				Message: fmt.Sprintf("metadata key %q is invalid; use letters, digits, dot, dash or underscore", truncate(key, 40)),
			}
		}
		// A key beginning with $ or containing a dot has special meaning to
		// MongoDB. The pattern above already excludes a leading $; dots are
		// permitted in the key text but the value is always stored as a plain
		// string inside a map, never interpreted as a path.
		val := stripControl(v)
		if len(val) > lim.MaxMetadataValueLen {
			return nil, &ValidationError{
				Field:   "metadata",
				Message: fmt.Sprintf("metadata value for %q exceeds %d characters", key, lim.MaxMetadataValueLen),
			}
		}
		out[key] = val
	}
	return out, nil
}

// storeOriginal writes uploaded bytes to disk.
//
// The stored name is derived from the item's own UUID, never from the
// user-supplied filename, so a crafted name cannot influence the path at all.
// Files are written without any execute permission and the directory layout is
// per-workspace.
func (s *Service) storeOriginal(workspaceID, itemID, filename string, content []byte) (string, error) {
	if !ids.Valid(itemID) || !ids.Valid(workspaceID) {
		return "", fmt.Errorf("refusing to store a file under a non-UUID identifier")
	}

	dir := filepath.Join(s.cfg.Uploads.Dir, workspaceID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}

	// The extension is taken from a fixed allow-list keyed on the *detected*
	// type, not from the filename, so ".php" or ".sh" can never appear.
	ext := ""
	switch {
	case strings.HasSuffix(strings.ToLower(SanitiseFilename(filename)), ".pdf"):
		ext = ".pdf"
	case strings.HasSuffix(strings.ToLower(SanitiseFilename(filename)), ".docx"):
		ext = ".docx"
	}

	path := filepath.Join(dir, itemID+ext)

	// Belt and braces: confirm the final path is inside the upload directory.
	absDir, err := filepath.Abs(s.cfg.Uploads.Dir)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absPath, absDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to write outside the upload directory")
	}

	// 0600: readable and writable by the service account only, never executable.
	if err := os.WriteFile(absPath, content, 0o600); err != nil {
		return "", err
	}
	return absPath, nil
}

// ReadOriginal returns the stored bytes of an uploaded file.
func (s *Service) ReadOriginal(item *domain.Item) ([]byte, error) {
	if item.OriginalStoredPath == "" {
		if item.OriginalText != "" {
			return []byte(item.OriginalText), nil
		}
		return nil, domain.ErrNotFound
	}
	absDir, err := filepath.Abs(s.cfg.Uploads.Dir)
	if err != nil {
		return nil, err
	}
	// Re-check containment on read: the stored path came from the database,
	// which is trusted, but a cheap check costs nothing and bounds the damage
	// from a corrupted record.
	if !strings.HasPrefix(item.OriginalStoredPath, absDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("stored file path is outside the upload directory")
	}
	return os.ReadFile(item.OriginalStoredPath)
}

// DeleteOriginal removes an item's stored file.
func (s *Service) DeleteOriginal(item *domain.Item) error {
	if item.OriginalStoredPath == "" {
		return nil
	}
	absDir, err := filepath.Abs(s.cfg.Uploads.Dir)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(item.OriginalStoredPath, absDir+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to delete outside the upload directory")
	}
	if err := os.Remove(item.OriginalStoredPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SanitiseFilename reduces a user-supplied filename to a safe display name.
//
// Only the base name survives, so "../../etc/passwd" becomes "passwd". Path
// separators, control characters, leading dots and Windows reserved names are
// all removed. The result is used for display and for choosing an extension —
// never as a path component on its own.
func SanitiseFilename(name string) string {
	if name == "" {
		return ""
	}
	// Handle both separators: a Windows client may send backslashes.
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)

	// filepath.Base returns "." or "/" for degenerate input.
	if name == "." || name == ".." || name == "/" {
		return ""
	}

	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r == 0:
			return '_'
		}
		return r
	}, name)

	// A leading dot would create a hidden file and can hide the real extension.
	name = strings.TrimLeft(name, ". ")
	name = strings.TrimRight(name, ". ")

	// Windows reserved device names, which can cause surprising behaviour if
	// the file is ever copied to such a system.
	base := name
	if i := strings.IndexByte(name, '.'); i > 0 {
		base = name[:i]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		name = "file_" + name
	}

	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func inferSourceType(mime string) domain.SourceType {
	switch mime {
	case "application/json":
		return domain.SourceTypeJSON
	case "text/markdown", "text/x-markdown":
		return domain.SourceTypeMarkdown
	case "application/pdf", "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return domain.SourceTypeDocument
	default:
		return domain.SourceTypeNote
	}
}

func isTextual(mime string) bool {
	switch mime {
	case "text/plain", "text/markdown", "text/x-markdown", "application/json", "text/json", "text/csv":
		return true
	}
	return false
}

func isDOCX(mime string) bool {
	return mime == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
