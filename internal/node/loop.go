package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"projectmesh.io/mesh-cli/internal/crash"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Loop is trd §3: every Interval, heartbeat; list the orgs' ready items;
// claim up to capacity − running, most urgent goal first; hand each claim to Dispatch. The claim
// is the lock — it is taken here, before anything runs, in both modes.
type Loop struct {
	GW  Gateway
	Cfg *Config
	// Scopes are the orgs served: each is heartbeaten, listed and
	// claimed from in turn, under the one capacity.
	Scopes Scopes
	// Owned is set when the node registered once with its owner (ADR-015,
	// Registration and the loop): Tick then heartbeats once under its
	// one id and takes Scopes from each answer (TakeAnswer). Nil is the
	// legacy per-org node, heartbeating each scope.
	Owned *Owned
	// Version is what the node reports itself as: the release tag it was
	// built from, else its commit (version.Resolved); Commit is the commit
	// (version.Commit), on every heartbeat the gateway takes it on.
	Version  string
	Commit   string
	Harness  string
	Interval time.Duration
	Log      *log.Logger
	// Dispatch runs a claimed item and returns when it is over; Loop
	// counts it as running meanwhile.
	Dispatch func(ctx context.Context, item gateway.WorkItem)
	// Release is called after Dispatch returns (optional).
	Release func(item gateway.WorkItem)
	// Evaluate reviews an item this node finished (status evaluating,
	// node_id ours) and returns when the verdict is recorded — or not,
	// in which case the next tick offers it again. Optional; nil leaves
	// evaluation to someone else. It does not count against capacity.
	Evaluate func(ctx context.Context, item gateway.WorkItem)
	// ValidateBatch validates a merge batch candidate this node claimed
	// and uploads its attestation (Runner.ValidateCandidate; runsheet
	// ADR 048 decision 3): the loop claims, finishes and fails it
	// (validateBatches). It counts against capacity while it runs.
	// Optional; nil validates no batch.
	ValidateBatch func(ctx context.Context, batch gateway.MainValidationBatch) (validate.Attestation, error)
	// ValidateMain validates a merged item's merge commit this node
	// claimed for the main guard (ADR 054 decision 3) and attests it as
	// the item's, as ValidateBatch does a candidate; it counts against
	// capacity the same way, offered after every batch. Optional; nil
	// validates no merge commit.
	ValidateMain func(ctx context.Context, req gateway.MainValidationRequest) (validate.Attestation, error)
	// BatchCheckEvery is how often a running batch validation reads its
	// claim back (DefaultBatchCheckEvery when zero).
	BatchCheckEvery time.Duration
	// Plan compiles a goal in planning (in one of the served orgs) into
	// its graph and returns when the plan is handed in — or not, in
	// which case the next tick offers it again. Optional.
	Plan func(ctx context.Context, goal gateway.Goal, scope Scope)
	// PlanReleased says a goal's plan claim was released for a repeated
	// refusal and its row has not changed since, with the refusal's text
	// (Planner.Released; ADR-023, a refused plan is released, not
	// re-planned): such a goal is passed over and held, never claimed.
	// Optional; nil releases none.
	PlanReleased func(goal gateway.Goal) (reason string, ok bool)
	// Research reads one direction of a project and proposes the goals
	// it asks for (runsheet trd §6c). The claim is taken here, before it
	// runs; the platform refuses it outside the direction's hours,
	// inside its gap, or when another node holds it, so a refusal is the
	// ordinary answer. Optional; it does not count against capacity — it
	// competes for the machine, not for the work.
	Research func(ctx context.Context, direction gateway.Direction, project gateway.Project, scope Scope)
	// Rebase runs a session of kind rebase on an item whose rebase this
	// node has claimed (runsheet ADR 008) and returns when the rebase is
	// finished or failed. The claim is taken here, before it runs; another
	// node's claim is a refusal, the ordinary answer. Optional; like an
	// evaluation it does not count against capacity.
	Rebase func(ctx context.Context, item gateway.WorkItem)
	// Scaled is told each tick, after the scaler, the capacity in force
	// and its range, and whether node.yaml's capacity is a range at all
	// (optional: `mesh node start` leaves it in ~/.mesh for `mesh node
	// status`).
	Scaled func(s Scaling, ranged bool)
	// Held is told each tick the goals this node holds off claiming for
	// (NoteHeld, ClearHeld), as Scaled is told the capacity in force
	// (optional: `mesh node start` leaves it in ~/.mesh for `mesh node
	// status`, goal a-refused-plan-is-released-not-replanned).
	Held func([]HeldGoal)
	// Shares is told at the end of each pass every served org's floor,
	// held and ready (ADR-023, Reporting), the rows the holding report's
	// orgs carries; `mesh node start` leaves them in node.shares.
	Shares func([]HoldingOrg)
	// Standing is told each tick every scheduled account's clock
	// standing (StandingAccounts; optional: `mesh node start` leaves it
	// in node.standing, for `mesh node status`, goal
	// every-subscription-lands-full-at-its-reset, item 6).
	Standing func([]AccountStanding)
	// Quartermaster runs the node's own fitness checks from every tick,
	// when due (goal the-quartermaster-keeps-the-nodes-fit): each non-ok
	// finding and each reclaim logged, the findings told to Findings and
	// carried by every heartbeat after. Nil runs none.
	Quartermaster *Quartermaster
	// CacheTrim measures the shared cache and trims it under its cap at
	// the first tick and at most once an hour after (cachetrim.go); nil
	// trims nothing.
	CacheTrim *CacheTrim
	// Findings is told the quartermaster's findings each time its checks
	// run (optional: `mesh node start` leaves them in node.findings by
	// RunState.WriteFindings, for `mesh node status`).
	Findings      func([]Finding)
	quartermaster quartermasterState
	// Attestations counts the node's recorded and refused attestations
	// over the trailing hour (the validators' ValidationNotices.Counts);
	// the health line carries them. Nil counts none.
	Attestations *AttestationCounts
	// HealthEvery is how often the tick logs the health line
	// (HealthLine); 0 is DefaultHealthEvery.
	HealthEvery time.Duration
	lastHealth  time.Time
	// AfterTick runs at the end of every tick (optional).
	AfterTick func(ctx context.Context)
	// Reload is asked at the top of every tick for the capacity, the
	// limits thresholds and the orgs served as they stand now (node.yaml
	// and MyOrgs), so none needs a restart (optional; Reloader builds
	// it). A capacity of 0, nil limits or nil scopes keep what the loop
	// has; an error is logged, and what came back with it is still taken.
	Reload func(ctx context.Context) (Reloaded, error)
	// FirstBeat runs once, after the first heartbeat through, on the
	// orgs it answered: `mesh node start`'s credentials report
	// (ReportCoverage), which an owned node cannot make before then.
	FirstBeat func(ctx context.Context, scopes Scopes)
	firstBeat sync.Once
	// Limits holds the latest Claude plan standing of each account, fed
	// by every session (ObserveLimits); the heartbeat carries it and the
	// loop paces by it. Nil until the first tick makes one.
	Limits *Limits
	// Now is the clock pacing and scaling read (time.Now when nil).
	Now func() time.Time
	// Windows are the plan's windows the scaler paces a ranged capacity
	// by (adr-004); nil reads them from Limits, the highest utilization
	// across accounts per window.
	Windows WindowSource
	// Machine is the host the scaler caps a ranged capacity by; nil
	// imposes no cap (HostMachine is this one).
	Machine MachineProbe
	// Update updates the node's own binary to the version the platform
	// asks for (selfUpdate); nil never updates.
	Update Updater
	// Confirming is the version a pending update is confirming
	// (StartCheck.To when StartCheck.Pending); empty is none. While it
	// is unconfirmed every failed heartbeat is logged and recorded
	// (Updater.NoteConfirmError), and a window of ConfirmWithin from
	// Started with none through makes Run return ErrUnconfirmed.
	Confirming string
	// ConfirmWithin is node.yaml's update.confirm_within; zero is
	// selfupdate.DefaultConfirmWithin.
	ConfirmWithin time.Duration
	// Started is when the process started, where the confirm window
	// opens; zero is the first tick's now.
	Started time.Time
	// Image makes the node image of a version present on this machine
	// (SandboxConfig.EnsureImage, mesh-specs ADR 007): a self-update
	// calls it once the release is verified and before it drains, and a
	// failure is the update's error. nil ensures none.
	Image func(ctx context.Context, version string) (sandbox.Image, error)
	// Ledger is what each account has spent today and this month
	// (adr-005-capacity-across-accounts), fed by every session
	// (ObserveSpend); the accounts scaler and the spending order read
	// it. Nil until the first use makes one in memory.
	Ledger *Ledger
	// Known is the accounts the platform's claim-time read has issued
	// lately (the same store as Credentials.Known): the loop paces,
	// chooses and scales on them beside node.yaml's (goal
	// loop-schedules-on-issued-accounts). Nil: none.
	Known *KnownAccounts
	// NoCredential is the projects whose claim-time read issued no
	// credential for an adapter (the same store as
	// Credentials.NoCredential): their goals' items and plans are passed
	// over with its reason until its re-check is due. Nil: none held.
	NoCredential *NoCredentials
	// CodexRoom says whether a Codex account has headroom for an
	// evaluation of that priority (Evaluator.CodexHeadroom; nil: none
	// ever): an evaluation no Claude account has room for starts on it
	// (ADR 011).
	CodexRoom func(priority string) bool
	// LookPath finds a program on the node's PATH (exec.LookPath): every
	// tick probes with it the tool runtimes and agent adapters the node
	// has (tools.ProbeRuntimes, ProbeAdapters), and the heartbeat carries
	// them, so one installed while the node runs is reported without a
	// restart. Nil probes nothing and reports neither.
	LookPath func(file string) (string, error)
	// Declines are the items, goals and directions a session here
	// declined for a required tool whose runtime the node lacks (trd §3
	// Tools): neither claimed, evaluated, planned nor researched again
	// while the runtimes are unchanged. The build, evaluate, plan and
	// research sessions record into the same one; nil declines nothing.
	Declines *Declines
	// Sandbox is the heartbeat's `sandbox` — the mode, runtime and image
	// sessions run in (OpenSandbox, ADR 007); nil sends none.
	Sandbox *gateway.SandboxInfo
	// AdoptSession follows a session a node process before this one
	// started to its end and runs what its kind runs after a session
	// (AdoptSessions, mesh-cli ADR 009; AdoptWith is the node's). Nil
	// adopts nothing.
	AdoptSession func(ctx context.Context, s FoundSession)
	// ClaimsDir is where a plan or research claim is recorded the moment
	// it is granted, until its session's end is handled (ClaimsDir of the
	// work root; ReopenClaims, mesh-cli ADR 009 Claims). Empty records
	// nothing. A record is not a session: the drain does not wait on it.
	ClaimsDir string
	// WorkRoot is the node's work root (~/.mesh/work): the path whose
	// free space the disk floor reads when node.yaml's disk.path is left
	// out. Empty, with no disk.path, the floor is not judged.
	WorkRoot string
	// Statfs reads a path's free space for the disk floor; the real
	// statfs(2) when nil (the quartermaster's, DiskCheck's).
	Statfs StatfsFunc
	// Live counts the build sessions alive on the host, orphans
	// included, against the capacity and sweeps the orphans
	// (liveproc.go). Nil counts the tracked builds alone.
	Live *LiveProbe
	// RuntimeProbe asks the container runtime whether it can start a
	// session's pod (Sandbox.Probe): every RuntimeProbeEvery (the
	// const when zero) by Now, the first tick at once; a failure latches
	// the runtime broken and a pass clears it (ADR-034). Nil — no
	// sandbox — probes nothing.
	RuntimeProbe      func(ctx context.Context) error
	RuntimeProbeEvery time.Duration
	// RuntimeChanged is told the latched runtime state every tick, nil
	// while healthy (RunState.WriteRuntime); nil tells no one.
	RuntimeChanged func(*RuntimeState)

	mu sync.Mutex
	// runtime is the latched broken runtime (NoteRuntimeBroken), nil
	// while healthy; runtimeProbedAt when RuntimeProbe last ran.
	runtime         *RuntimeState
	runtimeProbedAt time.Time
	// answer is the platform's last NodeAnswer (TakeAnswer); restrict is
	// node.yaml's orgs restriction as last reloaded (restrictSet: read
	// at all, else Cfg.Orgs).
	answer      *gateway.NodeAnswer
	restrict    []string
	restrictSet bool
	// condition is the node's latest condition that is not a failure of
	// its own — a session its sandbox's limits killed — and conditionAt
	// when it was noted: the heartbeat's reason for ConditionHold.
	condition   string
	conditionAt time.Time
	running     map[string]bool
	// runningOn is the account each running item runs on, by item: the
	// slot-minutes the accounts scaler counts per account, and the slots
	// the spending order sees taken.
	runningOn   map[string]string
	evaluating  map[string]bool
	planning    map[string]bool
	researching map[string]bool
	rebasing    map[string]bool
	// validating is the main validations running, by mergeBatchId or,
	// for a merged item's, workItemId: each a session against the
	// capacity (Running).
	validating map[string]bool
	// sessionOrg is the org each build, plan and research session counts
	// toward, by subject (item, goal or direction id): heldByOrg's count
	// against the org's floor (ADR-023).
	sessionOrg map[string]string
	// adopted are the sessions taken over from the process before this
	// one that have not yet ended, by subject: the heartbeat's `adopted`,
	// its SessionID and Alive re-read from the found session's state on
	// every heartbeat (mesh-cli ADR-009 amendment, Loop.Adopted).
	// adoptedIDs are every subject adopted this run, so Adopt never runs
	// one of them again from the start.
	adopted    map[string]adoptedEntry
	adoptedIDs map[string]bool
	// resumed are the retry turns running now — a build session taken up
	// again after a failed check, or a fresh one started with the same
	// message — by a key NoteRetryTurn hands out: the heartbeat's
	// `resumed` (Loop.Resumed).
	resumed   map[uint64]resumedEntry
	resumedID uint64
	wg        sync.WaitGroup
	// draining is set by Drain: nothing new is started, and Run returns
	// once every session of any kind has ended.
	draining bool
	// wake tells Run that a session ended or a drain began.
	wake chan struct{}
	// capacity is the one in force: a fixed node's number, or where a
	// ranged node's scaler has stepped it to (0 until the first tick).
	capacity int
	// capRange is node.yaml's capacity as last read: Cfg.Capacity until
	// Reload says otherwise (zero).
	capRange Capacity
	// scaling is the scaler's state while capRange is a range.
	scaling scaleState
	// limits are the thresholds in force: Cfg.Limits until Reload says
	// otherwise (limitsSet).
	limits    LimitsConfig
	limitsSet bool
	// spending is node.yaml's accounts and budgets in force: Cfg's until
	// Reload says otherwise (spendingSet).
	spending    Spending
	spendingSet bool
	// context is node.yaml's context budget in force: Cfg's until Reload
	// says otherwise (contextSet).
	context    ContextConfig
	contextSet bool
	// claim is node.yaml's `claim:` block in force: Cfg's until Reload
	// says otherwise (claimSet).
	claim    ClaimConfig
	claimSet bool
	// takes is node.yaml's `takes:` block in force: Cfg's until Reload
	// says otherwise (takesSet); read through Takes() and takesKind.
	takes    Takes
	takesSet bool
	// host is node.yaml's `host:` block in force, defaults filled: Cfg's
	// until Reload says otherwise (hostSet).
	host    HostConfig
	hostSet bool
	// disk is node.yaml's `disk:` block in force, its floor defaulted:
	// Cfg's until Reload says otherwise (diskSet).
	disk    DiskConfig
	diskSet bool
	// diskState is the disk floor as last judged (diskFloor).
	diskState diskState
	// backoff is the host back-off over the plan figure (hostBackoff,
	// ADR-023 the node backs off a hot host).
	backoff hostState
	// shares is node.yaml's `shares:` weights in force (ADR-023): Cfg's
	// until Reload says otherwise (sharesSet); read through Weights().
	shares    map[string]int
	sharesSet bool
	// sharesUnknown is the set of org names the last tick's `shares:`
	// named that this node does not serve, so a name already logged as
	// "is not a served org" is not logged again while it keeps appearing
	// unchanged; a name that drops out and comes back logs again.
	sharesUnknown map[string]bool
	// answerShares is the owner answer's per-org shares (ADR-023): set
	// when the last answer carried a non-zero Shares on any scope, over
	// node.yaml's shares: in Weights() (answerSharesSet); an answer
	// whose scopes are all 0 clears it, falling back to node.yaml's
	// shares: (or nil) from then on.
	answerShares    map[string]int
	answerSharesSet bool
	// sharesBothPresent records whether the platform's shares and
	// node.yaml's shares: were both in force as of the last answer, so
	// the "node.yaml's shares: ignored" line logs once per change into
	// that state, not every tick it holds.
	sharesBothPresent bool
	// paced is the pacing mode last logged, so a crossing is logged once.
	paced paceMode
	// noEvaluationClaimSaid is set once the run has said the platform
	// takes no claimWorkItemEvaluation (evaluateHeld, ADR-025).
	noEvaluationClaimSaid bool
	// evaluationElsewhereSaid are the items this node built whose
	// evaluation another node claimed, each said once (evaluateHeld).
	evaluationElsewhereSaid map[string]bool
	// noMainValidationSaid is set once the run has said the platform
	// serves no main validation (validateBatches).
	noMainValidationSaid bool
	// noCredentialReadSaid is set once the run has said the platform
	// does not serve credentialIndexs (reconcileCredentials).
	noCredentialReadSaid bool
	// waiting counts, by kind, the work this tick left unclaimed because
	// no account had headroom for it: logged once at the tick's end.
	waiting map[string]int
	// unserved counts, by priority and kind, the work this tick left
	// unclaimed because the accounts with headroom serve other
	// priorities alone (AccountConfig.Serves): its own line in
	// logWaiting.
	unserved map[string]map[string]int
	// heldBackFor is the work this tick left unclaimed because the
	// holdback kept the one subscription that would take it for urgent
	// work (heldBack): its own line in logWaiting.
	heldBackFor *holdbackWait
	// unclaimed is the ready work this pass left for headroom, one entry
	// per goal and kind (passOver), folded into holding at the tick's
	// end. Tick alone touches it.
	unclaimed []Unclaimed
	// holding is the last pass's holding report (HoldingReport), which
	// the next tick's heartbeat carries; nil before the first pass.
	holding *gateway.Holding
	// readyByOrg is the ready items each org's listing returned on the
	// last claim pass, by org id, an org whose read failed left out
	// (noteReady; ADR-023, Reporting).
	readyByOrg map[string]int
	// held is the goals this node holds off claiming for, by GoalID and
	// Kind (NoteHeld, ClearHeld; goal
	// a-refused-plan-is-released-not-replanned).
	held []HeldGoal
	// ClaimHolds is the goals whose items are not asked for again after
	// a claim refused for budget, pause or approval (ADR-024).
	ClaimHolds ClaimHolds
	// tickListed is the ids of this tick's ready listing (claimReady).
	tickListed map[string]bool
	// tickGoals holds each org's goals as read this tick, so the claim,
	// the planner and the director share one GoalIndex read per org.
	tickGoals map[string]goalsRead
	// updating is set while a self-update runs; restart once its binary
	// is in place, when Run returns ErrRestartForUpdate. updateWG waits
	// for the update to end (tests).
	updating bool
	restart  bool
	updateWG sync.WaitGroup
	// letGo cancels every context Context derived: the sessions detach
	// and Run returns. exiting is set by Restart.
	letGo   []context.CancelFunc
	exiting bool
	// failed is the last update that failed and updateErr its reason, as
	// the heartbeat carries it.
	failed    updateFailure
	updateErr string
	// confirmed is set once the first heartbeat has gone through.
	confirmed bool
	// confirmErr is the last heartbeat error while confirming
	confirmErr error
	// desiredUnsupported is set once the platform turned out not to
	// know the desired version (logged once).
	desiredUnsupported bool
	// runtimes and adapters are what the last probe found; probed is
	// set by the first.
	runtimes []string
	adapters []string
	probed   bool
}

