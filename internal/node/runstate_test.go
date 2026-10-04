package node

import "testing"

// WriteScaling leaves node.scaling for a fixed node while its host
// report is hot or it is still climbing back toward the plan figure,
// and removes it once the fixed node is level and quiet — the rule
// adr-004's amendment adds beside the existing one for a ranged node
// (always written) and a bare fixed node (never, with no host report
// at all).
func TestWriteScalingFixedNodeHostRule(t *testing.T) {
	s := RunState{Dir: t.TempDir()}

	hot := Scaling{Capacity: 1, Min: 1, Max: 4, Reason: "host hot: load 55/18 cores", Host: &HostReport{Load: 55, Cores: 18, Hot: true}}
	if err := s.WriteScaling(hot, false); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Scaling(); !ok || got.Capacity != 1 {
		t.Fatalf("hot fixed node: Scaling() = %+v, %v; want it left", got, ok)
	}

	climbing := Scaling{Capacity: 3, Min: 1, Max: 4, Reason: "host hot: load 55/18 cores", Host: &HostReport{Load: 9, Cores: 18, Hot: false}}
	if err := s.WriteScaling(climbing, false); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Scaling(); !ok || got.Capacity != 3 {
		t.Fatalf("climbing fixed node: Scaling() = %+v, %v; want it left", got, ok)
	}

	quiet := Scaling{Capacity: 4, Min: 4, Max: 4, Host: &HostReport{Load: 9, Cores: 18, Hot: false}}
	if err := s.WriteScaling(quiet, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Scaling(); ok {
		t.Fatal("level and quiet fixed node: Scaling() still reports, want it removed")
	}

	if err := s.WriteScaling(hot, false); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteScaling(Scaling{}, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Scaling(); ok {
		t.Fatal("a bare fixed node with no host report: Scaling() still reports, want it removed")
	}
}
