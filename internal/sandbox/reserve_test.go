package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A session reserves its agent's CPUs at Open and the services' budget
// when Start starts a sidecar, and Close gives both back, once; a
// WithReserve(nil) copy reserves nothing.
func TestASessionReservesItsCPUs(t *testing.T) {
	var mu sync.Mutex
	held := 0.0
	reserve := func(cpus float64) func() {
		mu.Lock()
		held += cpus
		mu.Unlock()
		return func() {
			mu.Lock()
			held -= cpus
			mu.Unlock()
		}
	}
	now := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return held
	}
	rt := &Fake{}
	sb := &Sandbox{Runtime: rt, TempDir: t.TempDir(), Config: Config{Limits: Limits{CPUs: 6}, ServicesLimits: Limits{CPUs: 2}}, Reserve: reserve}
	sess, err := sb.Open("g/1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if got := now(); got != 6 {
		t.Fatalf("after Open %g, want the agent's 6", got)
	}
	if _, err := sess.Start(context.Background(), []Service{{Name: "cache", Image: "redis:7", Ports: []string{"6379"}}}, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := now(); got != 8 {
		t.Fatalf("after Start %g, want 6 + the services' 2", got)
	}
	sess.Close()
	sess.Close()
	if got := now(); got != 0 {
		t.Fatalf("after Close %g, want 0", got)
	}
	plain, err := sb.WithReserve(nil).Open("g/2", "validate", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if got := now(); got != 0 {
		t.Fatalf("a WithReserve(nil) session reserved %g", got)
	}
}