type goalsRead struct {
	goals []gateway.Goal
	err   error
}

// goals reads the org's goals once per tick.
func (l *Loop) goals(ctx context.Context, sc Scope) ([]gateway.Goal, error) {
	if r, ok := l.tickGoals[sc.ID]; ok {
		return r.goals, r.err
	}
	goals, err := l.GW.Goals(ctx, sc.ID)
	if l.tickGoals != nil {
		l.tickGoals[sc.ID] = goalsRead{goals, err}
	}
	return goals, err
}

// goalProject is the project an item's work is held by: its goal's,
// through the tick's one GoalIndex read, else the item's own.
func (l *Loop) goalProject(ctx context.Context, sc Scope, item gateway.WorkItem) string {
	if l.NoCredential == nil {
		return item.ProjectID
	}
	goals, _ := l.goals(ctx, sc)
	for _, g := range goals {
		if g.ID == item.GoalID && g.ProjectID != "" {
			return g.ProjectID
		}
	}
	return item.ProjectID
}

// readyItem is a ready item with the scope it was listed in and its
// goal's priority rank.
type readyItem struct {
	item  gateway.WorkItem
	scope Scope
	rank  int
	// floor is set on an item shareOrder took for its org's floor (the
	// floor, and held what the org held before it); 0 for the rest.
	floor, held int
}

// finishFirstAttempts bounds the finish-first preference (ADR-017): an
// item with this many failed evaluations or more no longer goes before
// fresh work. The platform counts attempts as failed evaluations
// (attempts+1 on each verdict that did not pass; a release leaves them
// alone) and fails the item at three (mesh-specs trd), so an item at two
// is on its last try. One under the platform's cap means a poisoned item
// takes the head of the queue at most twice, then falls back behind
// fresh work rather than blocking it.
const finishFirstAttempts = 2

// startedWork reports whether a ready item carries work already begun on
// a branch, which it does in any of these shapes:
//   - an open pull request (PRURL): the branch was pushed and the item is
//     back — sent back for rework, for a rebase the integrator could not
//     do itself, or for re-evaluation;
//   - an evaluation (Evaluation): a retry after a failed evaluation, the
//     builder reading why the last one failed;
//   - a rebase conflict (RebaseConflict) or a RebaseState other than
//     empty or none: the branch conflicts with its base.
func startedWork(it gateway.WorkItem) bool {
	return it.PRURL != "" || it.Evaluation != "" || it.RebaseConflict != "" ||
		(it.RebaseState != "" && it.RebaseState != "none")
}

// finishFirst reports whether a ready item goes before fresh work of its
// priority: started work under the attempt bound (finishFirstAttempts).
func finishFirst(it gateway.WorkItem) bool {
	return startedWork(it) && it.Attempts < finishFirstAttempts
}

// readyTime parses an item's ReadyAt. The platform serves a Time as RFC
// 3339 with however many fractional digits it has, which is not
// lexically ordered, so the times are parsed, never compared as strings.
// ok is false for an empty or unreadable one (an older platform).
func readyTime(it gateway.WorkItem) (t time.Time, ok bool) {
	if it.ReadyAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, it.ReadyAt)
	return t, err == nil
}

// claimOrder sorts ready items from every org into the order they are
// claimed (ADR-017):
//  1. by the goal's priority (urgent, high, normal, low);
//  2. at equal priority, finish-first items (finishFirst: started work
//     under the attempt bound) before the rest;
//  3. among the rest, fresh items before retries (a retry carries an
//     evaluation), so a retry at the bound keeps its old place behind
//     fresh work; within the finish-first class this step does not
//     apply;
//  4. then the older ReadyAt first; an item with no ReadyAt (an older
//     platform) or one that does not parse goes after every item that
//     has one, as if ready now. Comparing times only when both have one
//     and seq otherwise would not be transitive (older A seq 3 < newer
//     B seq 1 < timeless C seq 2 < A), and a sort needs a strict weak
//     order, so the key is (has a time, the time, seq);
//  5. then seq.
//
// The sort is stable, so full ties keep the order the orgs were listed in.
func claimOrder(items []readyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if fa, fb := finishFirst(a.item), finishFirst(b.item); fa != fb {
			return fa
		}
		if ra, rb := a.item.Evaluation != "", b.item.Evaluation != ""; !finishFirst(a.item) && ra != rb {
			return rb
		}
		ta, oka := readyTime(a.item)
		tb, okb := readyTime(b.item)
		if oka != okb {
			return oka
		}
		if oka && !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return a.item.Seq < b.item.Seq
	})
}

// goalRanks joins the org's goals to their priority rank through the
// tick's one GoalIndex read. A goal it does not know — or every goal,
// when the read fails — ranks as normal; what says what the org's work
// is then done as normal ("claimed", "evaluated").
func (l *Loop) goalRanks(ctx context.Context, sc Scope, what string) func(goalID string) int {
	priority := map[string]string{}
	goals, err := l.goals(ctx, sc)
	if err != nil {
		l.logf("list goals of %s (its items %s as normal): %v", sc.Name(), what, err)
	}
	for _, g := range goals {
		priority[g.ID] = g.Priority
	}
	return func(goalID string) int { return gateway.PriorityRank(priority[goalID]) }
}

// evaluateOrder sorts the items held for evaluation from every org by
// their goal's priority, then seq.
func evaluateOrder(items []readyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.item.Seq < b.item.Seq
	})
}

