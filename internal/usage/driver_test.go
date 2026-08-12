package usage

import "testing"

func TestRegistryHasNoop(t *testing.T) {
	sink, err := Registry.Build("noop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("noop driver returned %T; want NoopSink", sink)
	}
}

func TestRegistryUnknownDriver(t *testing.T) {
	if _, err := Registry.Build("redis", nil); err == nil {
		t.Fatal("expected error for unregistered driver")
	}
}
