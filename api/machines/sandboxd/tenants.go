package sandboxd

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/loomhand/cocoonbox/api/machines"
)

var _ machines.Tenants = (*Tenants)(nil)

// Tenants manages the instance's tenants with its root api token, each in one egress class.
type Tenants struct {
	api         *client
	egressClass string
}

// NewTenants talks to the sandboxd instance at rawURL with its root api token; an empty egressClass gives tenants none.
func NewTenants(rawURL, token, egressClass string) (*Tenants, error) {
	api, err := newClient(rawURL, token)
	if err != nil {
		return nil, err
	}
	return &Tenants{api: api, egressClass: egressClass}, nil
}

func (t *Tenants) PutTenant(ctx context.Context, name, token string, maxClaims int) error {
	req := tenantRequest{Token: token, MaxClaims: maxClaims, EgressClass: t.egressClass}
	return t.api.do(ctx, http.MethodPut, "/v1/tenants/"+url.PathEscape(name), nil, req, nil)
}

func (t *Tenants) DeleteTenant(ctx context.Context, name string) error {
	if err := t.api.do(ctx, http.MethodDelete, "/v1/tenants/"+url.PathEscape(name), nil, nil, nil); err != nil && !errors.Is(err, machines.ErrNotFound) {
		return err
	}
	return nil
}