// rebaseOrder sorts the items whose rebase is requested from every org
// by their goal's priority, then seq: the order they are claimed in,
// as the rest of the node's work is.
func rebaseOrder(items []readyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.item.Seq < b.item.Seq
	})
}

// planningGoal is a goal in planning with the scope it was listed in.
type planningGoal struct {
	goal  gateway.Goal
	scope Scope
}

// planOrder sorts the goals in planning from every org by priority. The
// Goal carries no age, so the sort is stable: ties keep the order the
// orgs and their goals were listed in.
func planOrder(goals []planningGoal) {
	sort.SliceStable(goals, func(i, j int) bool {
		return gateway.PriorityRank(goals[i].goal.Priority) < gateway.PriorityRank(goals[j].goal.Priority)
	})
}

// claimReady lists every served org's ready items, joins each to its
// goal's priority (one GoalIndex read per org, none for an org with
// nothing ready) and claims them in shareOrder — an org below its floor
// first, then claimOrder across orgs (ADR-023) — until free runs out, each
// on the account chosen for its priority; items of a goal ranked past
// maxRank are left, and normal and low items take no more than
// normalFree of the free slots. An item no account has headroom for is
// not claimed: it stays ready for a later tick. With
// claim.unmerged_per_repo set, a fresh item of a repo with that many
// items in review is not claimed either; started work there is
// (ADR-017, Per-repo limit on unmerged work).
func (l *Loop) claimReady(ctx context.Context, free, normalFree, maxRank int) {
	if !l.takesKind(harness.KindBuild) {
		return
	}
	var ready []readyItem
	limit := l.UnmergedPerRepo()
	listed := make([][]gateway.WorkItem, len(l.Scopes))
	failed := make([]bool, len(l.Scopes))
	anyReady, complete := false, true
	for i, sc := range l.Scopes {
		items, err := l.GW.ReadyWorkItems(ctx, sc.ID)
		if err != nil {
			l.logf("list ready items of %s: %v", sc.Name(), err)
			complete = false
			failed[i] = true
			continue
		}
		listed[i] = items
		anyReady = anyReady || len(items) > 0
	}
	l.noteReady(l.Scopes, listed, failed)
	// A repo's unmerged work is counted over every served org — one with
	// nothing ready, or whose ready read failed, may still hold review
	// items of a repo another org has fresh work in — and only on a tick
	// with something to claim.
	unmerged := map[string]int{}
	var unread []string // orgs whose review listing failed this tick
	if limit > 0 && anyReady {
		for _, sc := range l.Scopes {
			if !l.countUnmerged(ctx, sc, unmerged) {
				unread = append(unread, sc.Name())
			}
		}
	}
	for i, sc := range l.Scopes {
		items := listed[i]
		if len(items) == 0 {
			continue
		}
		rank := l.goalRanks(ctx, sc, "claimed")
		for _, it := range items {
			if r := rank(it.GoalID); r <= maxRank {
				ready = append(ready, readyItem{item: it, scope: sc, rank: r})
			} else {
				// only a ranged node at its min ranks past low
				l.passOver(it.GoalID, "item", priorityName(r), fmt.Sprintf("the node is at its min of %d slot(s) for headroom and builds only urgent and high goals there", l.Capacity()), time.Time{})
			}
		}
	}
	// every org's floor first, then priority across orgs (ADR-023); the
	// org's count goes up with each claim so one tick does not overshoot
	// a floor the next item is taken for
	orgHeld := l.heldByOrg()
	ready = shareOrder(ready, Floors(l.Capacity(), l.Scopes, l.Weights()), orgHeld)
	l.noteUnlisted(listed, complete)
	held := map[string]bool{} // repos whose hold this tick has logged
	capped := l.takesCap(harness.KindBuild)
	defer l.logCapped(capped)
	for _, r := range ready {
		item, sc := r.item, r.scope
		l.mu.Lock()
		already := l.running[item.ID]
		l.mu.Unlock()
		if already {
			continue
		}
		if l.capFull(capped, item.GoalID, "item", priorityName(r.rank)) {
			continue
		}
		if free <= 0 {
			break
		}
		if l.claimHeld(ctx, sc, item, r.rank) {
			continue // refused for budget, pause or approval: the goal has not changed
		}
		if runtimes, _ := l.Runtimes(); l.Declines.Holds(item.ID, runtimes, time.Now()) {
			l.passOverInfrastructure(item.GoalID, "item", item.ID)
			continue // a required tool needs a runtime still missing here
		}
		if why, until, ok := l.NoCredential.Holds(l.goalProject(ctx, sc, item), string(harness.KindBuild), l.adapter()); ok {
			l.passOver(item.GoalID, "item", priorityName(r.rank), why, until)
			l.NoCredential.Say(item.GoalID, why, until)
			continue
		}
		fresh := !startedWork(item)
		if limit > 0 && fresh && len(unread) > 0 {
			// the repo's count is unknown: it may be at the limit already
			why := fmt.Sprintf("repo %s's unmerged pull requests could not be counted (%s's review items unread) under node.yaml's claim.unmerged_per_repo %d", item.RepoID, strings.Join(unread, ", "), limit)
			l.passOver(item.GoalID, "item", priorityName(r.rank), why, time.Time{})
			if !held[item.RepoID] {
				held[item.RepoID] = true
				l.logf("claim: %s; its fresh items wait, its started work is still claimed", why)
			}
			continue
		}
		if n := unmerged[item.RepoID]; limit > 0 && fresh && n >= limit {
			why := fmt.Sprintf("repo %s has %d unmerged pull requests, at node.yaml's claim.unmerged_per_repo %d", item.RepoID, n, limit)
			l.passOver(item.GoalID, "item", priorityName(r.rank), why, time.Time{})
			if !held[item.RepoID] {
				held[item.RepoID] = true
				l.logf("claim: %s; its fresh items wait, its started work is still claimed", why)
			}
			continue
		}
		pressing := r.rank <= gateway.PriorityRank("high")
		if !pressing && normalFree <= 0 {
			l.passOver(item.GoalID, "item", priorityName(r.rank), "the free slots are the urgent-only reserve at the end of an api key's day", time.Time{})
			continue
		}
		actx, ok := l.accountFor(ctx, priorityName(r.rank), true, "item")
		if !ok {
			l.passOver(item.GoalID, "item", priorityName(r.rank), "", time.Time{})
			continue
		}
		if err := l.GW.ClaimWorkItem(actorContext(ctx, l.Cfg, gateway.RoleBuilder), item.ID, sc.NodeID, l.Harness); err != nil {
			if kind := claimHoldKind(err.Error()); kind != "" && gateway.IsRefusal(err) {
				l.holdClaim(ctx, sc, item, kind, err.Error())
			} else if gateway.IsRefusal(err) {
				l.logf("%s item %d %q: not ours (%v)", sc.Name(), item.Seq, item.Title, err)
			} else {
				l.logf("%s claim item %d: %v", sc.Name(), item.Seq, err)
			}
			continue
		}
		free--
		if !pressing {
			normalFree--
		}
		normalFree = min(normalFree, free)
		if fresh {
			// its pull request will count against the repo: one tick
			// does not overshoot the limit
			unmerged[item.RepoID]++
		}
		org := sc.ID
		if len(l.Scopes) > 1 && r.floor > 0 && orgHeld[org] < r.floor {
			l.logf("%s: claimed item %d %q (below its floor of %d, holding %d)", sc.Name(), item.Seq, item.Title, r.floor, orgHeld[org])
		} else {
			l.logf("%s: claimed item %d %q", sc.Name(), item.Seq, item.Title)
		}
		orgHeld[org]++
		capped.started()
		l.start(actx, item, org)
	}
}

// countUnmerged adds the org's items in review — a pull request open and
// waiting to merge — to unmerged by repo: one OrgWorkItems read per org
// and tick, made for every served org while claim.unmerged_per_repo is
// set and something is ready. A failed
// read is logged and answers false: any repo may have review items in
// that org and be at the limit already, so claimReady holds every fresh
// item this tick and still claims started work.
func (l *Loop) countUnmerged(ctx context.Context, sc Scope, unmerged map[string]int) bool {
	rows, err := l.GW.OrgWorkItems(ctx, sc.ID, "review")
	if err != nil {
		l.logf("list review items of %s (its fresh items held this tick): %v", sc.Name(), err)
		return false
	}
	for _, it := range rows {
		unmerged[it.RepoID]++
	}
	return true
}

func (l *Loop) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log.Printf(format, args...)
	}
}

// Running is the number of items this node is building and of merge
// batch candidates it is validating: what counts against capacity.
// Evaluations, plans, research and rebases do not.
// A retry turn noted by NoteRetryTurn is building too: counted once by
// its item, so a turn inside the build that holds its item adds nothing
// and one whose item no build holds adds one.
func (l *Loop) Running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buildingLocked() + len(l.validating)
}

// buildingLocked is Running with l.mu held: the items in running and
// those of the noted retry turns, each item once.
func (l *Loop) buildingLocked() int {
	n := len(l.running)
	var extra map[string]bool
	for _, e := range l.resumed {
		item := e.session.ItemID
		if l.running[item] || extra[item] {
			continue
		}
		if extra == nil {
			extra = map[string]bool{}
		}
		extra[item] = true
		n++
	}
	return n
}

// runningOf is the number of sessions of kind this node runs: builds
// (running), evaluations, rebases, plans and research, each from the
// loop's own map.
func (l *Loop) runningOf(kind harness.Kind) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch kind {
	case harness.KindBuild:
		return l.buildingLocked()
	case harness.KindEvaluate:
		// a batch validation runs under the evaluate cap, as it is
		// claimed on the evaluation cadence
		return len(l.evaluating) + len(l.validating)
	case harness.KindRebase:
		return len(l.rebasing)
	case harness.KindPlan:
		return len(l.planning)
	case harness.KindResearch:
		return len(l.researching)
	}
	return 0
}

// kindRoom is how many more sessions of kind node.yaml's takes: cap
// leaves room for — the cap less what runs — and whether kind is
// capped at all (ADR-025, the cap).
func (l *Loop) kindRoom(kind harness.Kind) (int, bool) {
	limit, ok := l.Takes().Cap(kind)
	if !ok {
		return 0, false
	}
	return max(limit-l.runningOf(kind), 0), true
}

// kindCap is a claim path's takes: cap for one tick: the room counted
// once, at the path's start, and lowered by each start, so a session
// that ends within the tick never makes room for one more until the
// next tick and one tick never overshoots.
type kindCap struct {
	kind    harness.Kind
	capped  bool
	limit   int
	room    int
	waiting int
}

// takesCap reads kind's cap and room for this tick.
func (l *Loop) takesCap(kind harness.Kind) *kindCap {
	room, ok := l.kindRoom(kind)
	limit, _ := l.Takes().Cap(kind)
	return &kindCap{kind: kind, capped: ok, limit: limit, room: room}
}

// capFull says whether the cap holds the next session of the kind back:
// it is passed over with the cap's reason, said once per goal and kind
// on the holding report.
func (l *Loop) capFull(c *kindCap, goalID, unclaimedKind, priority string) bool {
	if !c.capped || c.room > 0 {
		return false
	}
	c.waiting++
	l.passOver(goalID, unclaimedKind, priority, c.reason(), time.Time{})
	return true
}

// started counts one start of the kind against the tick's room.
func (c *kindCap) started() {
	if c.capped {
		c.room--
	}
}

// reason is the cap's holding reason.
func (c *kindCap) reason() string {
	return fmt.Sprintf("at node.yaml's takes cap of %d %s(s)", c.limit, kindNoun(c.kind))
}

// logCapped says once per kind and tick what the cap held back.
func (l *Loop) logCapped(c *kindCap) {
	if c.waiting > 0 {
		l.logf("takes: %s at its cap of %d; %d waiting", c.kind, c.limit, c.waiting)
	}
}

// buildCapped says whether node.yaml's takes: cap on build is what
// leaves no room: claimReady still runs to name the items it holds.
func (l *Loop) buildCapped() bool {
	room, ok := l.kindRoom(harness.KindBuild)
	return ok && room == 0
}

// kindNoun names one session of kind in a holding reason.
func kindNoun(kind harness.Kind) string {
	if kind == harness.KindEvaluate {
		return "evaluation"
	}
	return string(kind)
}

// RunningIDs lists the items in flight.
func (l *Loop) RunningIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.running))
	for id := range l.running {
		ids = append(ids, id)
	}
	return ids
}

