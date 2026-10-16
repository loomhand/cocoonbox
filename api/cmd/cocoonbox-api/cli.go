package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/config"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/store"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	cliUsage = `usage:
  cocoonbox-api [-config FILE] user add NAME [-id ID] [-admin]
  cocoonbox-api [-config FILE] user delete NAME
  cocoonbox-api [-config FILE] user passwd NAME -stdin
  cocoonbox-api [-config FILE] user token-sha256 NAME -stdin
  cocoonbox-api [-config FILE] state migrate [-keyring DIR]
  cocoonbox-api [-config FILE] state expand [-user ID]
  cocoonbox-api [-config FILE] box rebase USER BOX IMAGE`

	defaultKeyringDir = "/var/lib/cocoonbox-api/keys"
)

type stateKeys interface {
	StateKey(ctx context.Context, user string) (keyring.Key, error)
	SetStateKey(ctx context.Context, user string, k keyring.Key) error
}

type connections interface {
	RevokeAll(ctx context.Context, user string) error
}

type accountDeleter interface {
	Delete(ctx context.Context, id string) error
}

// runCLI serves the operator commands; secrets arrive on stdin and never on the command line.
func runCLI(ctx context.Context, cfg *config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) >= 2 && args[0] == "state" && args[1] == "migrate" {
		return migrateState(ctx, cfg, args[2:], stdout)
	}
	if len(args) >= 2 && args[0] == "state" && args[1] == "expand" {
		return expandState(ctx, cfg, args[2:], stdout)
	}
	if len(args) >= 2 && args[0] == "box" && args[1] == "rebase" {
		return rebaseBox(ctx, cfg, args[2:], stdout)
	}
	if len(args) < 3 || args[0] != "user" {
		return errors.New(cliUsage)
	}
	verb, name := args[1], args[2]
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	id := fs.String("id", "", "user id; migrated users keep their old name so their box id stays")
	admin := fs.Bool("admin", false, "make the user an admin")
	fromStdin := fs.Bool("stdin", false, "read the secret from the first line of stdin")
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	accounts, err := openUsers(ctx, cfg)
	if err != nil {
		return err
	}
	defer accounts.Close()
	written := func(format string, args ...any) error {
		if notifyErr := accounts.Notify(ctx); notifyErr != nil {
			return notifyErr
		}
		_, printErr := fmt.Fprintf(stdout, format, args...)
		return printErr
	}
	if verb == "add" {
		u, createErr := accounts.Create(ctx, users.NewUser{ID: *id, Name: name, Admin: *admin})
		if createErr != nil {
			return createErr
		}
		return written("created user %s with id %s\n", u.Name, u.ID)
	}
	u, err := accounts.ByName(name)
	if err != nil {
		return err
	}
	if verb == "delete" {
		// A user who never had an API token never owned a box, so there is nothing to release.
		if u.HasToken {
			return errors.New("the user has an API token and may own a box: delete it in the console, which releases its boxes")
		}
		removed, deleteErr := removeUser(ctx, cfg, accounts, u)
		if deleteErr != nil {
			return deleteErr
		}
		return written("deleted user %s and %d state objects\n", u.Name, removed)
	}
	if !*fromStdin {
		return errors.New(cliUsage)
	}
	secret, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read stdin: %w", err)
	}
	secret = strings.TrimRight(secret, "\r\n")
	switch verb {
	case "passwd":
		err = accounts.SetPassword(ctx, u.ID, secret)
	case "token-sha256":
		err = accounts.SetTokenSHA256(ctx, u.ID, secret)
	default:
		return errors.New(cliUsage)
	}
	if err != nil {
		return err
	}
	return written("%s of user %s updated\n", verb, u.Name)
}

func removeUser(ctx context.Context, cfg *config.Config, accounts *users.Service, u users.User) (int, error) {
	st, err := newStore(cfg)
	if err != nil {
		return 0, err
	}
	var revoker connections
	if cfg.Connect.PublicURL != "" {
		broker, err := openConnect(ctx, cfg, accounts)
		if err != nil {
			return 0, err
		}
		defer broker.Close()
		revoker = broker
	}
	return deleteUser(ctx, st, revoker, accounts, u)
}

func deleteUser(ctx context.Context, st store.Store, revoker connections, accounts accountDeleter, u users.User) (int, error) {
	removed, err := boxes.DeleteState(ctx, st, u.ID)
	if err != nil {
		return 0, fmt.Errorf("delete state of %s: %w", u.Name, err)
	}
	if revoker != nil {
		if err := revoker.RevokeAll(ctx, u.ID); err != nil {
			return removed, fmt.Errorf("revoke connections of %s: %w", u.Name, err)
		}
	}
	return removed, accounts.Delete(ctx, u.ID)
}

// migrateState gives every user a state key in the users table and copies its box into the per-user store layout; a rerun is safe.
func migrateState(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dir := flags.String("keyring", defaultKeyringDir, "directory of the per-box key files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	st, err := newStore(cfg)
	if err != nil {
		return err
	}
	accounts, err := openUsers(ctx, cfg)
	if err != nil {
		return err
	}
	defer accounts.Close()
	for _, u := range accounts.List() {
		box := boxes.BoxID(u.ID)
		if err := importStateKey(ctx, accounts, u.ID, filepath.Join(*dir, box+".json")); err != nil {
			return fmt.Errorf("state key of %s: %w", u.Name, err)
		}
		copied, err := boxes.MigrateState(ctx, st, u.ID)
		if err != nil {
			return fmt.Errorf("migrate state of %s: %w", u.Name, err)
		}
		if _, err := fmt.Fprintf(stdout, "user %s (%s): state key set, %d objects copied from %s/\n", u.Name, u.ID, copied, box); err != nil {
			return err
		}
	}
	return accounts.Notify(ctx)
}

// expandState rewrites every delta record and manifest delta as full objects, for a rollback to an api without delta records; stop the api first.
func expandState(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("expand", flag.ContinueOnError)
	only := flags.String("user", "", "expand only this user id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	st, err := newStore(cfg)
	if err != nil {
		return err
	}
	accounts, err := openUsers(ctx, cfg)
	if err != nil {
		return err
	}
	defer accounts.Close()
	for _, u := range accounts.List() {
		if *only != "" && u.ID != *only {
			continue
		}
		n, err := boxes.ExpandState(ctx, st, accounts, u.ID)
		if err != nil {
			return fmt.Errorf("expand state of %s: %w", u.Name, err)
		}
		if _, err := fmt.Fprintf(stdout, "user %s (%s): %d snapshots made full\n", u.Name, u.ID, n); err != nil {
			return err
		}
	}
	return nil
}

// importStateKey gives user the key in the box key file at path, or a new key when there is no file and the user has none.
func importStateKey(ctx context.Context, keys stateKeys, user, path string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // path is built from a box id
	if errors.Is(err, fs.ErrNotExist) {
		if _, err = keys.StateKey(ctx, user); err == nil {
			return nil
		}
		return keys.SetStateKey(ctx, user, keyring.Generate(user))
	}
	if err != nil {
		return err
	}
	var file struct {
		ID     string `json:"id"`
		Secret []byte `json:"secret"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return keys.SetStateKey(ctx, user, keyring.Key{ID: file.ID, Secret: file.Secret})
}
