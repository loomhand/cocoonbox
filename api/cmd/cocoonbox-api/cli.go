package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/CMGS/cocoonbox/api/config"
	"github.com/CMGS/cocoonbox/api/users"
)

const cliUsage = `usage:
  cocoonbox-api [-config FILE] user add NAME [-id ID] [-admin]
  cocoonbox-api [-config FILE] user delete NAME
  cocoonbox-api [-config FILE] user passwd NAME -stdin
  cocoonbox-api [-config FILE] user token-sha256 NAME -stdin`

// runCLI serves the operator commands; secrets arrive on stdin and never on the command line.
func runCLI(ctx context.Context, cfg *config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
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
		if err = accounts.Delete(ctx, u.ID); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "deleted user %s\n", u.Name)
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