// Run ticks until ctx is done, then waits for the items in flight and
// returns ctx's error: a session that outlives the node (Job.Outlive)
// detaches at once, anything else is killed by the same ctx. Once
// Drain has been called it returns nil instead, as soon as no session of
// any kind is left — items, evaluations, plans, research and rebases alike, since
// only the items count against capacity and a node is rarely without
// the others. After Restart it returns ErrRestart at once, its sessions
// left running for the next process to adopt; after a self-update has
// put the new binary in place, ErrRestartForUpdate, the same way; and
// when a pending update's confirm window elapses with no heartbeat
// through, an *UnconfirmedError (ErrUnconfirmed).
func (l *Loop) Run(ctx context.Context) error {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	l.mu.Unlock()
	ctx = l.Context(ctx)
	interval := l.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	wake := l.wakeup()
	l.Tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if l.restartRequested() {
			l.detach()
			l.wg.Wait()
			return ErrRestartForUpdate
		}
		if err := l.unconfirmedNow(); err != nil {
			// the platform could not be reached inside the window: the
			// service starts the process again, its sessions left to adopt
			l.detach()
			l.wg.Wait()
			return err
		}
		if l.exitingNow() {
			l.wg.Wait()
			l.updateWG.Wait()
			return ErrRestart
		}
		if l.drained() && !l.updatingNow() {
			l.wg.Wait() // the last session's Release
			l.logf("drained: every session has ended")
			return nil
		}
		select {
		case <-ctx.Done():
			l.wg.Wait()
			l.updateWG.Wait()
			if l.restartRequested() {
				return ErrRestartForUpdate
			}
			if l.exitingNow() {
				return ErrRestart
			}
			return ctx.Err()
		case <-t.C:
			l.Tick(ctx)
		case <-wake:
		}
	}
}

// ErrRestart is what Run returns after Restart: `mesh node start` execs
// its binary again (or its service starts it), and the new process
// adopts the sessions this one left running (adr-009).
var ErrRestart = errors.New("restart")

// Context derives from parent a context Restart cancels. Run derives
// its own; the node's other work on the loop's behalf — the sessions it
// adopts before the first tick — runs in one too, so a restart lets go
// of every session at once.
func (l *Loop) Context(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	l.mu.Lock()
	l.letGo = append(l.letGo, cancel)
	gone := l.exiting || l.restart
	l.mu.Unlock()
	if gone {
		cancel()
	}
	return ctx
}

// Restart makes Run return ErrRestart at once — no drain, no wait for
// any session: every context Context derived is cancelled, so each
// session that outlives the node (Job.Outlive) detaches and runs on in
// its own unit, for the next process to adopt (`mesh node upgrade`,
// SIGHUP) — a sandboxed one, Claude's or Codex's, in its pod. A Codex
// session on the host still ends with the node.
func (l *Loop) Restart() {
	l.mu.Lock()
	l.exiting = true
	n := l.sessions()
	l.mu.Unlock()
	l.logf("restarting: %d session(s) left running for the next process to adopt", n)
	l.detach()
	l.notify()
}

// detach cancels every context Context derived.
func (l *Loop) detach() {
	l.mu.Lock()
	cancels := append([]context.CancelFunc(nil), l.letGo...)
	l.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// exitingNow reports whether Restart has been called.
func (l *Loop) exitingNow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exiting
}

// Drain stops the node taking anything new — items, evaluations, plans,
// research, rebases — and lets Run return once what is running has ended.
// Calling it again does nothing.
func (l *Loop) Drain() {
	l.mu.Lock()
	if l.draining {
		l.mu.Unlock()
		return
	}
	l.draining = true
	n := l.sessions()
	l.mu.Unlock()
	l.logf("draining: nothing new is claimed; %d session(s) to finish", n)
	l.notify()
}

// Draining reports whether Drain has been called.
func (l *Loop) Draining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining
}

// drained is draining with no session of any kind left.
func (l *Loop) drained() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining && l.sessions() == 0
}

// sessions counts every session in flight, of any kind; l.mu is held.
func (l *Loop) sessions() int {
	return l.buildingLocked() + len(l.evaluating) + len(l.planning) + len(l.researching) + len(l.rebasing) + len(l.validating)
}

func (l *Loop) wakeup() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wake == nil {
		l.wake = make(chan struct{}, 1)
	}
	return l.wake
}

// notify wakes Run without blocking: a session ended, or a drain began.
func (l *Loop) notify() {
	select {
	case l.wakeup() <- struct{}{}:
	default:
	}
}

// ConditionHold is how long a noted condition stays the heartbeat's
// reason.
const ConditionHold = time.Hour

// NoteCondition makes reason the heartbeat's until ConditionHold passes
// or another is noted: a session killed by its sandbox's limits is the
// node's condition, shown on the Nodes page while the node stays online
// (trd §1 Node, `sandbox`).
func (l *Loop) NoteCondition(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.condition, l.conditionAt = reason, time.Now()
}

// Condition is the heartbeat's reason now, "" when none holds.
func (l *Loop) Condition() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.condition == "" || time.Since(l.conditionAt) >= ConditionHold {
		return ""
	}
	return l.condition
}

// Tick is one round: heartbeat, self-update, list, claim, dispatch. A draining node
// only heartbeats — with draining set — (and runs AfterTick): it claims no
// item, plan, research or rebase and starts no evaluation — an item it holds for evaluation
// waits in evaluating for the node that comes up after it. Every
// heartbeat carries the plan limits last read, and every start runs on
// the account ChooseAccount picks by them: an account paced near its
// five-hour window's limit takes only urgent and high work, one stopped
// near either window's end takes nothing, and work no account has room
// for is not claimed — like a drain for new starts, but the process runs
// on. The node's own mode (nodePace) is logged at each crossing and, when
// paced, halves the capacity.
func (l *Loop) Tick(ctx context.Context) {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	l.mu.Unlock()
	l.tickGoals = map[string]goalsRead{}
	l.reload(ctx)
	l.probe()
	// the runtime's own word before the heartbeat, so a broken one is
	// on this tick's beat and claims nothing below (ADR-034)
	l.probeRuntime(ctx)
	l.scale()
	l.hostBackoff()
	l.diskFloor()
	runtimes, adapters := l.Runtimes()
	// the platform's word on standing first, so a withdrawn account is
	// gone from this heartbeat's limits and holding
	l.reconcileCredentials(ctx)
	l.dropWithdrawn()
	// the heartbeat goes out before this tick's pass, so its holding is
	// the last pass's decision (nil before the first; a draining node,
	// which makes no pass, keeps sending its last one unchanged)
	extras := gateway.HeartbeatExtras{Draining: l.Draining(), UpdateError: l.UpdateError(), Limits: l.limitsStore().Heartbeat(l.Known.Accounts(l.now()), l.now(), l.Thresholds()), Runtimes: runtimes, Adapters: adapters, Sandbox: l.Sandbox, Reason: l.Condition(), Adopted: l.Adopted(), Resumed: l.Resumed(), Holding: l.Holding(), Commit: l.Commit, Findings: l.heartbeatFindings(ctx)}
	rt := l.RuntimeBroken()
	l.runtimeHeartbeat(ctx, &extras, rt)
	scaling, ranged := l.Scaling()
	extras.Scaling = l.heartbeatScaling(scaling, ranged)
	if l.Scaled != nil {
		l.Scaled(scaling, ranged)
	}
	if l.Held != nil {
		l.Held(l.HeldGoals())
	}
	if l.Standing != nil {
		l.Standing(StandingAccounts(l.now(), l.scheduled(), l.limitsStore().Readings(), l.Thresholds()))
	}
	l.health()
	beat := false
	if owned := l.owned(); owned != nil {
		// one node, one heartbeat: its answer is the orgs served now and
		// who owns it, a hand-over taken with no restart
		answer, err := l.GW.HeartbeatNode(ctx, owned.NodeID, l.Version, l.Running(), extras)
		if err != nil {
			if l.confirmPending() {
				l.confirmFailed(owned.Owner.String(), err)
			} else {
				l.logf("heartbeat %s: %v", owned.Owner.String(), err)
			}
		} else {
			beat = true
			l.TakeAnswer(answer)
		}
	} else {
		for _, sc := range l.Scopes {
			if _, err := l.GW.HeartbeatNode(ctx, sc.NodeID, l.Version, l.Running(), extras); err != nil {
				if l.confirmPending() {
					l.confirmFailed(sc.Name(), err)
				} else {
					l.logf("heartbeat %s: %v", sc.Name(), err)
				}
			} else {
				beat = true
			}
		}
	}
	if beat {
		l.confirmUpdate()
		if l.FirstBeat != nil {
			l.firstBeat.Do(func() { l.FirstBeat(ctx, l.CurrentScopes()) })
		}
	}
	l.selfUpdate(ctx)
	// the quartermaster after the heartbeat, so its checks never hold
	// one up: what they find rides the next
	l.runQuartermaster(ctx)
	l.CacheTrim.RunIfDue(ctx, l.now())
	p := l.pacing()
	if !l.Draining() {
		// every start below first picks its account (accountFor): one
		// with no account to run on is left for a later tick
		l.waiting = map[string]int{}
		l.unserved = map[string]map[string]int{}
		l.heldBackFor = nil
		l.unclaimed = nil
		// a node low on disk claims nothing of any kind; what runs runs on
		low := l.diskLow()
		if rt != nil {
			// a broken runtime claims nothing of any kind — what runs
			// runs on, as in a drain — and says why on every ready goal
			l.passOverRuntime(ctx, rt)
		} else {
			if !low {
				l.evaluateHeld(ctx)
				l.rebaseRequested(ctx)
				l.planRequested(ctx)
				l.researchDue(ctx)
			}
			// the slots taken are the tracked builds or, when more run on
			// the host, the live agent sessions, orphans included (LiveProbe)
			inUse := l.inUse(ctx)
			// a merge batch candidate's validation takes a slot first: it
			// lands work already built and attested
			if !low {
				inUse += l.validateBatches(ctx, l.Capacity()-inUse)
			}
			free := l.Capacity() - inUse
			if p.mode == pacePaced {
				free = pacedFree(free, l.Capacity(), inUse)
			}
			// a ranged node at its min builds only urgent and high goals
			claimRank := gateway.PriorityRank("low")
			if l.atMin() {
				claimRank = min(claimRank, gateway.PriorityRank("high"))
			}
			// the urgent-only slots (the reserve at the end of an api key's
			// day) are not for normal and low work
			normalFree := min(free, l.Capacity()-l.urgentOnly()-inUse)
			// node.yaml's takes: cap on build bounds the free slots inside
			// the capacity; claimReady passes over what it holds back
			if room, ok := l.kindRoom(harness.KindBuild); ok && room < free {
				free = room
				normalFree = min(normalFree, free)
			}
			// a hot host claims nothing new; what runs runs on
			if (free > 0 || l.buildCapped()) && !l.hostHot() && !low {
				l.claimReady(ctx, free, normalFree, claimRank)
			}
			l.logWaiting()
		}
		// with no accounts configured this is the operator's own login,
		// one implicit account: normal, and unknown until a reading
		accounts, readings := l.accounts()
		h := HoldingReport(l.now(), accounts, readings, l.Thresholds(), l.Capacity(), p, l.unclaimed, l.shareRows(), l.Takes().Kinds())
		if rt != nil {
			h = runtimeHolding(h, rt)
		}
		l.mu.Lock()
		l.holding = &h
		l.mu.Unlock()
		if l.Shares != nil {
			l.Shares(h.Orgs)
		}
	}
	if l.AfterTick != nil {
		l.AfterTick(ctx)
	}
}

// drainPoll is how often DrainAndWait looks for the last session to end.
var drainPoll = 50 * time.Millisecond

// errResumed is DrainAndWait's answer when Resume ends the drain first.
var errResumed = errors.New("drain resumed")

