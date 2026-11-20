package nodev1

import (
	"strings"
	"testing"
)

// With CPU pressure read, high load average alone is not hot: dl360 read
// load 186 on 96 cores with pressure 11% and the CPUs 91% idle.
func TestHostGatePressureJudgesInPlaceOfLoad(t *testing.T) {
	rt := &Runtime{Host: Host{LoadPerCoreMax: 1.5, CPUSomeAvg60Max: 25}}
	m := &fakeMachine{reading: MachineReading{Load1: 186, CPUs: 96, HasCPUPressure: true, CPUSomeAvg60: 11.6}}
	rt.Machine = m
	if got := rt.hostGate(); got != "" {
		t.Fatalf("hostGate = %q with pressure 11.6%% under 25%%, want open", got)
	}
	m.set(MachineReading{Load1: 20, CPUs: 96, HasCPUPressure: true, CPUSomeAvg60: 40})
	if got := rt.hostGate(); !strings.HasPrefix(got, "host hot: cpu pressure 40.0%") {
		t.Fatalf("hostGate = %q with pressure 40%%, want hot by cpu pressure", got)
	}
}

// Without PSI the load average still judges, as before.
func TestHostGateFallsBackToLoadWithoutPressure(t *testing.T) {
	rt := &Runtime{Host: Host{LoadPerCoreMax: 1.5, CPUSomeAvg60Max: 25}}
	rt.Machine = &fakeMachine{reading: MachineReading{Load1: 186, CPUs: 96}}
	if got := rt.hostGate(); !strings.HasPrefix(got, "host hot: load/core") {
		t.Fatalf("hostGate = %q without PSI, want hot by load", got)
	}
}

func TestGateKindIgnoresTheNumbers(t *testing.T) {
	a, b := "host hot: load/core 1.63 over 1.50", "host hot: load/core 2.04 over 1.50"
	if gateKind(a) != gateKind(b) {
		t.Fatalf("gateKind(%q) != gateKind(%q)", a, b)
	}
	if gateKind(a) == gateKind("host hot: cpu pressure 40.0% over 25.0%") {
		t.Fatal("load and pressure reasons share a kind")
	}
}
