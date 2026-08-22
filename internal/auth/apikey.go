package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

type apiKeyRecord struct {
	teamID   string
	apiKeyID string
	digest   string
	limits   config.KeyLimits
}

// keyDigest normalizes a configured key into its SHA-256 hex digest. A key
// configured as "sha256:<hex>" is validated and used verbatim; any other
// value is treated as a plaintext key and hashed at startup, so plaintext
// never needs to live in process memory. Clients always authenticate with
// the plaintext key, which is hashed per request before lookup.
func keyDigest(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(value, "sha256:") {
		hexPart := strings.TrimPrefix(value, "sha256:")
		if len(hexPart) != sha256.Size*2 {
			return "", fmt.Errorf("invalid sha256 key digest (want %d hex chars)", sha256.Size*2)
		}
		if _, err := hex.DecodeString(hexPart); err != nil {
			return "", fmt.Errorf("invalid sha256 key digest: %w", err)
		}
		return strings.ToLower(hexPart), nil
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:]), nil
}

type apiKeyAuthenticator struct {
	keys map[string]apiKeyRecord
}

func newAPIKeyAuthenticator(teams []config.TeamConfig) (Authenticator, error) {
	keys := make(map[string]apiKeyRecord)
	for _, team := range teams {
		for _, key := range team.APIKeys {
			digest, err := keyDigest(key.Key)
			if err != nil {
				return nil, fmt.Errorf("team %q api key %q: %w", team.ID, key.ID, err)
			}
			if _, exists := keys[digest]; exists {
				return nil, fmt.Errorf("duplicate api key while building auth registry")
			}
			keys[digest] = apiKeyRecord{
				teamID:   team.ID,
				apiKeyID: key.ID,
				digest:   digest,
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
	digest, err := keyDigest(provided)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	record, ok := a.keys[digest]
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
	digest, err := keyDigest(rawKey)
	if err != nil {
		return apiKeyRecord{}, false
	}
	record, ok := a.keys[digest]
	return record, ok
}
