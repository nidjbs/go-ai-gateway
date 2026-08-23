package revocation

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestMemoryRevokeAndCheck(t *testing.T) {
	s := NewMemory()
	if s.IsRevoked("key-a") {
		t.Fatal("fresh store must not report revoked")
	}
	if err := s.Revoke(context.Background(), "key-a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !s.IsRevoked("key-a") {
		t.Fatal("expected key-a revoked after Revoke")
	}
	if s.IsRevoked("key-b") {
		t.Fatal("key-b must stay unrevoked")
	}
	if err := s.Revoke(context.Background(), "key-b"); err != nil {
		t.Fatalf("revoke key-b: %v", err)
	}
	got := s.RevokedKeys()
	if len(got) != 2 {
		t.Fatalf("RevokedKeys len = %d, want 2", len(got))
	}
}

func TestMemoryEmptyKeyID(t *testing.T) {
	s := NewMemory()
	if err := s.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("revoke empty: %v", err)
	}
	if s.IsRevoked("") {
		t.Fatal("empty keyID must never be revoked")
	}
}

func TestRedisRevokeAcrossLookup(t *testing.T) {
	srv := miniredis.RunT(t)
	s, err := NewRedis(map[string]any{"addr": srv.Addr()}, 50*time.Millisecond, 0, nil)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	defer s.Close()

	if s.IsRevoked("key-a") {
		t.Fatal("fresh redis store must not report revoked")
	}
	if err := s.Revoke(context.Background(), "key-a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Immediate local-cache hit.
	if !s.IsRevoked("key-a") {
		t.Fatal("expected key-a revoked immediately")
	}
	// A different store instance (simulating another replica) sees the
	// revocation through Redis.
	s2, err := NewRedis(map[string]any{"addr": srv.Addr()}, 0, 0, nil)
	if err != nil {
		t.Fatalf("NewRedis s2: %v", err)
	}
	defer s2.Close()
	if !s2.IsRevoked("key-a") {
		t.Fatal("second store must see redis-backed revocation")
	}
	if s2.IsRevoked("key-b") {
		t.Fatal("key-b must stay unrevoked")
	}
}

func TestRedisRevokeTTL(t *testing.T) {
	srv := miniredis.RunT(t)
	s, err := NewRedis(map[string]any{"addr": srv.Addr()}, 0, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	defer s.Close()
	if err := s.Revoke(context.Background(), "expiring"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !s.IsRevoked("expiring") {
		t.Fatal("expected revoked before TTL expiry")
	}
	srv.FastForward(100 * time.Millisecond)
	// Local cache (TTL 0 = disabled cache? cacheTTL=0 -> default 5s in New,
	// but NewRedis with cacheTTL 0 disables? we pass cacheTTL=0 -> keep 0)
	// Force a backend re-read by using a fresh store.
	s3, err := NewRedis(map[string]any{"addr": srv.Addr()}, 0, 0, nil)
	if err != nil {
		t.Fatalf("NewRedis s3: %v", err)
	}
	defer s3.Close()
	if s3.IsRevoked("expiring") {
		t.Fatal("revocation should have expired after TTL")
	}
}

func TestRedisRevokedKeys(t *testing.T) {
	srv := miniredis.RunT(t)
	s, err := NewRedis(map[string]any{"addr": srv.Addr()}, 0, 0, nil)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	defer s.Close()
	if err := s.Revoke(context.Background(), "k1"); err != nil {
		t.Fatalf("revoke k1: %v", err)
	}
	if err := s.Revoke(context.Background(), "k2"); err != nil {
		t.Fatalf("revoke k2: %v", err)
	}
	got := s.RevokedKeys()
	if len(got) != 2 {
		t.Fatalf("RevokedKeys len = %d, want 2 (got %v)", len(got), got)
	}
}

func TestNewUnknownDriver(t *testing.T) {
	if _, err := New(configDriver("bogus"), 0, 0, nil); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}