// DrainAndWait drains the loop and returns once no session of any kind
// is left, with ctx.Err() when ctx is done first, or with an error when
// Resume ends the drain first. It is the drain a self-update waits on
// before it swaps the binary; Run still returns on its own meanwhile.
func (l *Loop) DrainAndWait(ctx context.Context) error {
	l.Drain()
	t := time.NewTicker(drainPoll)
	defer t.Stop()
	for {
		if l.drained() {
			return nil
		}
		if !l.Draining() {
			return errResumed
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Resume undoes a drain that has not yet ended Run: the next tick claims
// again, and the heartbeat stops reporting draining.
func (l *Loop) Resume() {
	l.mu.Lock()
	was := l.draining
	l.draining = false
	l.mu.Unlock()
	if was {
		l.logf("resumed: claiming again")
	}
}

// Capacity is the number of items this node runs at once: the capacity
// in force, as last read or scaled, less what a hot host holds back
// (hostBackoff). Before the first tick a ranged node has its min; once
// scaling, a machine short of memory may hold it below (even 0).
func (l *Loop) Capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inForceLocked(l.planLocked())
}

// Thresholds are the limits thresholds in force, as last read.
func (l *Loop) Thresholds() LimitsConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limitsSet {
		return l.limits.Thresholds()
	}
	return l.Cfg.Limits.Thresholds()
}

// limitsStore is Limits, made on first use.
func (l *Loop) limitsStore() *Limits {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Limits == nil {
		l.Limits = &Limits{}
	}
	return l.Limits
}

// DefaultHealthEvery is how often the health line is logged when
// HealthEvery is left at 0.
const DefaultHealthEvery = 15 * time.Minute

// HealthLine is the node's periodic health line: the builds running
// against the capacity in force and the hour's attestation outcomes,
// `health: <running> of <capacity> running, attestations recorded <n>
// refused <m> (1h)`.
func (l *Loop) HealthLine() string {
	return fmt.Sprintf("health: %d of %d running, %s", l.Running(), l.Capacity(), l.Attestations.HealthLine())
}

// health logs HealthLine on the first tick and every HealthEvery after.
func (l *Loop) health() {
	every := l.HealthEvery
	if every <= 0 {
		every = DefaultHealthEvery
	}
	now := l.now()
	if !l.lastHealth.IsZero() && now.Sub(l.lastHealth) < every {
		return
	}
	l.lastHealth = now
	l.logf("%s", l.HealthLine())
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// pacing decides this tick's mode from the readings and logs a
// crossing into another mode — once, not every tick.
func (l *Loop) pacing() pacing {
	accounts, readings := l.accounts()
	p := nodePace(l.now(), accounts, readings, l.Thresholds())
	l.mu.Lock()
	was := l.paced
	l.paced = p.mode
	n := l.sessions()
	l.mu.Unlock()
	if p.mode == was {
		return p
	}
	switch p.mode {
	case pacePaced:
		l.logf("limits: paced until %s: %s — only urgent and high goals are claimed and planned, at most %d item(s) at once, and no research", p.resumeAt.UTC().Format(time.RFC3339), p.why, max(1, l.Capacity()/2))
	case paceStopped:
		also := ""
		for _, a := range accounts {
			if a.Kind == AccountAPI {
				also = " on a subscription (an api account takes urgent and high work, and work that waits)"
				break
			}
		}
		l.logf("limits: stopped until %s: %s — nothing new is started%s; %d session(s) to finish", p.resumeAt.UTC().Format(time.RFC3339), p.why, also, n)
	default:
		l.logf("limits: back to normal (was %s)", was)
	}
	return p
}

// accounts are the accounts the node chooses among and their latest
// readings by label: node.yaml's and the issued ones it knows
// (scheduled). With neither it is the one implicit account, the
// operator's own login, judged by its strictest reading.
func (l *Loop) accounts() ([]AccountConfig, map[string]harness.RateLimit) {
	readings := l.limitsStore().Readings()
	if accounts := l.scheduled(); len(accounts) > 0 {
		return accounts, readings
	}
	own := []AccountConfig{{Label: DefaultAccount, Kind: AccountSubscription}}
	if r, ok := strictest(l.now(), readings, l.Thresholds()); ok {
		return own, map[string]harness.RateLimit{DefaultAccount: r}
	}
	return own, nil
}

// CodexAccounts are the issued Codex accounts Known holds and their
// latest readings by label (Limits.CodexReadings), for judging a Codex
// session's account apart from the Claude ones.
func (l *Loop) CodexAccounts() ([]AccountConfig, map[string]harness.RateLimit) {
	var codex []AccountConfig
	for _, a := range l.Known.Accounts(l.now()) {
		if a.IsCodex() {
			codex = append(codex, a)
		}
	}
	return codex, l.limitsStore().CodexReadings()
}

// CodexCandidates are the Codex accounts an evaluation may run on
// first (ADR 011): the issued ones Known holds, and only those — the
// operator's own Codex login is never one (ADR 011's amendment
// 2026-10-02).
func (l *Loop) CodexCandidates() []AccountConfig {
	out, _ := l.CodexAccounts()
	return out
}

// scheduled are the accounts the loop schedules on: the issued Claude
// accounts Known holds, and only those — node.yaml names none
// (adr-005's amendments 2026-10). A Codex account is not among them: its
// sessions are not the capacity's, and its readings are not the Claude
// accounts' (CodexAccounts).
func (l *Loop) scheduled() []AccountConfig {
	return slices.DeleteFunc(l.Known.Accounts(l.now()), AccountConfig.IsCodex)
}

// accountFor picks the account a session of that priority runs on
// (ChooseAccount) and returns ctx carrying it — with no issued account
// known yet, nothing: the claim-time read decides. ok is
// false when no account has headroom: the session is not started, and
// a what that is not empty is counted for the tick's one line
// (logWaiting).
func (l *Loop) accountFor(ctx context.Context, priority string, workWaits bool, what string) (context.Context, bool) {
	return l.withAccount(ctx, what, priority, func(accounts []AccountConfig, readings map[string]harness.RateLimit) (AccountConfig, bool) {
		return ChooseAccount(l.now(), accounts, readings, l.Thresholds(), priority, workWaits)
	})
}

// accountToFinish picks the account for an evaluation or a rebase —
// work already built, which pacing never held back: by the spending
// order for its priority first, and failing that a paced subscription
// as if it were pressing. A stopped account still takes nothing, and an
// api account is still only what its priority or exhausted
// subscriptions allow.
func (l *Loop) accountToFinish(ctx context.Context, priority, what string) (context.Context, bool) {
	return l.withAccount(ctx, what, priority, func(accounts []AccountConfig, readings map[string]harness.RateLimit) (AccountConfig, bool) {
		if a, ok := ChooseAccount(l.now(), accounts, readings, l.Thresholds(), priority, true); ok {
			return a, true
		}
		var subs []AccountConfig
		for _, a := range accounts {
			if a.Kind != AccountAPI && a.Serves(priority) {
				subs = append(subs, a)
			}
		}
		return ChooseAccount(l.now(), subs, readings, l.Thresholds(), "high", true)
	})
}

// withAccount chooses by choose among the accounts the spending order
// leaves a session of that priority (spendable): an api key past its
// cap never, the reserve at the end of its day only for urgent and high
// work, and for an item (what "item") only an account with a slot free.
func (l *Loop) withAccount(ctx context.Context, what, priority string, choose func([]AccountConfig, map[string]harness.RateLimit) (AccountConfig, bool)) (context.Context, bool) {
	accounts, readings := l.accounts()
	pressing, item := priority == "urgent" || priority == "high", what == "item"
	a, ok := choose(l.spendable(accounts, pressing, item), readings)
	if !ok {
		switch {
		case what == "":
		case l.waiting != nil && l.noteHeldBack(accounts, readings, priority, what, func(h AccountConfig) bool {
			_, ok := choose(l.spendable([]AccountConfig{h}, pressing, item), readings)
			return ok
		}):
		case l.unserved != nil && unservedOnly(accounts, func(all []AccountConfig) bool {
			_, ok := choose(l.spendable(all, pressing, item), readings)
			return ok
		}):
			if l.unserved[priority] == nil {
				l.unserved[priority] = map[string]int{}
			}
			l.unserved[priority][what]++
		case l.waiting != nil:
			l.waiting[what]++
		}
		return ctx, false
	}
	ctx = withChooser(withPriority(ctx, priority), func(issued []AccountConfig) (AccountConfig, bool) {
		// Credentials.Open hands in one adapter's accounts: a Codex
		// session's are judged by the Codex readings
		readings := l.limitsStore().Readings()
		if len(issued) > 0 && issued[0].IsCodex() {
			readings = l.limitsStore().CodexReadings()
		}
		return choose(l.spendable(issued, pressing, item), readings)
	})
	if len(l.scheduled()) == 0 {
		return ctx, true
	}
	// an issued account rides ctx for its label alone (runsOn, the
	// heartbeat's slots): Credentials.Open opens what the read issues
	return WithAccount(ctx, a), true
}

// holdbackWait is what the holdback left unclaimed this tick: the
// account it kept, the earliest reset among the others (when the hold
// can lift), and the work by kind.
type holdbackWait struct {
	label  string
	until  time.Time
	counts map[string]int
}

// noteHeldBack counts what toward the holdback's line when the holdback
// is why no account was chosen: a subscription serving priority is
// held back (heldBack) and, listed alone, fits would take the work.
func (l *Loop) noteHeldBack(accounts []AccountConfig, readings map[string]harness.RateLimit, priority, what string, fits func(AccountConfig) bool) bool {
	if priority == "urgent" {
		return false
	}
	now, cfg := l.now(), l.Thresholds()
	var subs []AccountConfig
	for _, a := range accounts {
		if a.Kind == AccountSubscription && a.Serves(priority) {
			subs = append(subs, a)
		}
	}
	h, ok := heldBack(now, subs, readings, cfg)
	if !ok || !fits(h) {
		return false
	}
	if l.heldBackFor == nil || l.heldBackFor.label != h.Label {
		w := &holdbackWait{label: h.Label, counts: map[string]int{}}
		for _, a := range subs {
			if a.Label == h.Label {
				continue
			}
			r := readings[a.Label]
			at := pace(now, r, cfg.For(a)).resumeAt
			if at.IsZero() {
				at = latestReset(r)
			}
			if !at.IsZero() && (w.until.IsZero() || at.Before(w.until)) {
				w.until = at
			}
		}
		l.heldBackFor = w
	}
	l.heldBackFor.counts[what]++
	return true
}

// unservedOnly says the accounts' priorities alone left the work
// without one: with every account serving every priority, fits would
// find one.
func unservedOnly(accounts []AccountConfig, fits func([]AccountConfig) bool) bool {
	all := make([]AccountConfig, len(accounts))
	reserved := false
	for i, a := range accounts {
		reserved = reserved || len(a.Priorities) > 0
		a.Priorities = nil
		all[i] = a
	}
	return reserved && fits(all)
}

// passOver records that this pass left a goal's ready work (kind "item",
// "plan", "evaluation", "rebase", or "research" under a direction's id)
// for headroom: one entry per goal and kind, counting its
// items, with the reason heldReason gives the first — held, or with
// none every account's standing — and each later held not yet in it
// appended, so two repos at their limit in one goal are both named.
// until is this hold's own (zero for a hold with none, such as the
// unmerged-per-repo limit or the urgent-only reserve); when several
// holds fold into one entry, its Until is the earliest of their
// non-zero untils.
func (l *Loop) passOver(goalID, kind, priority, held string, until time.Time) {
	for i := range l.unclaimed {
		if u := &l.unclaimed[i]; u.GoalID == goalID && u.Kind == kind {
			u.Count++
			if held != "" && !strings.Contains(u.Reason, held) {
				u.Reason += "; " + held
			}
			if !until.IsZero() && (u.Until.IsZero() || until.Before(u.Until)) {
				u.Until = until
			}
			return
		}
	}
	accounts, readings := l.accounts()
	rows := holdingAccounts(l.now(), accounts, readings, l.Thresholds(), l.Capacity())
	l.unclaimed = append(l.unclaimed, Unclaimed{GoalID: goalID, Kind: kind, Count: 1, Reason: heldReason(priority, held, rows), Until: until})
}

// passOverInfrastructure records, on the holding report, an item this
// node holds off after an infrastructure release (ReleaseOnInfrastructure):
// kind and goal as passOver's, the reason the hold's own `infrastructure:
// <error>` — not prefixed with the priority, so the console's summary
// names it — and Until the hold's end — and answers whether it did. Any
// other decline is left unreported, as before.
func (l *Loop) passOverInfrastructure(goalID, kind, itemID string) bool {
	runtimes, _ := l.Runtimes()
	why, until, ok := l.Declines.HeldUntil(itemID, runtimes, time.Now())
	if !ok || !strings.HasPrefix(why, InfrastructurePrefix) {
		return false
	}
	for i := range l.unclaimed {
		if u := &l.unclaimed[i]; u.GoalID == goalID && u.Kind == kind && strings.HasPrefix(u.Reason, InfrastructurePrefix) {
			u.Count++
			if !strings.Contains(u.Reason, why) {
				u.Reason += "; " + why
			}
			if until.Before(u.Until) {
				u.Until = until
			}
			return true
		}
	}
	l.unclaimed = append(l.unclaimed, Unclaimed{GoalID: goalID, Kind: kind, Count: 1, Reason: why, Until: until})
	return true
}

// CredentialReadRefused is said once per run when the platform will
// not serve the loop's credential read: withdrawals then come only from
// the claim-time read (Credentials.Open's Reconcile).
const CredentialReadRefused = "credentials: the platform does not serve credentialIndexs to this node; withdrawals come from the claim-time read"

// reconcileCredentials reads every served org's credentialIndexs rows
// and withdraws the known labels the platform says are disabled,
// deleted, not allowed on this node, or no longer there
// (KnownAccounts.ReconcileRows; adr-005's amendment 2026-10-03, item 2):
// each one's reading is forgotten, and dropWithdrawn takes it out of the
// last pass's holding, so the heartbeat about to go out carries
// neither. A
// refused or unknown read is said once per run and leaves the
// claim-time read as the only source; any other failed read is logged
// and nothing is withdrawn on this tick.
func (l *Loop) reconcileCredentials(ctx context.Context) {
	if l.Known == nil || len(l.Known.Accounts(l.now())) == 0 {
		return
	}
	scopes := l.CurrentScopes()
	if len(scopes) == 0 {
		return
	}
	nodeID := scopes[0].NodeID
	if owned := l.owned(); owned != nil {
		nodeID = owned.NodeID
	}
	var rows []gateway.CredentialRow
	for _, sc := range scopes {
		got, err := l.GW.Credentials(ctx, sc.ID)
		if gateway.IsRefusal(err) {
			if !l.noCredentialReadSaid {
				l.noCredentialReadSaid = true
				l.logf("%s", CredentialReadRefused)
			}
			return
		}
		if err != nil {
			l.logf("credentials: org %s: %v; nothing withdrawn this tick", sc.Name(), err)
			return
		}
		// a legacy node has an id per org: a row allowed on this org's
		// id is allowed on the node
		for _, r := range got {
			if sc.NodeID != nodeID && slices.Contains(r.AllowedNodeIDs, sc.NodeID) {
				r.AllowedNodeIDs = nil
			}
			rows = append(rows, r)
		}
	}
	withdrawn := l.Known.ReconcileRows(rows, nodeID, l.now())
	if len(withdrawn) == 0 {
		return
	}
	for _, w := range withdrawn {
		l.limitsStore().Forget(limitsAccountKey(w.Adapter, w.Label))
		l.logf("accounts: %s withdrawn (platform: %s)", w.Label, w.Reason)
	}
}

// dropWithdrawn takes every account Known now holds withdrawn — by the
// credential read or by a claim-time read since the last pass — out of
// the last pass's holding, which the heartbeat about to go out carries.
func (l *Loop) dropWithdrawn() {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.holding
	if h == nil || !slices.ContainsFunc(h.Accounts, func(a gateway.HoldingAccount) bool { return l.Known.Withdrawn(a.Account) }) {
		return
	}
	kept := *h
	kept.Accounts = slices.DeleteFunc(slices.Clone(h.Accounts), func(a gateway.HoldingAccount) bool { return l.Known.Withdrawn(a.Account) })
	l.holding = &kept
}

// Holding is the last pass's holding report, which the next heartbeat
// carries; nil before the first pass.
func (l *Loop) Holding() *gateway.Holding {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holding
}

// logWaiting says once per tick what waits for an account with
// headroom, not once per item.
func (l *Loop) logWaiting() {
	judged, _ := l.accounts()
	for _, p := range []string{"urgent", "high", "normal", "low"} {
		if len(l.unserved[p]) > 0 {
			l.logf("accounts: no account serves %s work (%s); %s left unclaimed until one does", p, servesLabels(judged), countKinds(l.unserved[p]))
		}
	}
	if w := l.heldBackFor; w != nil {
		until := "one of them resets"
		if !w.until.IsZero() {
			until = w.until.UTC().Format(time.RFC3339)
		}
		l.logf("accounts: %s held back for urgent work (the others within 10%% of stop); %s left unclaimed until %s", w.label, countKinds(w.counts), until)
	}
	if len(l.waiting) == 0 {
		return
	}
	l.logf("accounts: no account has headroom for %s; judged %s; left unclaimed until one has", countKinds(l.waiting), labels(judged))
}

// countKinds is "2 item(s), 1 plan(s)", by kind in order.
func countKinds(counts map[string]int) string {
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s(s)", counts[k], k))
	}
	return strings.Join(parts, ", ")
}

// servesLabels is each account with what it serves: "max20 serves
// urgent and high, shhemail serves all".
func servesLabels(accounts []AccountConfig) string {
	if len(accounts) == 0 {
		return "(none)"
	}
	out := make([]string, len(accounts))
	for i, a := range accounts {
		what := "all"
		if len(a.Priorities) > 0 {
			what = andList(a.Priorities)
		}
		out[i] = a.Label + " serves " + what
	}
	return strings.Join(out, ", ")
}

// priorityName is the priority a goal rank stands for.
func priorityName(rank int) string {
	return [...]string{"urgent", "high", "normal", "low"}[min(max(rank, 0), 3)]
}

// CurrentScopes are the orgs served, as last read — for the sessions
// that need an item's org login after the loop has started.
func (l *Loop) CurrentScopes() Scopes {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Scopes
}

// reload takes the capacity, the limits, the accounts, the session
// budgets and the orgs from Reload, logging a change.
// Tick is the only writer of Scopes, so its own reads need no lock.
func (l *Loop) reload(ctx context.Context) {
	if l.Reload == nil {
		return
	}
	got, err := l.Reload(ctx)
	scopes := got.Scopes
	if err != nil {
		// one physical line per tick: a cause's own newlines (a YAML
		// error lists its lines) are folded in with the rest.
		l.logf("reload config: %s", oneLine(err.Error()))
	}
	if got.Capacity.Max > 0 {
		l.setRange(got.Capacity)
	}
	if got.Limits != nil {
		was, now := l.Thresholds(), got.Limits.Thresholds()
		if now != was {
			var parts []string
			if was.PaceFiveHour != now.PaceFiveHour {
				parts = append(parts, fmt.Sprintf("pace five-hour %s → %s", pct(was.PaceFiveHour), pct(now.PaceFiveHour)))
			}
			if was.Stop != now.Stop {
				parts = append(parts, fmt.Sprintf("stop %s → %s", pct(was.Stop), pct(now.Stop)))
			}
			if was.Reserve != now.Reserve {
				parts = append(parts, fmt.Sprintf("reserve %s → %s", pct(was.Reserve), pct(now.Reserve)))
			}
			l.logf("limits: %s", strings.Join(parts, ", "))
		}
		l.mu.Lock()
		l.limits, l.limitsSet = *got.Limits, true
		l.mu.Unlock()
	}
	if got.Spending != nil {
		for _, line := range budgetChanges(Spending{Budgets: l.sessionBudgets(), ProjectBudgets: l.projectBudgets()}, *got.Spending) {
			l.logf("%s", line)
		}
		l.mu.Lock()
		l.spending, l.spendingSet = *got.Spending, true
		l.mu.Unlock()
	}
	if got.Context != nil {
		was, now := l.ContextBudget(), got.Context.Effective()
		if now != was {
			l.logf("context: bytes_max %d → %d, doc_bytes_max %d → %d", was.BytesMax, now.BytesMax, was.DocBytesMax, now.DocBytesMax)
		}
		l.mu.Lock()
		l.context, l.contextSet = now, true
		l.mu.Unlock()
	}
	if got.Claim != nil {
		if was, now := l.UnmergedPerRepo(), got.Claim.UnmergedPerRepo; now != was {
			l.logf("claim: unmerged_per_repo %d → %d", was, now)
		}
		l.mu.Lock()
		l.claim, l.claimSet = *got.Claim, true
		l.mu.Unlock()
	}
	if got.Takes != nil {
		if was, now := l.Takes(), *got.Takes; was.String() != now.String() {
			l.logf("takes: %s → %s", was, now)
		}
		l.mu.Lock()
		l.takes, l.takesSet = *got.Takes, true
		l.mu.Unlock()
	}
	if got.Host != nil {
		was, now := l.HostThresholds(), got.Host.Effective()
		if now.LoadPerCoreMax != was.LoadPerCoreMax {
			l.logf("host: load_per_core_max %s → %s", hostNum(was.LoadPerCoreMax), hostNum(now.LoadPerCoreMax))
		}
		if now.PackageTempMaxC != was.PackageTempMaxC {
			l.logf("host: package_temp_max_c %s → %s", hostNum(was.PackageTempMaxC), hostNum(now.PackageTempMaxC))
		}
		if now.OverloadPerCore != was.OverloadPerCore {
			l.logf("host: overload_per_core %s → %s", hostNum(was.OverloadPerCore), hostNum(now.OverloadPerCore))
		}
		if now.CPUSomeAvg60Max != was.CPUSomeAvg60Max {
			l.logf("host: cpu_some_avg60_max %s → %s", hostNum(was.CPUSomeAvg60Max), hostNum(now.CPUSomeAvg60Max))
		}
		if now.QuietFraction != was.QuietFraction {
			l.logf("host: quiet_fraction %s → %s", hostNum(was.QuietFraction), hostNum(now.QuietFraction))
		}
		if now.OverloadCPUSomeAvg60 != was.OverloadCPUSomeAvg60 {
			l.logf("host: overload_cpu_some_avg60 %s → %s", hostNum(was.OverloadCPUSomeAvg60), hostNum(now.OverloadCPUSomeAvg60))
		}
		if now.StarveAfter != was.StarveAfter {
			l.logf("host: starve_after %s → %s", was.StarveAfter, now.StarveAfter)
		}
		l.mu.Lock()
		l.host, l.hostSet = now, true
		l.mu.Unlock()
	}
	if got.Disk != nil {
		was, now := l.DiskThresholds(), got.Disk.Effective()
		if now.FreeMinGB != was.FreeMinGB {
			l.logf("disk: free_min_gb %s → %s", hostNum(was.FreeMinGB), hostNum(now.FreeMinGB))
		}
		if now.Path != was.Path {
			l.logf("disk: path %s → %s", orNone(was.Path), orNone(now.Path))
		}
		l.mu.Lock()
		l.disk, l.diskSet = now, true
		l.mu.Unlock()
	}
	if got.SharesRead {
		served := map[string]bool{}
		for _, sc := range l.Scopes {
			served[sc.Name()] = true
		}
		var unknown []string
		filtered := map[string]int{}
		for org, w := range got.Shares {
			if !served[org] {
				unknown = append(unknown, org)
				continue
			}
			filtered[org] = w
		}
		if len(filtered) == 0 {
			filtered = nil
		}
		sort.Strings(unknown)
		l.mu.Lock()
		alreadyLogged := l.sharesUnknown
		l.mu.Unlock()
		nowUnknown := map[string]bool{}
		for _, org := range unknown {
			nowUnknown[org] = true
			if !alreadyLogged[org] {
				l.logf("shares: %s is not a served org", org)
			}
		}
		was := map[string]int{}
		for org, w := range l.nodeYAMLWeights() {
			if served[org] {
				was[org] = w
			}
		}
		if len(was) == 0 {
			was = nil
		}
		if now := filtered; !sharesEqual(was, now) {
			l.logf("shares %s → %s", sharesString(was), sharesString(now))
		}
		l.mu.Lock()
		l.shares, l.sharesSet = filtered, true
		l.sharesUnknown = nowUnknown
		l.mu.Unlock()
		l.noteSharesBoth()
	}
	if got.OrgsRead {
		l.mu.Lock()
		changed := !l.restrictSet || !slices.Equal(l.restrict, got.Orgs)
		l.restrict, l.restrictSet = got.Orgs, true
		answer := l.answer
		l.mu.Unlock()
		// an owned node's restriction applies to the last answer at once
		if changed && answer != nil {
			l.TakeAnswer(answer)
		}
	}
	if scopes != nil && !sameOrgs(scopes, l.Scopes) {
		l.logf("orgs %s → %s", strings.Join(l.Scopes.Names(), ", "), strings.Join(scopes.Names(), ", "))
		l.mu.Lock()
		l.Scopes = scopes
		l.mu.Unlock()
	}
}

// oneLine folds s onto one line, each line break (and the indent after
// it) becoming "; ".
func oneLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(slices.DeleteFunc(lines, func(l string) bool { return l == "" }), "; ")
}

