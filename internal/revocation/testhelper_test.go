package revocation

import "github.com/nidjbs/go-ai-gateway/internal/config"

func configDriver(name string) config.StorageDriver {
	return config.StorageDriver{Driver: name, Options: map[string]any{}}
}
