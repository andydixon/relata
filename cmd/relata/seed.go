package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/storage/mongo"
)

// seedDoc is one document in the demonstration dataset.
type seedDoc struct {
	Title      string
	Body       string
	SourceType domain.SourceType
	MIME       string
	Filename   string
	Metadata   map[string]string
	DaysAgo    int
	// Role explains what this document demonstrates, printed after seeding.
	Role string
}

// demoDate is the reference point the dataset is built around.
func demoDate(daysAgo int) time.Time {
	return time.Now().UTC().AddDate(0, 0, -daysAgo).Truncate(24 * time.Hour)
}

// seedDocuments is a small, varied corpus that exercises every part of the
// relationship model — including the cases where similarity must NOT be
// mistaken for a factual connection.
func seedDocuments() []seedDoc {
	return []seedDoc{
		// --- Two clearly related documents ---------------------------------
		{
			Title:      "Project Atlas — delivery schedule (original)",
			SourceType: domain.SourceTypeDocument,
			MIME:       "text/markdown",
			DaysAgo:    30,
			Role:       "Clearly related to the revision below, and superseded by it.",
			Metadata:   map[string]string{"project": "atlas", "department": "operations"},
			Body: `# Project Atlas — delivery schedule

Prepared by Dr Alice Chen for Example Ltd on 2025-02-10.

## Summary

The initial delivery date for the Atlas units is 3 March 2025.
Example Ltd will ship 240 units to the Northfield depot.

## Contact

Operational questions should go to ops@example.com.
The total project cost is £42,000 excluding VAT.
`,
		},
		{
			Title:      "Project Atlas — revised delivery schedule",
			SourceType: domain.SourceTypeDocument,
			MIME:       "text/markdown",
			DaysAgo:    12,
			Role:       "Updates and supersedes the original schedule; shares entities and subject.",
			Metadata:   map[string]string{"project": "atlas", "department": "operations"},
			Body: `# Project Atlas — revised delivery schedule

Prepared by Dr Alice Chen for Example Ltd on 2025-02-28.
This revision replaces the schedule issued on 2025-02-10.

## Summary

The revised delivery date is 14 March 2025.
Example Ltd will ship 240 units to the Northfield depot as before.

The delay follows a shortage of housings from the subcontractor.

## Contact

Operational questions should go to ops@example.com.
`,
		},

		// --- One contradiction ---------------------------------------------
		{
			Title:      "Atlas invoice reconciliation",
			SourceType: domain.SourceTypeNote,
			MIME:       "text/plain",
			DaysAgo:    9,
			Role:       "Contradicts the original schedule about the project cost.",
			Metadata:   map[string]string{"project": "atlas", "department": "finance"},
			Body: `Finance note, prepared 2025-03-01.

Reconciling the Atlas paperwork against what Example Ltd has actually invoiced.

The total project cost is £51,500 excluding VAT. This does not match the figure in the
delivery schedule and needs resolving before the next payment run.

Raised with Dr Alice Chen; awaiting a response.
`,
		},

		// --- One update that supersedes older information -------------------
		{
			Title:      "Returns policy v3 (current)",
			SourceType: domain.SourceTypeDocument,
			MIME:       "text/markdown",
			DaysAgo:    5,
			Role:       "Supersedes the older returns policy below.",
			Metadata:   map[string]string{"policy": "returns", "version": "3"},
			Body: `# Returns policy, version 3

Effective 2025-03-05. This policy replaces version 2, issued 2024-11-01.

## Terms

The returns window is 60 days from delivery.
Refunds are issued to the original payment method within 14 days.

Damaged goods must be reported to Example Ltd within 48 hours of delivery.
`,
		},
		{
			Title:      "Returns policy v2 (withdrawn)",
			SourceType: domain.SourceTypeDocument,
			MIME:       "text/markdown",
			DaysAgo:    120,
			Role:       "The older policy, superseded by version 3.",
			Metadata:   map[string]string{"policy": "returns", "version": "2"},
			Body: `# Returns policy, version 2

Effective 2024-11-01.

## Terms

The returns window is 30 days from delivery.
Refunds are issued to the original payment method within 21 days.

Damaged goods must be reported to Example Ltd within 24 hours of delivery.
`,
		},

		// --- Two superficially similar but meaningfully different docs -------
		{
			Title:      "Northfield depot fire drill report",
			SourceType: domain.SourceTypeDocument,
			MIME:       "text/plain",
			DaysAgo:    20,
			Role:       "Superficially similar to the safety inspection: same place, same vocabulary, different event.",
			Metadata:   map[string]string{"site": "northfield"},
			Body: `Fire drill report — Northfield depot

Date of exercise: 2025-02-18. Conducted by the site safety officer.

The depot was evacuated in 4 minutes. All 38 staff were accounted for at the assembly point.
Two fire doors on the east side were found to be obstructed by pallets and were cleared.

This was a scheduled drill. No actual incident occurred.
`,
		},
		{
			Title:      "Northfield depot safety inspection",
			SourceType: domain.SourceTypeDocument,
			MIME:       "text/plain",
			DaysAgo:    18,
			Role:       "Similar wording to the fire drill report, but a different event entirely.",
			Metadata:   map[string]string{"site": "northfield"},
			Body: `Safety inspection — Northfield depot

Date of inspection: 2025-02-20. Conducted by an external assessor.

The depot was inspected against the site safety schedule. Racking, lighting and fire doors
were examined. Two fire doors on the east side were noted as previously obstructed and
now clear.

This was a routine annual inspection, not a drill and not an incident.
`,
		},

		// --- One document sharing an entity but not a subject ---------------
		{
			Title:      "Example Ltd sponsorship announcement",
			SourceType: domain.SourceTypeNote,
			MIME:       "text/plain",
			DaysAgo:    15,
			Role:       "Shares the entity Example Ltd with the Atlas documents, but is about something else entirely.",
			Metadata:   map[string]string{"department": "marketing"},
			Body: `Announcement, 2025-02-22.

Example Ltd has agreed to sponsor the regional youth athletics championship for the
2025 season. The sponsorship covers kit and travel for four clubs.

Dr Alice Chen will present the awards at the closing ceremony.

This has no bearing on any current delivery or procurement work.
`,
		},

		// --- One likely duplicate -------------------------------------------
		{
			Title:      "Atlas revised delivery — forwarded copy",
			SourceType: domain.SourceTypeEmail,
			MIME:       "text/plain",
			DaysAgo:    11,
			Role:       "A near-duplicate of the revised schedule. Relata flags it; it never merges it.",
			Metadata:   map[string]string{"project": "atlas"},
			Body: `From: alice.chen@example.com
To: depot@example.com
Subject: FW: Project Atlas revised delivery schedule
Date: Sat, 1 Mar 2025 08:15:00 +0000

Forwarding for the depot's records.

The revised delivery date is 14 March 2025.
Example Ltd will ship 240 units to the Northfield depot as before.
The delay follows a shortage of housings from the subcontractor.

Operational questions should go to ops@example.com.
`,
		},

		// --- One unrelated document ------------------------------------------
		{
			Title:      "Office plant care rota",
			SourceType: domain.SourceTypeNote,
			MIME:       "text/plain",
			DaysAgo:    25,
			Role:       "Unrelated to everything else. Should not acquire relationships.",
			Body: `Plant care rota, agreed 2025-02-14.

Watering is on Mondays and Thursdays. The ficus by the window needs less water in winter
than the ferns. Feed everything monthly between March and September.

Whoever is last out on Friday should close the blinds on the south side.
`,
		},

		// --- A JSON document ------------------------------------------------
		{
			Title:      "Atlas shipment record (JSON)",
			SourceType: domain.SourceTypeJSON,
			MIME:       "application/json",
			Filename:   "atlas-shipment.json",
			DaysAgo:    10,
			Role:       "Structured data sharing entities and figures with the Atlas documents.",
			Metadata:   map[string]string{"project": "atlas"},
			Body: `{
  "shipment": {
    "project": "Atlas",
    "supplier": "Example Ltd",
    "destination": "Northfield depot",
    "units": 240,
    "scheduled_date": "2025-03-14",
    "contact": "ops@example.com"
  },
  "notes": [
    "Revised from the original date of 2025-03-03.",
    "Housing shortage reported by the subcontractor."
  ]
}`,
		},
	}
}

