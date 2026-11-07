package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MachineKey is a gw key issued to one machine incarnation, named by its ak_id; Installed means the machine's node holds it.
type MachineKey struct {
	ID          string
	Machine     string
	Incarnation string
	Installed   bool
}

// IssueMachineKey adds a gw key of owner for machine's incarnation, records it and returns it with its ak_id; a disabled owner's key is born expired.
func (s *Service) IssueMachineKey(ctx context.Context, owner, machine, incarnation string) (string, string, error) {
	r, err := s.record(owner)
	if err != nil {
		return "", "", err
	}
	key, id, err := s.gw.AddMachine(ctx, owner, !r.Disabled)
	if err != nil {
		return "", "", fmt.Errorf("add gw key: %w", err)
	}
	if err := s.store.exec(ctx, "INSERT INTO machine_keys (ak_id, machine_id, user_id, incarnation) VALUES ($1, $2, $3, $4)", id, machine, owner, incarnation); err != nil {
		return "", "", errors.Join(err, s.gw.Delete(context.WithoutCancel(ctx), id))
	}
	return key, id, nil
}

// MachineKeys lists the recorded gw keys of machine, of every machine when machine is empty, the newest first.
func (s *Service) MachineKeys(ctx context.Context, machine string) ([]MachineKey, error) {
	rows, err := s.store.pool.Query(ctx, `SELECT ak_id, machine_id, incarnation, installed FROM machine_keys WHERE $1 = '' OR machine_id = $1 ORDER BY created_at DESC`, machine)
	if err != nil {
		return nil, fmt.Errorf("list machine keys: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[MachineKey])
}

// InstallMachineKey records that machine's node holds the gw key with ak_id id and none of its other keys, or none at all for an empty id.
func (s *Service) InstallMachineKey(ctx context.Context, machine, id string) error {
	return s.store.exec(ctx, "UPDATE machine_keys SET installed = (ak_id = $2) WHERE machine_id = $1", machine, id)
}

// RevokeMachineKey revokes the gw key with ak_id id and forgets it.
func (s *Service) RevokeMachineKey(ctx context.Context, id string) error {
	if err := s.gw.Delete(ctx, id); err != nil {
		return fmt.Errorf("revoke gw key %s: %w", id, err)
	}
	return s.store.exec(ctx, "DELETE FROM machine_keys WHERE ak_id = $1", id)
}
