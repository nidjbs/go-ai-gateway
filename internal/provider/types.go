package provider

import "fmt"

type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("upstream returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("upstream returned HTTP %d: %s", e.StatusCode, e.Message)
}
