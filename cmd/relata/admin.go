package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/storage/mongo"
)

// loggerType is the logger the commands share.
type loggerType = *slog.Logger

// newID returns a fresh identifier.
func newID() string { return ids.New() }

// cmdCreateAdmin creates the first administrator account.
//
// The password is read from the terminal without echo, or from an environment
// variable for automated setup. It is never accepted as a command-line flag,
// because arguments are visible in the process list and shell history.
func cmdCreateAdmin(args []string) error {
	fs := flag.NewFlagSet("createadmin", flag.ExitOnError)
	configFile := fs.String("config", ".env", "path to a key=value configuration file (optional)")
	username := fs.String("username", "", "username for the new administrator (required)")
	displayName := fs.String("display-name", "", "display name (optional)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Create the initial administrator account.

Usage:
  relata createadmin --username <name>

The password is read from the terminal without echo. For unattended setup,
set RELATA_ADMIN_PASSWORD in the environment instead.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*username) == "" {
		fs.Usage()
		return errors.New("--username is required")
	}
	name, err := auth.ValidateUsername(*username)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, cfg.LogJSON)

	password, err := readPassword()
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := mongo.Connect(ctx, cfg.Mongo, nil)
	if err != nil {
		return fmt.Errorf("mongodb: %w", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	if err := store.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("create mongodb indexes: %w", err)
	}

	ws, err := ensureWorkspace(ctx, store)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	// The plain-text password is not referenced again from here on.

	now := time.Now().UTC()
	user := &domain.User{
		ID:           newID(),
		Username:     name,
		PasswordHash: hash,
		DisplayName:  strings.TrimSpace(*displayName),
		Role:         domain.RoleAdmin,
		WorkspaceID:  ws.ID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := store.Users.Create(ctx, user); err != nil {
		if errors.Is(err, domain.ErrDuplicate) {
			return fmt.Errorf("a user named %q already exists", name)
		}
		return err
	}

	logger.Info("administrator created", "username", name, "workspace", ws.Slug)
	fmt.Printf("Administrator %q created in workspace %q.\nSign in at %s\n",
		name, ws.Slug, cfg.HTTP.BaseURL)
	return nil
}

// readPassword obtains the password without echoing it and without ever
// placing it on the command line.
func readPassword() (string, error) {
	if env := os.Getenv("RELATA_ADMIN_PASSWORD"); env != "" {
		return env, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Piped input: read a single line. Useful in scripts, and no less safe
		// than an environment variable.
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			return "", errors.New("no password supplied on standard input")
		}
		return strings.TrimRight(sc.Text(), "\r\n"), nil
	}

	fmt.Print("Password (at least 12 characters): ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	fmt.Print("Confirm password: ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	if string(first) != string(second) {
		return "", errors.New("the passwords do not match")
	}
	return string(first), nil
}
