package connect

import (
	"context"
	"encoding/json/v2"

	"github.com/loomhand/cocoonbox/api/keyring"
)

type secrets struct {
	AccessToken  string `json:"access_token,omitzero"`
	RefreshToken string `json:"refresh_token,omitzero"`
	APIKey       string `json:"api_key,omitzero"`
	Header       string `json:"header,omitzero"`
	// Client is the registered client of an MCP-authorized grant; a catalog grant uses the catalog's.
	Client client `json:"client,omitzero"`
}

type flowSecrets struct {
	Verifier string   `json:"verifier"`
	Client   client   `json:"client,omitzero"`
	Scopes   []string `json:"scopes,omitzero"`
}

// sealer seals grant and flow secrets as JSON with keyring.Sealer.
type sealer struct {
	keyring.Sealer
}

func (s *sealer) seal(ctx context.Context, user string, v any, ad string) ([]byte, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return s.Seal(ctx, user, plain, ad)
}

func (s *sealer) open(ctx context.Context, user string, sealed []byte, ad string, v any) error {
	plain, err := s.Open(ctx, user, sealed, ad)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, v)
}

func grantData(id, user, connector string) string {
	return "connect_grants|" + id + "|" + user + "|" + connector
}

func flowData(id, user, connector string) string {
	return "connect_flows|" + id + "|" + user + "|" + connector
}
