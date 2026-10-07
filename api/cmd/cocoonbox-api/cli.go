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

	"github.com/CMGS/cocoonbox/api/boxes"
	"github.com/CMGS/cocoonbox/api/config"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/users"
)

const (
	cliUsage = `usage:
  cocoonbox-api [-config FILE] user add NAME [-id ID] [-admin]
  cocoonbox-api [-config FILE] user delete NAME
  cocoonbox-api [-config FILE] user passwd NAME -stdin
  cocoonbox-api [-config FILE] user token-sha256 NAME -stdin
  cocoonbox-api [-config FILE] state migrate [-keyring DIR]
  cocoonbox-api [-config FILE] state expand [-user ID]`

	defaultKeyringDir = "/var/lib/cocoonbox-api/keys"
)

type stateKeys interface {
	StateKey(ctx context.Context, user string) (keyring.Key, error)
	SetStateKey(ctx context.Context, user string, k keyring.Key) error
}

// runCLI serves the operator commands; secrets arrive on stdin and never on the command line.
func runCLI(ctx context.Context, cfg *config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) >= 2 && args[0] == "state" && args[1] == "migrate" {
		return migrateState(ctx, cfg, args[2:], stdout)
	}
	if len(args) >= 2 && args[0] == "state" && args[1] == "expand" {
		return expandState(ctx, cfg, args[2:], stdout)
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
	if verb == "add" {
		u, createErr := accounts.Create(ctx, users.NewUser{ID: *id, Name: name, Admin: *admin})
		if createErr != nil {
			return createErr
		}
		_, err = fmt.Fprintf(stdout, "created user %s with id %s\n", u.Name, u.ID)
		return err
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
		st, storeErr := newStore(cfg)
		if storeErr != nil {
			return storeErr
		}
		removed, stateErr := boxes.DeleteState(ctx, st, u.ID)
		if stateErr != nil {
			return fmt.Errorf("delete state of %s: %w", u.Name, stateErr)
		}
		if err = accounts.Delete(ctx, u.ID); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "deleted user %s and %d state objects\n", u.Name, removed)
		return err
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
	_, err = fmt.Fprintf(stdout, "%s of user %s updated\n", verb, u.Name)
	return err
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
	return nil
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
