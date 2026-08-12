package registry

import (
	"errors"
	"testing"
)

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry[int]()
	r.Register("alpha", func(map[string]any) (int, error) { return 1, nil })
	r.Register("beta", func(map[string]any) (int, error) { return 2, nil })

	got, ok := r.Lookup("alpha")
	if !ok {
		t.Fatal("alpha not found")
	}
	v, err := got(nil)
	if err != nil || v != 1 {
		t.Fatalf("alpha build = %d, %v; want 1, nil", v, err)
	}

	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("missing unexpectedly found")
	}
}

func TestRegistryBuildUnknown(t *testing.T) {
	r := NewRegistry[string]()
	_, err := r.Build("nope", nil)
	if err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestRegistryOverwrite(t *testing.T) {
	r := NewRegistry[int]()
	r.Register("x", func(map[string]any) (int, error) { return 1, nil })
	r.Register("x", func(map[string]any) (int, error) { return 2, nil })

	v, err := r.Build("x", nil)
	if err != nil || v != 2 {
		t.Fatalf("after overwrite build = %d, %v; want 2, nil", v, err)
	}
}

func TestRegistryNames(t *testing.T) {
	r := NewRegistry[struct{}]()
	r.Register("b", func(map[string]any) (struct{}, error) { return struct{}{}, nil })
	r.Register("a", func(map[string]any) (struct{}, error) { return struct{}{}, nil })
	r.Register("c", func(map[string]any) (struct{}, error) { return struct{}{}, nil })

	got := r.Names()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Names()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestRegistryRegisterIgnoresEmpty(t *testing.T) {
	r := NewRegistry[int]()
	r.Register("", func(map[string]any) (int, error) { return 1, nil })
	r.Register("ok", nil)
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("empty/nil registrations should be dropped; got %v", names)
	}
}

func TestRegistryBuildPropagatesFactoryError(t *testing.T) {
	r := NewRegistry[int]()
	want := errors.New("boom")
	r.Register("failing", func(map[string]any) (int, error) { return 0, want })

	_, err := r.Build("failing", nil)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v; want %v", err, want)
	}
}
