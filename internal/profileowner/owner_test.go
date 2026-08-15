package profileowner

import "testing"

func TestRegistrySerializesOwners(t *testing.T) {
	var registry Registry
	if !registry.Acquire("cpu") || registry.Acquire("trace") || registry.Active() != "cpu" {
		t.Fatalf("active=%q", registry.Active())
	}
	if registry.Release("trace") || registry.Active() != "cpu" {
		t.Fatalf("wrong owner released registry")
	}
	if !registry.Release("cpu") || !registry.Acquire("trace") {
		t.Fatalf("registry did not become available")
	}
}
