package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"example.com/light-llm-gateway/internal/config"
)

type apiKeyRecord struct {
	teamID   string
	apiKeyID string
	key      string
	limits   config.KeyLimits
}

type apiKeyAuthenticator struct {
	keys map[string]apiKeyRecord
}

func newAPIKeyAuthenticator(teams []config.TeamConfig) (Authenticator, error) {
	keys := make(map[string]apiKeyRecord)
	for _, team := range teams {
		for _, key := range team.APIKeys {
			raw := strings.TrimSpace(key.Key)
			if _, exists := keys[raw]; exists {
				return nil, fmt.Errorf("duplicate api key while building auth registry")
			}
			keys[raw] = apiKeyRecord{
				teamID:   team.ID,
				apiKeyID: key.ID,
				key:      raw,
				limits:   key.Limits,
			}
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("api-key authentication requires at least one configured key")
	}
	return apiKeyAuthenticator{keys: keys}, nil
}

func (a apiKeyAuthenticator) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	provided, ok := bearerToken(r)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	record, ok := a.keys[provided]
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		Subject:  record.apiKeyID,
		APIKeyID: record.apiKeyID,
		TeamID:   record.teamID,
	}, nil
}

func (a apiKeyAuthenticator) Lookup(rawKey string) (apiKeyRecord, bool) {
	record, ok := a.keys[strings.TrimSpace(rawKey)]
	return record, ok
}
