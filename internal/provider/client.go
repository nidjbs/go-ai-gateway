package provider

import "fmt"

func unsupportedField(name string) error {
	return &RequestError{Message: fmt.Sprintf("%s is not supported for Anthropic providers", name)}
}
