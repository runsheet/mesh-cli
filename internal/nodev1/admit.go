package nodev1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A validation is admitted for every lane it runs, all at once or not at
// all (goal a-validation-is-admitted-for-every-lane-it-runs; 18 Nov
// root-cause review: dl360 admitted a runsheet validation on its 4 ledger
// CPUs while its lanes needed 28, held some lanes best-effort and waited
// on the rest with the run's budget running, and ran far more than it
// could hold).
//
// Once a validate lease's checkout is in and its pod's services are up,
// admitLanes sizes the lease from its suites' lanes (validate.AdmitSizes:
// every lane but a sharded suite's further shards, which a run admits
// one by one as it starts them (holdLane, waiting for room) and gives
// back to the node as each ends; validate.LaneSizes:
// each lane's CPUs and memory, the room laneRoom gives it, which is the
// limit of the lane's steps, and beside it the room of the lane's own
// sidecars, a Postgres among them, which run under limits of their own:
// servicesRoom) and takes every lane's room of the
// node's scheduler in one all-or-nothing take (Scheduler.TakeAll). The
// first lane runs in the lease's own pod, so it asks only for what its
// room needs beyond the lease's own hold (askOf): a lease whose lanes
// sum to 28 CPUs holds 28 in all, not 28 plus its 4. A lease whose lanes
// do not fit now waits, holding none of them; one whose lanes could
// never fit the node's total is refused, naming why. Every admission is
// a line in node.log naming asked, free and held.
//
// A lane's room goes back to the node when the last run_suite step in
// that lane exits (holdLane's hold, released by the lane's end), not
// when the lease ends (2026-11-23: dl360 booked 86 of 88 CPUs while its
// load was 4 on 96 cores, every admitted unit and checks lane held idle
// for the rest of its lease while the session triaged and wrote its
// finish). A later step that needs the lane again — a re-run of its
// failing tests — takes the same ask afresh (admittedAsk), waiting for
// room as a shard's lane does. The lease's own hold (askOf) stays until
// the lease ends; a lane no run opened is given back then
// (releaseEagerLanes).
//
// The lanes a lease is running are in its hand-off (handoff.json's
// lanes): a node restarted mid-lease adopts their pods and holds their
// room again (adoptLanes) instead of leaving them orphaned.

// errNeverFits is a lease whose lanes could never fit the node.
var errNeverFits = errors.New("the node could never hold this validation's lanes")

// admittedLane is one lane's room as admission took it, and what it
// asked of the node's scheduler for it.
type admittedLane struct {
	name string
	room meshapi.Resources
	ask  scheduler.Ask
	l    *scheduler.Lease
}

// admitLanes admits b's validation for every lane its checkout's
// validate.yml runs (admit), its holds kept on p by lane name for
// holdLane to take; nothing on a node without a scheduler or pods, for
// a lease that is no validation, or a checkout without validate.yml.
func (r *Runtime) admitLanes(ctx context.Context, b *meshapi.Brief, workspace string, p *leasePod) error {
	if r.Scheduler == nil || r.Pods == nil || !validationKind(b) {
		return nil
	}
	cfg, err := validate.Load(workspace)
	if err != nil {
		return nil
	}
	start := time.Now()
	got, err := r.admit(ctx, b, validate.AdmitSizes(cfg.Steps), r.servicesRoom(p))
	if err != nil {
		return err
	}
	// the time the lease waited for its lanes' room is the node's, never
	// the repository's budget's: the budget's clock starts again from
	// admission (budgetBegan)
	p.admissionWait = time.Since(start)
	r.keepAdmitted(b, p, got)
	return nil
}

// keepAdmitted keeps admission's holds on p by lane name, for the run
// that opens each lane to take (holdLane), and what each asked, for a
// later step to ask again once the lane's end gave it back.
func (r *Runtime) keepAdmitted(b *meshapi.Brief, p *leasePod, got []admittedLane) {
	for _, a := range got {
		h := wrapHold(a.l, a.room)
		h.ended = r.laneEnded(b, a.name)
		p.setAdmitted(a.name, a.room, a.ask)
		p.setEagerLane(a.name, h)
	}
}

