package vault

import (
	"context"
	"errors"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

func (s *Service) Add(ctx context.Context, req *controlv1.AddKeyRequest) (*controlv1.Key, error) {
	owner, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err = checkName(req.GetName()); err != nil {
		return nil, keyStatus(err)
	}
	sp, err := checkSpec(req.GetHosts(), req.GetHeader(), req.GetTemplate(), req.GetQuery())
	if err != nil {
		return nil, keyStatus(err)
	}
	value := req.GetValue()
	if err = checkValue(value); err != nil {
		return nil, keyStatus(err)
	}
	sec := &secret{
		id: idPrefix + randomHex(idBytes), owner: owner, name: req.GetName(), hosts: sp.hosts, header: sp.header, template: sp.template,
		query: sp.query, placeholder: newPlaceholder(value), hint: valueHint(value), fingerprint: fingerprint(value), valueLen: len(value), version: 1,
	}
	if sec.sealed, err = s.seal.seal(ctx, owner, value, valueData(sec.id, owner)); err != nil {
		return nil, err
	}
	if err := s.store.insert(ctx, sec, func(others []*secret) error { return fits(append(others, sec)) }); err != nil {
		return nil, keyStatus(err)
	}
	s.audit(ctx, sec, opCreate)
	s.changed(ctx, owner)
	return sec.key(), nil
}

func (s *Service) List(ctx context.Context, _ *controlv1.ListKeysRequest) (*controlv1.ListKeysResponse, error) {
	owner, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.store.secrets(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := &controlv1.ListKeysResponse{Keys: make([]*controlv1.Key, 0, len(list))}
	for _, sec := range list {
		out.Keys = append(out.Keys, sec.key())
	}
	return out, nil
}

func (s *Service) Rotate(ctx context.Context, req *controlv1.RotateKeyRequest) (*controlv1.Key, error) {
	owner, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	value := req.GetValue()
	if err = checkValue(value); err != nil {
		return nil, keyStatus(err)
	}
	sec, err := s.store.rotate(ctx, owner, req.GetName(), func(sec *secret) error {
		sealed, sealErr := s.seal.seal(ctx, owner, value, valueData(sec.id, owner))
		if sealErr != nil {
			return sealErr
		}
		sec.sealed, sec.hint, sec.fingerprint, sec.valueLen = sealed, valueHint(value), fingerprint(value), len(value)
		return nil
	})
	if err != nil {
		return nil, keyStatus(err)
	}
	s.audit(ctx, sec, opRotate)
	s.changed(ctx, owner)
	return sec.key(), nil
}

func (s *Service) Delete(ctx context.Context, ref *controlv1.KeyRef) (*controlv1.DeleteKeyResponse, error) {
	owner, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.store.remove(ctx, owner, ref.GetName())
	if err != nil {
		return nil, keyStatus(err)
	}
	s.audit(ctx, &secret{id: id, owner: owner, name: ref.GetName()}, opDelete)
	s.changed(ctx, owner)
	return &controlv1.DeleteKeyResponse{}, nil
}

// audit records op on sec; a failure is logged, since the change itself is done.
func (s *Service) audit(ctx context.Context, sec *secret, op string) {
	actor := actorOf(ctx)
	logger := log.WithFunc("vault.Service.audit")
	if err := s.store.audit(ctx, sec.owner, sec.id, sec.name, op, actor); err != nil {
		logger.Error(ctx, err, "write the audit record")
	}
	logger.Infof(ctx, "user %s: %s key %s (%s) from %s", sec.owner, op, sec.name, sec.id, actor)
}

func callerID(ctx context.Context) (string, error) {
	user := auth.User(ctx)
	if user == "" {
		return "", status.Error(codes.Unauthenticated, "missing user")
	}
	return user, nil
}

func keyStatus(err error) error {
	switch {
	case errors.Is(err, errInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, errExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, errNoKey):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, errQuota):
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return err
}
