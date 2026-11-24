package nodev1

import (
	"fmt"
	"math"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/scheduler"
)

// The node's holding report (goal
// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P5): `mesh node status` names every ask waiting for the scheduler's
// room and the reserve it waits on — "waiting for room" says which —
// beside each reserve's room and what it has free, and what each kind's
// leases used (P3).

// holding is the scheduler's waiting asks and its reserves, none
// without a scheduler.
func (r *Runtime) holding() ([]WaitingStatus, []ReserveStatus) {
	if r.Scheduler == nil {
		return nil, nil
	}
	var ws []WaitingStatus
	for _, w := range r.Scheduler.Waiting() {
		lease, lane, _ := strings.Cut(w.Owner, "/lane-")
		ws = append(ws, WaitingStatus{LeaseID: lease, Lane: lane, Reserve: w.Reserve, Person: w.Person, Room: w.Room(),
			CPUs: w.CPUs, MemGB: w.MemGB, Since: w.Since.UTC(), Why: w.Why})
	}
	var rs []ReserveStatus
	for _, name := range []string{scheduler.AssistantReserve, ValidationReserveName} {
		room := r.Scheduler.ReserveOf(name)
		if room.CPUs <= 0 && room.MemGB <= 0 {
			continue
		}
		free := r.Scheduler.ReserveFreeOf(name)
		p := r.Scheduler.PersonShareOf(name)
		rs = append(rs, ReserveStatus{Name: name, CPUs: room.CPUs, MemGB: room.MemGB, FreeCPUs: free.CPUs, FreeMemGB: free.MemGB,
			PersonCPUs: p.CPUs, PersonMem: p.MemGB})
	}
	return ws, rs
}

// holdingLines are the status's reserves, waiting asks and kinds'
// usage as `mesh node status` prints them.
func (s Status) holdingLines(now time.Time) []string {
	var out []string
	for _, r := range s.Reserves {
		line := fmt.Sprintf("Reserve %s: %g cpus, %s GB, %g cpus, %s GB free", r.Name, r.CPUs, num2(r.MemGB), num2f(r.FreeCPUs), num2(r.FreeMemGB))
		if r.PersonCPUs > 0 || r.PersonMem > 0 {
			line += fmt.Sprintf("; a person share of %g cpus, %s GB", r.PersonCPUs, num2(r.PersonMem))
		}
		out = append(out, line+".")
	}
	if len(s.Waiting) > 0 {
		out = append(out, fmt.Sprintf("Waiting for room: %d:", len(s.Waiting)))
		for _, w := range s.Waiting {
			who := w.LeaseID
			if w.Lane != "" {
				who += " lane " + w.Lane
			}
			out = append(out, fmt.Sprintf("  %s  %g cpus, %s GB  on %s  for %s: %s", who, num2f(w.CPUs), num2(w.MemGB), w.Room, now.Sub(w.Since).Round(time.Second), w.Why))
		}
	}
	if len(s.Usage) > 0 {
		out = append(out, "Use by kind (cpus, GB: median / p95 / highest per lease):")
		for _, u := range s.Usage {
			out = append(out, fmt.Sprintf("  %s  %d ended, %d running, %d at most at once; %d measured: cpus %g / %g / %g, GB %g / %g / %g",
				u.Kind, u.Ended, u.Running, u.MaxAtOnce, u.Measured, u.CPUP50, u.CPUP95, u.CPUMax, u.MemGBP50, u.MemGBP95, u.MemGBMax))
		}
	}
	return out
}

func num2f(f float64) float64 { return math.Round(f*100) / 100 }