// probe re-reads which tool runtimes and agent adapters the node has
// (trd §1 Node, `runtimes` and `adapters`), logging a change.
func (l *Loop) probe() {
	if l.LookPath == nil {
		return
	}
	runtimes, adapters := tools.ProbeRuntimes(l.LookPath), tools.ProbeAdapters(l.LookPath)
	l.mu.Lock()
	wasR, wasA := l.runtimes, l.adapters
	l.runtimes, l.adapters, l.probed = runtimes, adapters, true
	l.mu.Unlock()
	if !slices.Equal(wasR, runtimes) || !slices.Equal(wasA, adapters) {
		l.logf("tools: runtimes [%s], adapters [%s]", strings.Join(runtimes, " "), strings.Join(adapters, " "))
	}
}

// Runtimes returns the tool runtimes (npx, uvx, docker) and the agent
// adapters (claude-code, codex) the last tick found on the node — or,
// before the first tick (an item adopted at start), a probe now.
func (l *Loop) Runtimes() (runtimes, adapters []string) {
	l.mu.Lock()
	probed := l.probed
	l.mu.Unlock()
	if !probed {
		l.probe()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.runtimes), slices.Clone(l.adapters)
}

// ToolRuntimes is Runtimes' runtimes alone: what a session's local tools
// may start on (Runner.Runtimes, Evaluator.Runtimes).
func (l *Loop) ToolRuntimes() []string {
	runtimes, _ := l.Runtimes()
	return runtimes
}