// demoSearches are the queries the dataset is built to answer.
var demoSearches = []struct{ Query, Shows string }{
	{"What information relates to the revised delivery date?",
		"The revised schedule, the original it replaces, the forwarded copy and the JSON shipment record."},
	{"Which sources disagree about the project cost?",
		"The original schedule (£42,000) and the finance note (£51,500), linked by a contradicts relationship."},
	{"Show information associated with Example Ltd.",
		"Every item naming the company, including the sponsorship note, which shares the entity but not the subject."},
	{"What replaced the original policy?",
		"Returns policy v3, related to the withdrawn v2."},
	{"Find items similar in meaning but not actually about the same event.",
		"The fire drill report and the safety inspection: similar wording, different events."},
}

// cmdSeed loads the demonstration dataset.
func cmdSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	configFile := fs.String("config", ".env", "path to a key=value configuration file (optional)")
	username := fs.String("username", "", "username to attribute the seeded items to (defaults to the first admin)")
	wait := fs.Bool("wait", false, "wait for processing to finish before returning")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait when --wait is set")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Load Relata's demonstration dataset.

The dataset contains two clearly related documents, two superficially similar but
different ones, a contradiction, an update that supersedes older information, a
document sharing an entity but not a subject, a likely duplicate, and an unrelated
document — so that the difference between similarity and evidence is visible.

