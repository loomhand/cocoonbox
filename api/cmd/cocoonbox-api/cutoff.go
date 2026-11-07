package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/loomhand/cocoonbox/api/config"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/vault"
)

type pushLedger interface {
	PushesOn(ctx context.Context, node string) ([]users.Push, error)
	DropPushesOn(ctx context.Context, node string) error
	MachineKeys(ctx context.Context, machine string) ([]users.MachineKey, error)
	RevokeMachineKey(ctx context.Context, id string) error
}

type grantRevoker interface {
	DropDoorKey(ctx context.Context, machine string) error
	RevokeAll(ctx context.Context, user string) error
}

type keyNames interface {
	Names(ctx context.Context, owner string) ([]string, error)
}

// cutoffNode revokes what the control plane owns of the credentials pushed to node and lists what the operator follows up.
func cutoffNode(ctx context.Context, cfg *config.Config, node string, stdout io.Writer) error {
	accounts, err := openUsers(ctx, cfg)
	if err != nil {
		return err
	}
	defer accounts.Close()
	var revoker grantRevoker
	if cfg.Connect.PublicURL != "" {
		broker, openErr := openConnect(ctx, cfg, accounts)
		if openErr != nil {
			return openErr
		}
		defer broker.Close()
		revoker = broker
	}
	dsn, err := cfg.DSN()
	if err != nil {
		return err
	}
	keys, err := vault.Open(ctx, vault.Options{DSN: dsn, Keys: accounts})
	if err != nil {
		return err
	}
	defer keys.Close()
	return cutoff(ctx, accounts, revoker, keys, node, stdout)
}

// cutoff rotates the gw and door keys of every machine that had secrets pushed to node, revokes the Connect grants of their owners at the vendors, lists the vault keys to rotate, and forgets node's pushes once all of it succeeded.
func cutoff(ctx context.Context, ledger pushLedger, revoker grantRevoker, keys keyNames, node string, stdout io.Writer) error {
	pushes, err := ledger.PushesOn(ctx, node)
	if err != nil {
		return err
	}
	kinds, owners := map[string][]string{}, map[string]string{}
	connectOwners, vaultOwners := map[string]bool{}, map[string]bool{}
	for _, p := range pushes {
		kinds[p.Machine], owners[p.Machine] = append(kinds[p.Machine], p.Kind), p.Owner
		switch p.Kind {
		case users.PushConnect:
			connectOwners[p.Owner] = true
		case users.PushVault:
			vaultOwners[p.Owner] = true
		}
	}
	if len(kinds) == 0 {
		_, err := fmt.Fprintf(stdout, "no credential pushes recorded for node %s\n", node)
		return err
	}
	if revoker == nil && slices.ContainsFunc(pushes, func(p users.Push) bool { return p.Kind == users.PushDoor || p.Kind == users.PushConnect }) {
		return errors.New("the node got Connect credentials: run the cutoff with connect.public_url set")
	}
	for _, machine := range slices.Sorted(maps.Keys(kinds)) {
		issued, err := ledger.MachineKeys(ctx, machine)
		if err != nil {
			return err
		}
		for _, k := range issued {
			if err := ledger.RevokeMachineKey(ctx, k.ID); err != nil {
				return err
			}
		}
		if revoker != nil {
			if err := revoker.DropDoorKey(ctx, machine); err != nil {
				return fmt.Errorf("drop the door key of %s: %w", machine, err)
			}
		}
		if _, err := fmt.Fprintf(stdout, "machine %s of user %s got %s: its gw and door keys are revoked; the provisioner gives it new ones while it runs\n",
			machine, owners[machine], strings.Join(kinds[machine], ", ")); err != nil {
			return err
		}
	}
	for _, owner := range slices.Sorted(maps.Keys(connectOwners)) {
		if err := revoker.RevokeAll(ctx, owner); err != nil {
			return fmt.Errorf("revoke the grants of %s: %w", owner, err)
		}
		if _, err := fmt.Fprintf(stdout, "user %s: Connect grants revoked at the vendors that support it; the user must reconnect\n", owner); err != nil {
			return err
		}
	}
	for _, owner := range slices.Sorted(maps.Keys(vaultOwners)) {
		names, err := keys.Names(ctx, owner)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(stdout, "user %s: tell the user to rotate vault keys %s\n", owner, strings.Join(names, ", ")); err != nil {
			return err
		}
	}
	return ledger.DropPushesOn(ctx, node)
}