// admit takes every lane's room of sizes, for b's lease, of the node's
// scheduler at once, waiting as long as ctx allows; errNeverFits when
// they could never fit beside the lease's own hold.
//
// services is what each lane's own sidecars run on (servicesRoom), held
// beside its room for a lane whose suites run services
// (validate.LaneSize.Services): the first lane's are the lease's pod's.
func (r *Runtime) admit(ctx context.Context, b *meshapi.Brief, sizes []validate.LaneSize, services meshapi.Resources) ([]admittedLane, error) {
	if r.Scheduler == nil || len(sizes) == 0 {
		return nil, nil
	}
	lanes := make([]admittedLane, len(sizes))
	asks := make([]scheduler.Ask, len(sizes))
	var asked, rooms meshapi.Resources
	for i, s := range sizes {
		room := r.laneRoom(b, s.Name, s.Resources)
		svc := services
		if !s.Services {
			// a lane none of whose suites runs services starts no
			// sidecars (suiteLanes), so holds none of their room
			svc = meshapi.Resources{}
		}
		ask := laneAsk(b, s.Name, room, svc)
		if i == 0 {
			// the first lane runs in the lease's own pod, on the lease's
			// own hold: it asks for what its room and the pod's
			// services need beyond that
			ask.CPUs = math.Max(ask.CPUs-b.Resources.CPUs, 0)
			ask.MemGB = math.Max(ask.MemGB-b.Resources.MemGB, 0)
		}
		lanes[i] = admittedLane{name: s.Name, room: room, ask: ask}
		asks[i] = ask
		asked.CPUs += ask.CPUs
		asked.MemGB += ask.MemGB
		rooms.CPUs += room.CPUs
		rooms.MemGB += room.MemGB
	}
	total := r.Scheduler.Total()
	need := meshapi.Resources{CPUs: asked.CPUs + b.Resources.CPUs, MemGB: asked.MemGB + b.Resources.MemGB}
	if total.CPUs > 0 && need.CPUs > total.CPUs+1e-9 {
		r.logf("nodev1: lease %s: admission refused: its %d lanes need %g cpus with the lease's own, the node has %g in all", b.LeaseID, len(sizes), need.CPUs, total.CPUs)
		return nil, fmt.Errorf("%w: its %d lanes need %g cpus with the lease's own %g, the node has %g in all", errNeverFits, len(sizes), need.CPUs, b.Resources.CPUs, total.CPUs)
	}
	if total.MemGB > 0 && need.MemGB > total.MemGB+1e-9 {
		r.logf("nodev1: lease %s: admission refused: its %d lanes need %s GB with the lease's own, the node has %g GB in all", b.LeaseID, len(sizes), num2(need.MemGB), total.MemGB)
		return nil, fmt.Errorf("%w: its %d lanes need %s GB with the lease's own %g, the node has %g GB in all", errNeverFits, len(sizes), num2(need.MemGB), b.Resources.MemGB, total.MemGB)
	}
	free := r.Scheduler.Free()
	ls, err := r.Scheduler.TryTakeAll(asks)
	if err != nil {
		r.logf("nodev1: lease %s: admission waits: asked %g cpus, %s GB for %d lanes, free %g cpus, %s GB (%v)",
			b.LeaseID, asked.CPUs, num2(asked.MemGB), len(sizes), free.CPUs, num2(free.MemGB), err)
		start := time.Now()
		if ls, err = r.Scheduler.TakeAll(ctx, asks); err != nil {
			return nil, fmt.Errorf("waiting for the node's room for every lane: %w", err)
		}
		r.logf("nodev1: lease %s: admission waited %s", b.LeaseID, time.Since(start).Round(time.Second))
		free = scheduler.Capacity{CPUs: r.Scheduler.Free().CPUs + asked.CPUs, MemGB: r.Scheduler.Free().MemGB + asked.MemGB}
	}
	held := r.Scheduler.Total()
	after := r.Scheduler.Free()
	r.logf("nodev1: lease %s: admitted %d lanes: asked %g cpus, %s GB (lanes %g cpus, %s GB less the lease's own %g cpus, %g GB), free %g cpus, %s GB, held %g cpus, %s GB (the node holds %g of its %g cpus, %s of %s GB)",
		b.LeaseID, len(sizes), asked.CPUs, num2(asked.MemGB), rooms.CPUs, num2(rooms.MemGB), b.Resources.CPUs, b.Resources.MemGB,
		free.CPUs, num2(free.MemGB), need.CPUs, num2(need.MemGB), held.CPUs-after.CPUs, held.CPUs, num2(held.MemGB-after.MemGB), num2(held.MemGB))
	for i := range lanes {
		lanes[i].l = ls[i]
	}
	return lanes, nil
}