func sameOrgs(a, b Scopes) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// evaluateHeld starts an evaluation of every item in evaluating in the
// served orgs that no evaluation here is running on, most urgent goal
// first across the orgs (evaluateOrder; the goals are read only for an
// org with one). Every item, the ones this node built included, is
// claimed first with ClaimWorkItemEvaluation — the one lock across
// nodes, so an evaluate-only node evaluates what others built and a
// builder never evaluates beside one that took its item (ADR-025's
// amendment 2026-10-14; the platform converges for the same node). A
// refusal is the ordinary answer, another node has it, and is not
// logged as an error; on an item this node built it is said once per
// item. A platform without the claim leaves this node to its own items,
// with no claim, said once per run. An adopted evaluation is already in
// l.evaluating and is not claimed again. The verdict moves the item on; an evaluation that ends without
// one leaves it here for the next tick.
func (l *Loop) evaluateHeld(ctx context.Context) {
	if l.Evaluate == nil || !l.takesKind(harness.KindEvaluate) {
		return
	}
	var held []readyItem
	claims := 0 // -1 no claim taken, 1 taken, 0 not yet asked
	for _, sc := range l.Scopes {
		rows, err := l.GW.OrgWorkItems(ctx, sc.ID, "evaluating")
		if err != nil {
			l.logf("list evaluating items of %s: %v", sc.Name(), err)
			continue
		}
		var rank func(string) int
		for _, it := range rows {
			if claims == 0 {
				claims = -1
				if l.GW.TakesEvaluationClaim(ctx) {
					claims = 1
				}
			}
			if it.NodeID != sc.NodeID {
				if claims < 0 {
					l.mu.Lock()
					said := l.noEvaluationClaimSaid
					l.noEvaluationClaimSaid = true
					l.mu.Unlock()
					if !said {
						l.logf("the platform takes no evaluation claim; evaluating only what this node holds")
					}
					continue
				}
			}
			if rank == nil {
				rank = l.goalRanks(ctx, sc, "evaluated")
			}
			held = append(held, readyItem{item: it, scope: sc, rank: rank(it.GoalID)})
		}
	}
	evaluateOrder(held)
	capped := l.takesCap(harness.KindEvaluate)
	defer l.logCapped(capped)
	for _, h := range held {
		item := h.item
		l.mu.Lock()
		if l.evaluating == nil {
			l.evaluating = map[string]bool{}
		}
		if l.evaluating[item.ID] {
			l.mu.Unlock()
			continue
		}
		// a build of the item still running on this node has not pushed
		// yet (its session may have handed the item in through the
		// platform's finish tool before the node's push): an evaluation
		// started now would rewrite the shared checkout's credentials
		// with its read token and the push would be refused "Write
		// access to repository not granted" (dl360, 2026-10-10). The
		// item waits here for the next tick, until the build has ended.
		if l.running[item.ID] {
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		if l.capFull(capped, item.GoalID, "evaluation", priorityName(h.rank)) {
			continue
		}
		if runtimes, _ := l.Runtimes(); l.Declines.Holds(item.ID, runtimes, time.Now()) {
			l.passOverInfrastructure(item.GoalID, "evaluation", item.ID)
			continue
		}
		if why, until, ok := l.evaluationHeld(l.goalProject(ctx, h.scope, item), priorityName(h.rank)); ok {
			// its project's claim-time read refused an evaluation: it
			// waits in evaluating until the hold's until, said once for
			// the goal
			l.passOver(item.GoalID, "evaluation", priorityName(h.rank), why, until)
			l.NoCredential.Say(item.GoalID, why, until)
			continue
		}
		// no Claude account with room: a Codex account with room for it
		// still evaluates it (ADR 011)
		actx, ok := l.accountToFinish(ctx, priorityName(h.rank), "")
		if !ok && l.CodexRoom != nil && l.CodexRoom(priorityName(h.rank)) {
			actx, ok = withPriority(ctx, priorityName(h.rank)), true
		}
		if !ok {
			if l.waiting != nil {
				l.waiting["evaluation"]++
			}
			continue
		}
		l.mu.Lock()
		l.evaluating[item.ID] = true
		l.mu.Unlock()
		if sc := h.scope; claims > 0 {
			// the claim is the lock across nodes, the builder's own
			// evaluation included: refused, another node has it
			built := item.NodeID == sc.NodeID
			if err := l.GW.ClaimWorkItemEvaluation(ctx, item.ID, sc.NodeID); err != nil {
				l.mu.Lock()
				delete(l.evaluating, item.ID)
				l.mu.Unlock()
				if !gateway.IsRefusal(err) {
					l.logf("%s: claim the evaluation of item %d: %v", sc.Name(), item.Seq, err)
				} else if built {
					l.sayEvaluationElsewhere(ctx, item, err)
				}
				continue
			}
			// read again: the claim made the item this node's, and the
			// evaluator (and an adoption after a restart) judges by the
			// item's nodeId
			if fresh, err := l.GW.WorkItem(ctx, item.ID); err == nil && fresh != nil {
				item = *fresh
			} else {
				item.NodeID = sc.NodeID
			}
			l.logf("%s: claimed the evaluation of item %d %q", sc.Name(), item.Seq, item.Title)
		}
		capped.started()
		l.logf("evaluating item %d %q", item.Seq, item.Title)
		l.wg.Add(1)
		go func(ctx context.Context, item gateway.WorkItem) {
			defer crash.Guard()
			defer l.wg.Done()
			defer func() {
				l.mu.Lock()
				delete(l.evaluating, item.ID)
				l.mu.Unlock()
				l.notify()
			}()
			l.Evaluate(ctx, item)
		}(actx, item)
	}
}

// sayEvaluationElsewhere says once per item that the evaluation of an
// item this node built was claimed by another node: `item <goal>/<seq>:
// evaluation is node <x>'s; not evaluated here`. The holder is the one
// the refusal names, else the item's nodeId read again when it is not
// this node's; not an error.
func (l *Loop) sayEvaluationElsewhere(ctx context.Context, item gateway.WorkItem, refusal error) {
	l.mu.Lock()
	if l.evaluationElsewhereSaid == nil {
		l.evaluationElsewhereSaid = map[string]bool{}
	}
	said := l.evaluationElsewhereSaid[item.ID]
	l.evaluationElsewhereSaid[item.ID] = true
	l.mu.Unlock()
	if said {
		return
	}
	holder := ""
	if m := evaluationHolderRE.FindStringSubmatch(refusal.Error()); m != nil {
		holder = m[1]
	} else if fresh, err := l.GW.WorkItem(ctx, item.ID); err == nil && fresh != nil && fresh.NodeID != "" && fresh.NodeID != item.NodeID {
		holder = fresh.NodeID
	}
	slug := item.GoalID
	if g, err := l.GW.Goal(ctx, item.GoalID); err == nil && g != nil && g.Slug != "" {
		slug = g.Slug
	}
	label := fmt.Sprintf("%s/%d", slug, item.Seq)
	if holder == "" {
		l.logf("item %s: evaluation is another node's; not evaluated here", label)
		return
	}
	l.logf("item %s: evaluation is node %s's; not evaluated here", label, holder)
}

// evaluationHolderRE reads the holder from a claimWorkItemEvaluation
// refusal, `node <id> has claimed the evaluation`.
var evaluationHolderRE = regexp.MustCompile(`node (\S+) has claimed the evaluation`)

// rebaseRequested claims every item in the served orgs whose rebase is
// requested and that no rebase here is on, most urgent goal first across
// the orgs (rebaseOrder; the goals are read only for an org with one),
// and starts a session of kind rebase on each claim it wins. Not gated
// by capacity: a rebase finishes work already done. A refusal — another
// node holds it, or it is no longer requested — is the ordinary answer
// and is not logged as an error.
func (l *Loop) rebaseRequested(ctx context.Context) {
	if l.Rebase == nil || !l.takesKind(harness.KindRebase) {
		return
	}
	var requested []readyItem
	for _, sc := range l.Scopes {
		rows, err := l.GW.RequestedRebases(ctx, sc.ID)
		if err != nil {
			l.logf("list requested rebases of %s: %v", sc.Name(), err)
			continue
		}
		if len(rows) == 0 {
			continue
		}
		rank := l.goalRanks(ctx, sc, "rebased")
		for _, it := range rows {
			requested = append(requested, readyItem{item: it, scope: sc, rank: rank(it.GoalID)})
		}
	}
	rebaseOrder(requested)
	capped := l.takesCap(harness.KindRebase)
	defer l.logCapped(capped)
	for _, r := range requested {
		item, sc := r.item, r.scope
		l.mu.Lock()
		if l.rebasing == nil {
			l.rebasing = map[string]bool{}
		}
		if l.rebasing[item.ID] {
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		if l.capFull(capped, item.GoalID, "rebase", priorityName(r.rank)) {
			continue
		}
		if l.passOverInfrastructure(item.GoalID, "rebase", item.ID) {
			// released as an infrastructure hold: not this node's again
			// until the hold ends
			continue
		}
		if why, until, ok := l.NoCredential.Holds(l.goalProject(ctx, sc, item), string(harness.KindRebase), l.adapter()); ok {
			// no credential open to this node, or none with headroom:
			// the rebase waits, said once for the goal
			l.passOver(item.GoalID, "rebase", priorityName(r.rank), why, until)
			l.NoCredential.Say(item.GoalID, why, until)
			continue
		}
		actx, ok := l.accountToFinish(ctx, priorityName(r.rank), "rebase")
		if !ok {
			continue
		}
		l.mu.Lock()
		l.rebasing[item.ID] = true
		l.mu.Unlock()
		// the claim is the lock across nodes: refused, another node has it
		if err := l.GW.ClaimWorkItemRebase(ctx, item.ID, sc.NodeID); err != nil {
			l.mu.Lock()
			delete(l.rebasing, item.ID)
			l.mu.Unlock()
			if !gateway.IsRefusal(err) {
				l.logf("%s: claim the rebase of item %d: %v", sc.Name(), item.Seq, err)
			}
			continue
		}
		capped.started()
		l.logf("%s: rebasing item %d %q", sc.Name(), item.Seq, item.Title)
		l.wg.Add(1)
		go func(ctx context.Context, item gateway.WorkItem) {
			defer crash.Guard()
			defer l.wg.Done()
			defer func() {
				l.mu.Lock()
				delete(l.rebasing, item.ID)
				l.mu.Unlock()
				l.notify()
			}()
			l.Rebase(ctx, item)
		}(actx, item)
	}
}

// planRequested starts the planner on every goal in planning in the
// served orgs that no planner here is on, most urgent goal first across
// the orgs (planOrder), each on the account chosen for its priority. A
// goal no account has headroom for is left for later.
func (l *Loop) planRequested(ctx context.Context) {
	if l.Plan == nil || !l.takesKind(harness.KindPlan) {
		return
	}
	var requested []planningGoal
	listed := map[string]bool{} // the orgs whose goals were read
	for _, sc := range l.Scopes {
		goals, err := l.goals(ctx, sc)
		if err != nil {
			l.logf("list goals of %s: %v", sc.Name(), err)
			continue
		}
		listed[sc.Name()] = true
		for _, g := range goals {
			if g.Status == "planning" {
				requested = append(requested, planningGoal{goal: g, scope: sc})
			}
		}
	}
	planOrder(requested)
	l.clearReleasedPlans(requested, listed)
	capped := l.takesCap(harness.KindPlan)
	defer l.logCapped(capped)
	for _, r := range requested {
		g, sc := r.goal, r.scope
		l.mu.Lock()
		if l.planning == nil {
			l.planning = map[string]bool{}
		}
		if l.planning[g.ID] {
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		if l.capFull(capped, g.ID, "plan", priorityName(gateway.PriorityRank(g.Priority))) {
			continue
		}
		// a planner session here declined a required tool whose runtime
		// is still missing: the goal waits for a node that has it
		runtimes, _ := l.Runtimes()
		if why, ok := l.Declines.Held(g.ID, runtimes, time.Now()); ok {
			l.passOver(g.ID, "plan", priorityName(gateway.PriorityRank(g.Priority)), why, time.Time{})
			continue
		}
		if why, until, ok := l.NoCredential.Holds(g.ProjectID, string(harness.KindPlan), l.adapter()); ok {
			l.passOver(g.ID, "plan", priorityName(gateway.PriorityRank(g.Priority)), why, until)
			l.NoCredential.Say(g.ID, why, until)
			continue
		}
		// a plan released for a repeated refusal waits for the goal's row
		// to change: no account, no claim (ADR-023, a refused plan is
		// released, not re-planned)
		if reason, ok := l.planReleased(g); ok {
			l.passOver(g.ID, "plan", priorityName(gateway.PriorityRank(g.Priority)), reason, time.Time{})
			l.NoteHeld(HeldGoal{GoalID: g.ID, Slug: g.Slug, Org: sc.Name(), Kind: "plan", Reason: reason, Since: l.heldSince(g.ID, "plan")})
			continue
		}
		actx, ok := l.accountFor(ctx, priorityName(gateway.PriorityRank(g.Priority)), true, "plan")
		if !ok {
			l.passOver(g.ID, "plan", priorityName(gateway.PriorityRank(g.Priority)), "", time.Time{})
			continue
		}
		l.mu.Lock()
		l.planning[g.ID] = true
		l.mu.Unlock()
		// the claim is the lock across nodes: refused, another node has it
		if err := l.GW.ClaimPlan(ctx, g.ID, sc.NodeID); err != nil {
			l.mu.Lock()
			delete(l.planning, g.ID)
			l.mu.Unlock()
			if gateway.IsRefusal(err) {
				l.logf("%s: goal %q is being planned elsewhere", sc.Name(), g.Slug)
			} else {
				l.logf("%s: claim planning of %q: %v", sc.Name(), g.Slug, err)
			}
			continue
		}
		capped.started()
		l.logf("%s: planning goal %q", sc.Name(), g.Slug)
		// on disk before the session opens: a node that exits before then
		// leaves the claim for the next process to re-open (ADR 009, Claims)
		l.recordClaim(ClaimRecord{NodeID: sc.NodeID, Kind: harness.KindPlan, GoalID: g.ID, ScopeID: sc.ID, Name: g.Slug})
		l.startPlan(actx, g, sc)
	}
}

// planReleased is PlanReleased's answer for g, false with no hook.
func (l *Loop) planReleased(g gateway.Goal) (string, bool) {
	if l.PlanReleased == nil {
		return "", false
	}
	return l.PlanReleased(g)
}

// clearReleasedPlans clears the plan hold of every goal of a listed org
// that is no longer released: its row changed, or it left planning.
func (l *Loop) clearReleasedPlans(requested []planningGoal, listed map[string]bool) {
	still := map[string]bool{}
	for _, r := range requested {
		if _, ok := l.planReleased(r.goal); ok {
			still[r.goal.ID] = true
		}
	}
	for _, h := range l.HeldGoals() {
		if h.Kind == "plan" && listed[h.Org] && !still[h.GoalID] {
			l.ClearHeld(h.GoalID, "plan")
		}
	}
}

// heldSince is when goal and kind was first held, now when it is not.
func (l *Loop) heldSince(goalID, kind string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range l.held {
		if h.GoalID == goalID && h.Kind == kind {
			return h.Since
		}
	}
	return l.now()
}

// researchDue offers every live direction of the served orgs to the
// platform: claiming is asking, and being refused is being told to wait
// — the window in the direction's own zone, the gap, and another node's
// claim are all the platform's to judge (runsheet trd §6c), so a
// refusal is not logged as an error. Each direction is offered on its
// own, so two directions of one project take their turns independently.
func (l *Loop) researchDue(ctx context.Context) {
	if l.Research == nil || !l.takesKind(harness.KindResearch) {
		return
	}
	capped := l.takesCap(harness.KindResearch)
	defer l.logCapped(capped)
	for _, sc := range l.Scopes {
		directions, err := l.GW.Directions(ctx, sc.ID, "")
		if err != nil {
			l.logf("list directions of %s: %v", sc.Name(), err)
			continue
		}
		live := make([]gateway.Direction, 0, len(directions))
		for _, d := range directions {
			if d.Active() {
				live = append(live, d)
			}
		}
		if len(live) == 0 {
			continue
		}
		projects, err := l.GW.Projects(ctx, sc.ID)
		if err != nil {
			l.logf("list projects of %s: %v", sc.Name(), err)
			continue
		}
		byID := make(map[string]gateway.Project, len(projects))
		for _, p := range projects {
			byID[p.ID] = p
		}
		// the org's goals once, to see which directions have room: a
		// set direction's session on a project at its ceiling would have
		// every proposal dropped, and an unattended night should not pay
		// for that (runsheet ADR 022). A propose direction is offered
		// whatever the room: its output is pitches (runsheet goal
		// pitches) or, on an older platform, GoalProposals a person
		// accepts, neither taking a live-goal slot — its session is told
		// the room as information.
		goals, err := l.goals(ctx, sc)
		if err != nil {
			l.logf("list goals of %s: %v", sc.Name(), err)
			continue
		}
		for _, d := range live {
			p, ok := byID[d.ProjectID]
			if !ok {
				continue // archived, or not this node's to see
			}
			if !d.Proposes() && RoomFor(d, p, goals) <= 0 {
				continue
			}
			l.mu.Lock()
			busy := l.researching[d.ID]
			l.mu.Unlock()
			if busy {
				continue
			}
			if l.capFull(capped, d.ID, "research", "") {
				continue
			}
			// a director session here declined a required tool whose
			// runtime is still missing: the direction is not offered
			runtimes, _ := l.Runtimes()
			if why, ok := l.Declines.Held(d.ID, runtimes, time.Now()); ok {
				l.passOver(d.ID, "research", "", why, time.Time{})
				continue
			}
			// the project's claim-time read refused research (none
			// issued, or none with headroom): the direction is not
			// offered until the hold's until, said once for it
			if why, until, ok := l.NoCredential.Holds(d.ProjectID, string(harness.KindResearch), l.adapter()); ok {
				l.passOver(d.ID, "research", "", why, until)
				l.NoCredential.Say(d.ID, why, until)
				continue
			}
			// research is not work that waits: it runs only on a
			// subscription with headroom for normal work, never on an
			// api account and never on a paced one; it has no priority
			// (""), so any credential's priorities serve it (adr-005's
			// amendment 2026-10-02)
			actx, ok := l.accountFor(ctx, "", false, "")
			if !ok {
				continue
			}
			l.mu.Lock()
			if l.researching == nil {
				l.researching = map[string]bool{}
			}
			if l.researching[d.ID] {
				l.mu.Unlock()
				continue
			}
			l.researching[d.ID] = true
			l.mu.Unlock()
			if err := l.GW.ClaimDirectionResearch(ctx, d.ID, sc.NodeID); err != nil {
				l.mu.Lock()
				delete(l.researching, d.ID)
				l.mu.Unlock()
				if !gateway.IsRefusal(err) {
					l.logf("%s: claim the research of %q: %v", sc.Name(), p.Slug+"/"+d.Slug, err)
				}
				continue
			}
			capped.started()
			l.logf("%s: researching %q", sc.Name(), p.Slug+"/"+d.Slug)
			l.recordClaim(ClaimRecord{NodeID: sc.NodeID, Kind: harness.KindResearch, DirectionID: d.ID, ScopeID: sc.ID, Name: p.Slug + "/" + d.Slug})
			l.startResearch(actx, d, p, sc)
		}
	}
}

// Adopt runs an item this node already holds on the platform (claimed
// or running, after a restart) without claiming it again, from the
// start: an item whose session is still there is AdoptSessions', which
// takes the session up instead. It reports whether the item was taken up
// (false when it is already running here, its session was adopted, or
// the node is draining — the node after it adopts the item).
func (l *Loop) Adopt(ctx context.Context, item gateway.WorkItem) bool {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	if l.running[item.ID] || l.adoptedIDs[item.ID] || l.draining {
		l.mu.Unlock()
		return false
	}
	l.running[item.ID] = true
	l.countForLocked(item.ID, item.OrgID)
	l.mu.Unlock()
	l.logf("adopted held item %d %q", item.Seq, item.Title)
	// already held: it runs on the account serving its goal's priority
	// with the most headroom, a paced one as if pressing, or on what the
	// claim-time read issues when none has any — never on a credential
	// reserved for other work (adr-005's amendment 2026-10-02)
	priority := l.goalPriority(ctx, item.GoalID)
	ctx = withPriority(ctx, priority)
	if actx, ok := l.accountToFinish(ctx, priority, ""); ok {
		ctx = actx
	}
	l.run(ctx, item)
	return true
}

// goalPriority is the priority of the goal an item belongs to, read
// from the platform; a goal it cannot read is "high", as a held item's
// was before priorities were served, said in the log.
func (l *Loop) goalPriority(ctx context.Context, goalID string) string {
	g, err := l.GW.Goal(ctx, goalID)
	if err != nil || g == nil {
		l.logf("adopt: goal %s: priority unread (%v); taken as high", goalID, err)
		return "high"
	}
	return priorityName(gateway.PriorityRank(g.Priority))
}

func (l *Loop) start(ctx context.Context, item gateway.WorkItem, org string) {
	l.mu.Lock()
	l.running[item.ID] = true
	l.countForLocked(item.ID, org)
	l.mu.Unlock()
	l.run(ctx, item)
}

// runsOn notes the account an item runs on, from ctx (DefaultAccount
// when the loop chose none).
func (l *Loop) runsOn(ctx context.Context, itemID string) {
	label := DefaultAccount
	if a, ok := ChosenAccount(ctx); ok {
		label = a.Label
	}
	l.mu.Lock()
	if l.runningOn == nil {
		l.runningOn = map[string]string{}
	}
	l.runningOn[itemID] = label
	l.mu.Unlock()
}

// run dispatches an item already marked running.
func (l *Loop) run(ctx context.Context, item gateway.WorkItem) {
	l.runsOn(ctx, item.ID)
	l.wg.Add(1)
	go func() {
		defer crash.Guard()
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			delete(l.running, item.ID)
			delete(l.runningOn, item.ID)
			delete(l.sessionOrg, item.ID)
			l.mu.Unlock()
			if l.Release != nil {
				l.Release(item)
			}
			l.notify()
		}()
		if l.Dispatch != nil {
			l.Dispatch(ctx, item)
		}
	}()
}

// evaluationHeld is the reason a project's evaluations wait and until
// when: its Claude evaluations are held (NoCredentials.Holds, kind
// evaluate) and Codex is no way round — no Codex room for the priority,
// or Codex held too. A Codex evaluation with room is not held by a
// refused Claude read: the evaluator runs it there (ADR 011).
func (l *Loop) evaluationHeld(projectID, priority string) (string, time.Time, bool) {
	why, until, ok := l.NoCredential.Holds(projectID, string(harness.KindEvaluate), AdapterClaudeCode)
	if !ok || l.CodexRoom == nil || !l.CodexRoom(priority) {
		return why, until, ok
	}
	if _, _, codexHeld := l.NoCredential.Holds(projectID, string(harness.KindEvaluate), AdapterCodex); codexHeld {
		return why, until, true
	}
	return "", time.Time{}, false
}

// adapter is the agent CLI the loop's builds and plans run on, as the
// claim-time read names it: claude-code unless the loop's harness is
// codex.
func (l *Loop) adapter() string {
	if l.Harness == AdapterCodex {
		return AdapterCodex
	}
	return AdapterClaudeCode
}

// resumedEntry is one retry turn kept in Loop.resumed, in the order noted.
type resumedEntry struct {
	seq     uint64
	session gateway.ResumedSession
}

// NoteRetryTurn records a retry turn on itemID — a session of kind taken
// up again by sessionID, or a fresh one when fresh — so every heartbeat
// carries it as `resumed` until the returned done func runs. done removes
// the entry and is safe to call more than once.
func (l *Loop) NoteRetryTurn(itemID, kind, sessionID string, fresh bool) (done func()) {
	l.mu.Lock()
	if l.resumed == nil {
		l.resumed = map[uint64]resumedEntry{}
	}
	l.resumedID++
	key := l.resumedID
	l.resumed[key] = resumedEntry{seq: key, session: gateway.ResumedSession{ItemID: itemID, Kind: kind, SessionID: sessionID, Fresh: fresh}}
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.resumed, key)
			l.mu.Unlock()
		})
	}
}

// Resumed is the heartbeat's `resumed`: the retry turns noted by
// NoteRetryTurn whose done func has not yet run, oldest first; nil when
// there are none.
func (l *Loop) Resumed() []gateway.ResumedSession {
	l.mu.Lock()
	entries := make([]resumedEntry, 0, len(l.resumed))
	for _, e := range l.resumed {
		entries = append(entries, e)
	}
	l.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	out := make([]gateway.ResumedSession, len(entries))
	for i, e := range entries {
		out[i] = e.session
	}
	return out
}
