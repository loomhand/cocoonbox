package sandboxd

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/CMGS/cocoonbox/api/machines"
)

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

// Put adds or changes tenant name with its claim cap and the egress class; an empty token keeps the stored one.
func (t *Tenants) Put(ctx context.Context, name, token string, maxClaims int) error {
	req := tenantRequest{Token: token, MaxClaims: maxClaims, EgressClass: t.egressClass}
	return t.api.do(ctx, http.MethodPut, "/v1/tenants/"+url.PathEscape(name), nil, req, nil)
}

// Delete removes tenant name; removing an absent tenant succeeds.
func (t *Tenants) Delete(ctx context.Context, name string) error {
	if err := t.api.do(ctx, http.MethodDelete, "/v1/tenants/"+url.PathEscape(name), nil, nil, nil); err != nil && !errors.Is(err, machines.ErrNotFound) {
		return err
	}
	return nil
}
