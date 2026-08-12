package main

import (
	"crypto/rand"
	"strings"
	"testing"
)

func TestGenerateReturnsSKPrefixedToken(t *testing.T) {
	key, err := generateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "sk-") {
		t.Fatalf("key = %q, want sk- prefix", key)
	}
	if len(key) <= len("sk-") {
		t.Fatalf("key = %q, want longer token", key)
	}
}

func TestGenerateProducesUniqueKeys(t *testing.T) {
	first, err := generateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("keys collided: %q", first)
	}
}
