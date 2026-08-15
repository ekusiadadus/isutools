package isutools

import (
	"runtime/pprof"
	"slices"
	"testing"
)

func TestResolveAdditionalRuntimeProfilesAreExplicitOptIn(t *testing.T) {
	settings := resolveProfileSettings(envMap(map[string]string{
		envAllocsProfile:        "on",
		envGoroutineProfile:     "1",
		envThreadcreateProfile:  "true",
		envGoroutineLeakProfile: "yes",
	}))
	if !settings.allocs || !settings.goroutine || !settings.threadcreate || !settings.goroutineleak {
		t.Fatalf("settings=%+v", settings)
	}
	kinds := settings.apply(nil)
	for _, kind := range []string{"allocs", "goroutine", "threadcreate"} {
		if !slices.Contains(kinds, kind) {
			t.Fatalf("kinds=%v missing %s", kinds, kind)
		}
	}
	if pprof.Lookup("goroutineleak") != nil && !slices.Contains(kinds, "goroutineleak") {
		t.Fatalf("kinds=%v missing supported goroutineleak", kinds)
	}
}

func TestAdditionalRuntimeProfileDefaultsDoNothing(t *testing.T) {
	settings := resolveProfileSettings(envMap(nil))
	if settings.allocs || settings.goroutine || settings.threadcreate || settings.goroutineleak {
		t.Fatalf("additional profiles must default off: %+v", settings)
	}
}