Usage:
  relata seed [--wait]

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, cfg.LogJSON)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Minute)
	defer cancel()
	ctx = observability.WithLogger(ctx, logger)

	store, err := mongo.Connect(ctx, cfg.Mongo, nil)
	if err != nil {
		return fmt.Errorf("mongodb: %w", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	if err := store.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("create mongodb indexes: %w", err)
	}

	// Seeding is attributed to a real account, so the audit trail records who
	// the items belong to rather than inventing a system actor.
	user, err := seedUser(ctx, store, *username)
	if err != nil {
		return err
	}
	principal := &domain.Principal{
		UserID: user.ID, Username: user.Username, Role: user.Role, WorkspaceID: user.WorkspaceID,
	}

	recorder := audit.NewRecorder(store.Audit)
	ingestSvc := ingest.NewService(store.Items, store.Jobs, cfg, recorder, nil)

	docs := seedDocuments()
	fmt.Printf("Loading %d demonstration documents into workspace %s…\n\n", len(docs), user.WorkspaceID)

	var created []string
	for _, d := range docs {
		created2, err := seedOne(ctx, ingestSvc, principal, d)
		if err != nil {
			return err
		}
		created = append(created, created2)
	}

	fmt.Printf("\n%d documents queued for processing.\n", len(created))

	if *wait {
		fmt.Println("\nWaiting for background processing to finish…")
		if err := waitForProcessing(ctx, store, principal.WorkspaceID, len(created), *timeout); err != nil {
			return err
		}
		fmt.Println("Processing complete.")
	} else {
		fmt.Println("\nProcessing continues in the background. Watch it at /jobs.")
	}

	fmt.Print("\nWhat this dataset demonstrates:\n\n")
	for _, d := range docs {
		if d.Role != "" {
			fmt.Printf("  • %-46s %s\n", trimTo(d.Title, 46), d.Role)
		}
	}

	fmt.Print("\nDemonstration searches:\n\n")
	for _, s := range demoSearches {
		fmt.Printf("  %s\n      → %s\n\n", s.Query, s.Shows)
	}

	fmt.Printf("Open %s/search to try them.\n", cfg.HTTP.BaseURL)
	fmt.Println("\nNote: relationship strengths shown in the interface are heuristic scores")
	fmt.Println("computed from explicit signals. They are not calibrated probabilities, and an")
	fmt.Println("inferred relationship is a proposal until a person confirms it.")
	return nil
}