// laneEnded is the line a lane's hold says as its room goes back to
// the node: the lane, what it held and what the node has free after.
func (r *Runtime) laneEnded(b *meshapi.Brief, name string) func(cpus, memGB float64) {
	return func(cpus, memGB float64) {
		if r.Scheduler == nil {
			return
		}
		free := r.Scheduler.Free()
		r.logf("nodev1: lease %s: lane %s ended: its %g cpus, %s GB back to the node, free %g cpus, %s GB",
			b.LeaseID, name, cpus, num2(memGB), free.CPUs, num2(free.MemGB))
	}
}

// num2 is a float rounded to two places, for the log.
func num2(f float64) string { return fmt.Sprintf("%g", math.Round(f*100)/100) }

// handoffLanes is what a hand-off carries of a validation's lanes: the
// pods running beside the lease, each with the room it holds, so a node
// restarted mid-lease adopts them rather than leaves them orphaned.
type handoffLanes struct {
	Pods []handoffLane `json:"pods,omitempty"`
}

// handoffLane is one running lane's pod and the room it holds.
type handoffLane struct {
	Name  string  `json:"name"`
	Pod   string  `json:"pod"`
	CPUs  float64 `json:"cpus"`
	MemGB float64 `json:"memGb"`
}

// laneBook is the lanes each lease is running, by lease and pod, for its
// hand-off.
type laneBook struct {
	mu   sync.Mutex
	pods map[string]map[string]handoffLane
}

func (k *laneBook) add(lease string, l handoffLane) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pods == nil {
		k.pods = map[string]map[string]handoffLane{}
	}
	if k.pods[lease] == nil {
		k.pods[lease] = map[string]handoffLane{}
	}
	k.pods[lease][l.Pod] = l
}

func (k *laneBook) drop(lease, pod string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.pods[lease], pod)
	if len(k.pods[lease]) == 0 {
		delete(k.pods, lease)
	}
}

// of is lease's running lanes as its hand-off writes them, nil for none.
func (k *laneBook) of(lease string) *handoffLanes {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.pods[lease]) == 0 {
		return nil
	}
	out := &handoffLanes{}
	for _, l := range k.pods[lease] {
		out.Pods = append(out.Pods, l)
	}
	sort.Slice(out.Pods, func(i, j int) bool { return out.Pods[i].Pod < out.Pods[j].Pod })
	return out
}

// adoptLanes adopts a hand-off's running lanes for lease id: their pods
// kept, never removed while the lease runs; their room held of the
// node's scheduler again, all at once (waiting at most roomWait — the
// pods run whether or not the ledger has room, so a node that has none
// says so and holds nothing rather than refuse what already runs); and
// both given back at the lease's end, held on a (the pods removed then,
// with the node's sandbox).
func (r *Runtime) adoptLanes(ctx context.Context, a *actor.Actor, id string, st *handoffState) {
	if st == nil || st.Lanes == nil || len(st.Lanes.Pods) == 0 {
		return
	}
	var ls []*scheduler.Lease
	if r.Scheduler != nil {
		asks := make([]scheduler.Ask, 0, len(st.Lanes.Pods))
		for _, l := range st.Lanes.Pods {
			asks = append(asks, scheduler.Ask{Owner: id + "/lane-" + l.Name, CPUs: l.CPUs, MemGB: l.MemGB, Cache: scheduler.CacheShared, TTL: 3 * laneRenewEvery})
		}
		wctx, cancel := context.WithTimeout(ctx, roomWait)
		var err error
		ls, err = r.Scheduler.TakeAll(wctx, asks)
		cancel()
		if err != nil {
			r.logf("nodev1: lease %s: no room to hold its adopted lanes again (%v); they run on, held of nothing", id, err)
		}
	}
	var holds []*laneHold
	for i, l := range st.Lanes.Pods {
		r.lanes.add(id, l)
		if ls != nil {
			holds = append(holds, wrapHold(ls[i], meshapi.Resources{CPUs: l.CPUs, MemGB: l.MemGB}))
		}
		r.logf("nodev1: lease %s: adopted lane %s's pod %s (%g cpus, %s GB)", id, l.Name, l.Pod, l.CPUs, num2(l.MemGB))
	}
	pods := st.Lanes.Pods
	end := func() error {
		for _, h := range holds {
			h.free()
		}
		for _, l := range pods {
			r.lanes.drop(id, l.Pod)
			if r.Sandbox == nil {
				continue
			}
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := r.Sandbox.Remove(rctx, sandbox.Pod{Name: l.Pod}); err != nil {
				r.logf("nodev1: lease %s: removing its adopted lane pod %s: %v", id, l.Pod, err)
			}
			cancel()
		}
		return nil
	}
	if a != nil {
		a.Hold("the lease's adopted lanes", end)
	}
}