func seedOne(ctx context.Context, svc *ingest.Service, p *domain.Principal, d seedDoc) (string, error) {
	created := demoDate(d.DaysAgo)
	req := ingest.Request{
		Title:        d.Title,
		Content:      []byte(d.Body),
		DeclaredMIME: d.MIME,
		Filename:     d.Filename,
		SourceType:   d.SourceType,
		Metadata:     d.Metadata,
		CreatedAt:    &created,
		// The dataset deliberately contains a near-duplicate. It is a distinct
		// document, not byte-identical, so it is accepted normally — which is
		// the point: Relata flags near-duplicates rather than refusing them.
		AllowDuplicate: false,
	}

	res, err := svc.Ingest(ctx, p, req)
	if err != nil {
		var dup *ingest.DuplicateError
		if errors.As(err, &dup) {
			fmt.Printf("  – %-50s already present (identical content)\n", trimTo(d.Title, 50))
			return dup.ExistingID, nil
		}
		return "", fmt.Errorf("seed %q: %w", d.Title, err)
	}
	fmt.Printf("  ✓ %-50s %s\n", trimTo(d.Title, 50), res.Item.ID)
	return res.Item.ID, nil
}

// seedUser finds the account to attribute seeded items to.
func seedUser(ctx context.Context, store *mongo.Store, username string) (*domain.User, error) {
	if username != "" {
		u, err := store.Users.GetByUsername(ctx, username)
		if err != nil {
			return nil, fmt.Errorf("no such user %q — create one with: relata createadmin --username %s", username, username)
		}
		return u, nil
	}

	n, err := store.Users.Count(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("no accounts exist yet — create one first with: relata createadmin --username admin")
	}

	// Fall back to the default workspace's first administrator.
	ws, err := store.Workspaces.GetBySlug(ctx, "default")
	if err != nil {
		return nil, errors.New("no default workspace — create an administrator first with: relata createadmin --username admin")
	}
	for _, candidate := range []string{"admin", "administrator", "relata"} {
		if u, err := store.Users.GetByUsername(ctx, candidate); err == nil && u.WorkspaceID == ws.ID {
			return u, nil
		}
	}
	return nil, errors.New("could not determine which account to use — pass --username")
}

// waitForProcessing blocks until the queue drains or the timeout elapses.
func waitForProcessing(ctx context.Context, store *mongo.Store, workspaceID string, expected int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastReport := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		counts, err := store.Jobs.CountByStatus(ctx, workspaceID)
		if err != nil {
			return err
		}
		pending := counts[domain.JobPending] + counts[domain.JobRunning]

		if pending == 0 {
			ready, _ := store.Items.CountByStatus(ctx, workspaceID)
			done := ready[domain.StatusReady] + ready[domain.StatusPartial]
			if done >= int64(expected) || counts[domain.JobFailed] > 0 {
				if counts[domain.JobFailed] > 0 {
					fmt.Printf("  %d job(s) failed — see /jobs for details.\n", counts[domain.JobFailed])
				}
				return nil
			}
			// No queued work and not everything is ready: the workers are
			// probably not running.
			return errors.New("no jobs are queued but items are unprocessed — is `relata serve` running?")
		}

		if time.Since(lastReport) > 10*time.Second {
			fmt.Printf("  %d job(s) still running…\n", pending)
			lastReport = time.Now()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s with %d job(s) outstanding", timeout, pending)
		}
	}
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
