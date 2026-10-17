package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitdata"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Resolved is a work item with its goal, project and repo read.
type Resolved struct {
	Item    gateway.WorkItem
	Goal    *gateway.Goal
	Project *gateway.Project
	Repo    *gateway.Repo
}

// Branch is the item's branch name.
func (r *Resolved) Branch() string { return BranchName(r.Goal.Slug, r.Item.Seq) }

// BaseRef is the item's base ref, or the repo's default branch.
func (r *Resolved) BaseRef() string {
	if r.Item.BaseRef != "" {
		return r.Item.BaseRef
	}
	if r.Repo != nil && r.Repo.DefaultBranch != "" {
		return r.Repo.DefaultBranch
	}
	return "main"
}

// Resolve reads the item's goal, project and repo.
func Resolve(ctx context.Context, gw Gateway, item gateway.WorkItem) (*Resolved, error) {
	goal, err := gw.Goal(ctx, item.GoalID)
	if err != nil {
		return nil, fmt.Errorf("goal: %w", err)
	}
	project, err := gw.Project(ctx, goal.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	repo, err := gw.Repo(ctx, item.RepoID)
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	return &Resolved{Item: item, Goal: goal, Project: project, Repo: repo}, nil
}

// Runner runs one claimed item to its end (poll mode: `claude -p`).
type Runner struct {
	GW      Gateway
	Harness harness.Harness
	WS      Workspacer
	Cfg     *Config
	Scopes  Scopes
	// Roots, when set, are each served org's own roots (ADR-034): the
	// item's checkout, clone and scratch go under its org's, WorkRoot
	// unused for them. Nil keeps WorkRoot for every org.
	Roots    *OrgRootSet
	WorkRoot string
	// BuildCache is a host validation's GOCACHE; "" is BuildCacheDir().
	BuildCache string
	// MCP are the MCP servers every session gets (runsheet).
	MCP map[string]any
	// MCPFunc, when set, is asked for them at every item instead: the
	// node's machine access token expires within the hour, so a build
	// started long after the node did must carry a bearer taken now
	// (channel.Session.MCPFunc is the same seam).
	MCPFunc func(context.Context) map[string]any
	// MaxBudgetUSD caps a session's budget whatever the goal has left.
	MaxBudgetUSD float64
	// Model is the node's own build model (node.yaml build, else
	// default; "" the CLI's default): what a build session runs on when
	// the item names no model and its agent none (BuildModel).
	Model string
	// Credentials asks the platform for the session's credentials and
	// reports its usage record. Required: a nil one is a programming
	// error, and its Open panics (no node.yaml fallback, adr-005).
	Credentials *Credentials
	// ProgressEvery is how often progress is reported (60 s).
	ProgressEvery time.Duration
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	// BatchFallback is its merge batch candidates' notice, a platform
	// that mints no batch token (nil: every batch).
	BatchFallback *TokenFallback
	// Runtimes are the tool runtimes the node has now (Loop.Runtimes;
	// nil: none), which a local tool needs to start.
	Runtimes func() []string
	// Declines remembers an item released for a required tool whose
	// runtime the node lacks, so the loop does not take it again (nil:
	// not remembered).
	Declines *Declines
	// Condition is told the node's condition when a session's sandbox
	// limits kill it (Loop.NoteCondition, the heartbeat's reason); nil
	// tells no one.
	Condition func(reason string)
	// RuntimeBroken is told the error when a session's pod could not
	// start on a broken container runtime (a *sandbox.RuntimeError) and
	// its work was handed back runtime_broken; nil tells no one.
	RuntimeBroken func(error)
	// Budgets is node.yaml's `budgets:` in force, read live (ADR 012,
	// Loop.KindModelBudget); nil falls back to Cfg's own, read once at
	// start.
	Budgets func(kind, model string) (amount float64, source string)
	// ContextBudget is node.yaml's `context:` budget in force, read
	// live (Loop.ContextBudget); nil is Cfg's own, read once at start.
	ContextBudget func() ContextConfig
	// Sandbox, when set, runs the session in its own pod with the
	// services its checkout declares started beside it (ADR 007); nil is
	// `sandbox: off`, no services.
	Sandbox *sandbox.Sandbox
	// State keeps what an item's handed-back build spent its budget on
	// for the item's next claim (ADR 012 Hand-back); nil keeps nothing.
	State *RunState
	// HasCommit says whether dir's branch has a commit past origin/base
	// (nil: `git rev-list --count origin/<base>..HEAD`, HasCommitSince),
	// the harness's hand-back test.
	HasCommit func(dir, base string) bool
	// MeshBinary is the mesh executable a session's tool-trim hook runs
	// ("" is `mesh` on the session's PATH).
	MeshBinary string
	// Machine reads the host for the starve check (starve.go): a build
	// silent for host.starve_after while it reads overloaded is stopped
	// and released. nil never judges the host overloaded.
	Machine MachineProbe
	// Host is node.yaml's `host:` in force (Loop.HostThresholds); nil is
	// Cfg's own, read once at start.
	Host func() HostConfig
	// Inspect runs `podman image inspect` for a sandboxed validation's
	// runner image digest (nil: sandbox.ExecRunner).
	Inspect sandbox.Runner
	// Notices say once per run that the platform takes no validation
	// attestations, or no per-step minutes, yet (nil: every item).
	Notices *ValidationNotices
	// RetryTurns notes a validation retry turn for the heartbeat while
	// it runs (Loop.NoteRetryTurn): the item, the session kind, the
	// session id it resumes ("" fresh) and whether it is fresh; the func
	// it returns ends the note. nil notes nothing.
	RetryTurns func(itemID, kind, sessionID string, fresh bool) (done func())
	// Resumes keeps each finished build's session id and transcript for
	// the item's retry to resume (resumestore.go, goal
	// a-failed-check-returns-to-the-session-that-built-it), and a retry
	// turn puts its kept transcript back when the session's own is gone;
	// nil keeps and restores nothing.
	Resumes *ResumeStore
	// GitData and Cache, both set (ServedDiff), validate a pushed commit
	// — a merge batch candidate, a merged item's merge commit — from
	// GitHub's served diff in node-owned scratch (serveddiff.go); either
	// nil takes the worktree of the shared clone.
	GitData *gitdata.Client
	Cache   *SharedCache
	Log     *log.Logger

	// servedFallbackSaid is each reason the served diff fell back to the
	// worktree, said once per run; made by ServedDiff, nil says nothing
	// (a Runner never readied for it takes the worktree as it always did).
	servedFallbackSaid *sync.Map
}

// ServedDiff readies the Runner to validate a pushed commit from the
// served diff (serveddiff.go): client the Git Data reads, cache the
// shared tree cache whose misses it fills — cache.Source, when unset,
// GitDataSource on client, so every blob the diff writes is read once
// and cached. Either nil leaves the worktree path alone.
func (r *Runner) ServedDiff(client *gitdata.Client, cache *SharedCache) *Runner {
	r.GitData, r.Cache = client, cache
	r.servedFallbackSaid = &sync.Map{}
	if cache != nil && cache.Source == nil && client != nil {
		cache.Source = GitDataSource{Client: client}
	}
	return r
}

// NewServedDiffCache is the node's shared tree cache as its served-diff
// validations use it: under SharedCacheRoot, filled from the shared
// clones under ReposRoot when they hold an object, capped at node.yaml's
// validation_cache_max_bytes, its counts left in ~/.mesh for `mesh node
// status`.
func NewServedDiffCache(cfg *Config, logf func(string, ...any)) (*SharedCache, error) {
	root, err := SharedCacheRoot()
	if err != nil {
		return nil, fmt.Errorf("cache root: %w", err)
	}
	c := &SharedCache{Root: root, MaxBytes: cfg.ValidationCacheCap(), Logf: logf}
	if repos, err := ReposRoot(); err == nil {
		c.ReposRoot = repos
	}
	if st, err := DefaultRunState(); err == nil {
		c.State = &st
	}
	return c, nil
}

// toolResults bounds the job's tool results by node.yaml's
// tool_results (trd §3 Tool results): the CLI's limits on its env and,
// for a session on the host, the Bash hook in its settings — the pod has
// no mesh binary to run it.
func (r *Runner) toolResults(job *harness.Job) {
	kb := DefaultToolResultsKB
	if r.Cfg != nil {
		kb = r.Cfg.ToolResults.KB()
	}
	job.Env = append(job.Env, ToolResultsEnv(kb)...)
	if r.Sandbox == nil && job.Session == nil {
		bin := r.MeshBinary
		if bin == "" {
			bin = "mesh"
		}
		job.Settings = ToolTrimSettings(bin, kb)
	}
}

func (r *Runner) runtimes() []string {
	if r.Runtimes == nil {
		return nil
	}
	return r.Runtimes()
}

// mcp is the session's MCP servers, asked for afresh per item when
// MCPFunc is set.
func (r *Runner) mcp(ctx context.Context) map[string]any {
	if r.MCPFunc != nil {
		return r.MCPFunc(ctx)
	}
	return r.MCP
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// wireGitLogf gives a *Git WS its Logf, once, so a retried fetch's one
// line (ADR-023's Retry section) reaches the node's own log through
// r.logf, as every other line of the item does.
func (r *Runner) wireGitLogf() {
	if g, ok := r.WS.(*Git); ok && g.Logf == nil {
		g.Logf = r.logf
	}
}

// kindModelBudget is node.yaml's budgets: figure for kind and model —
// live, through r.Budgets, when set; else Cfg's own, read once at start.
func (r *Runner) kindModelBudget(kind, model string) (float64, string) {
	if r.Budgets != nil {
		return r.Budgets(kind, model)
	}
	return r.Cfg.KindModelBudget(kind, model)
}

// budgetWithSource is Budget with where the figure came from, for the
// session's starting log line.
func (r *Runner) budgetWithSource(goal *gateway.Goal, kind, model string) (float64, string) {
	kindBudget, kindSource := r.kindModelBudget(kind, model)
	return SessionBudget(kindBudget, kindSource, goal, r.MaxBudgetUSD)
}

// Budget is the session's cap (ADR 012): the smallest of the goal's
// remaining budget, node.yaml's budgets: figure for kind and model, and
// MaxBudgetUSD (0 in any of these means "the other one" wins).
func (r *Runner) Budget(goal *gateway.Goal, kind, model string) float64 {
	amount, _ := r.budgetWithSource(goal, kind, model)
	return amount
}

// hasCommit is the job's HasCommit for dir's branch against base.
func (r *Runner) hasCommit(dir, base string) func() bool {
	if r.HasCommit != nil {
		return func() bool { return r.HasCommit(dir, base) }
	}
	return HasCommitSince(dir, base)
}

// costReporter turns cumulative dollars into the cents-since-last-report
// each ReportWorkItemProgress and FinishWorkItem carries.
type costReporter struct {
	mu            sync.Mutex
	reportedCents int
	totalUSD      float64
	// baseUSD is what the sessions before the current one spent: a
	// validation retry's turn counts its own dollars from zero (carry).
	baseUSD float64
}

func (c *costReporter) observe(usd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if usd += c.baseUSD; usd > c.totalUSD {
		c.totalUSD = usd
	}
}

// carry starts the next session's dollars on top of the total so far.
func (c *costReporter) carry() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseUSD = c.totalUSD
}

// total is every session's dollars so far.
func (c *costReporter) total() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalUSD
}

// delta returns the unreported cents and marks them reported.
func (c *costReporter) delta() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := int(math.Round(c.totalUSD * 100))
	d := total - c.reportedCents
	if d < 0 {
		d = 0
	}
	c.reportedCents = total
	return d
}

// RunItem is the whole of one claimed item: resolve, workspace, context,
// session, progress, push, finish — or fail, or release it when GitHub
// refuses the item's token through every retry (ReleaseOnCredential) or
// its workspace cannot be prepared (ReleaseOnWorkspace, ADR-024). A
// cancelled ctx (the node's own shutdown) leaves the item as it is for a
// retry.
func (r *Runner) RunItem(ctx context.Context, item gateway.WorkItem) error {
	r.wireGitLogf()
	// every command the build sends acts as its builder (ADR-015)
	ctx = actorContext(ctx, r.Cfg, gateway.RoleBuilder)
	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("resolve: %w", err))
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	ctx = WithLockHolder(ctx, label)
	r.logf("item %s: %q on %s from %s", label, item.Title, res.Repo.FullName(), res.BaseRef())

	spec, err := orgSpec(r.Roots, r.Scopes, item.OrgID, WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()})
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("workspace: %w", err))
	}
	// live on this node while this runs: no cleanup touches it (HoldWorkspace)
	defer HoldWorkspace(spec.Dir())()
	// the item's own GitHub token, on the checkout before it is cloned
	cred, err := NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf)
	if we := WorkspaceErrorOf(err); we != nil {
		// the token could not be written into the repo cache: the
		// node's fault, released as an infrastructure hold, never failed
		return r.releaseInfrastructure(ctx, item, label, we)
	}
	if err != nil {
		return r.fail(ctx, item, err)
	}
	dir, err := PrepareWithRetry(ctx, r.WS, spec, cred, r.logf)
	if ce := (*CredentialError)(nil); errors.As(err, &ce) {
		sc, _ := r.Scopes.ByOrg(item.OrgID)
		return ReleaseOnCredential(ctx, r.GW, item.ID, sc.NodeID, ce, r.logf)
	}
	// a workspace that cannot be prepared is the node's condition, not
	// the item's: the claim is released with the error, never failed
	// (ADR-024, a workspace failure releases the claim)
	nodeID := func() string { sc, _ := r.Scopes.ByOrg(item.OrgID); return sc.NodeID }
	if err != nil {
		return r.releaseWorkspace(ctx, item.ID, label, nodeID(), err)
	}
	// the session commits as its agent with the pairers' trailers, never
	// as the node's owner (ADR-024)
	ident := identityFor(r.Cfg, tools.RoleFor(string(harness.KindBuild)), item, res.Goal)
	if err := r.WS.Identify(ctx, dir, ident); err != nil {
		return r.releaseWorkspace(ctx, item.ID, label, nodeID(), err)
	}

	orgName := r.Scopes.NameOf(item.OrgID)
	cx, err := FetchContext(ctx, r.GW, orgName, res.Project.Slug, ContextOptions{Budget: r.contextBudget(), Repos: []string{res.Repo.FullName()}, Checkouts: map[string]string{res.Repo.FullName(): dir}, Goal: res.Goal, Logf: r.logf})
	if err != nil {
		r.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	files, written, err := WriteContext(dir, cx)
	if err != nil {
		return r.releaseWorkspace(ctx, item.ID, label, nodeID(), fmt.Errorf("write context: %w", err))
	}
	r.logf("item %s: %s", label, written.Line())
	if w := written.TargetWarning(); w != "" {
		r.logf("item %s: %s", label, w)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || cx.Live != nil {
		contextDir = ContextDir
	}

	model := BuildModel(ctx, r.GW, item, r.Model)
	budget, budgetSource := r.budgetWithSource(res.Goal, "build", model)
	r.logf("item %s: build on %s, budget $%.2f (%s)", label, modelName(model), budget, budgetSource)

	// the clauses the last evaluation failed, so the retry works through a
	// list instead of mining them out of Evaluation's prose. Unread records
	// are not fatal: the build runs on the prose alone, as it did before.
	var failed []harness.FailedClause
	latest := gateway.PriorAttempt{}
	if item.Attempts > 0 {
		was, err := r.GW.WorkItemPriorAttempts(ctx, item.ID)
		if err != nil {
			r.logf("item %s: earlier evaluations unread, building on the notes alone: %v", label, err)
		}
		for _, p := range was {
			if p.Attempt >= latest.Attempt {
				latest = p
			}
		}
		for _, c := range latest.Clauses {
			failed = append(failed, harness.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
		}
	}
	mcp := r.mcp(ctx)
	in := harness.PromptInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalStatement: res.Goal.Statement, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, ItemID: item.ID, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: spec.Branch, BaseRef: spec.BaseRef, ContextDir: contextDir, ContextFiles: files,
		Attempts: item.Attempts, Evaluation: item.Evaluation, FailedClauses: failed, Project: orgName + "/" + res.Project.Slug, MeshContext: len(mcp) > 0,
		LastSession: r.State.LastSession(item.ID),
		FoldNotes:   r.foldNotes(ctx, item, label),
	}
	job := harness.Job{Kind: harness.KindBuild, Workspace: dir, MaxBudgetUSD: budget, Label: label, MCPServers: mcp, Model: model}
	// past 60% of its budget with nothing committed the session is handed
	// back for one commit-and-finish turn (ADR 012 Hand-back)
	job.HandBackAt = harness.DefaultHandBackAt
	job.HasCommit = r.hasCommit(dir, spec.BaseRef)
	if tok := cred.Token(); tok != nil {
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return r.fail(ctx, item, fmt.Errorf("gh config dir: %w", err))
		}
		// a session left running for the next node keeps its dirs: the
		// node that ends it removes them (TempDirs)
		defer unlessDetached(ctx, func() { os.RemoveAll(ghDir) })
		job.Env = SessionEnv(tok, ghDir)
	}
	// the session's own scratch as its TMPDIR, gone with the session
	// (sessionScratch); a detached one's by the node that ends it
	scratchRoot, err := scratchRootFor(r.Roots, r.Scopes, item.OrgID, r.WorkRoot)
	if err != nil {
		return r.fail(ctx, item, err)
	}
	scratch, err := sessionScratch(r.Sandbox, scratchRoot, r.BuildCache, harness.KindBuild, item.ID, &job)
	if err != nil {
		return r.fail(ctx, item, err)
	}
	defer unlessDetached(ctx, func() { RemoveScratch(scratch) })
	ident.Apply(&job)
	sc, _ := r.Scopes.ByOrg(item.OrgID)
	acct, err := r.Credentials.Open(withSubject(ctx, sc, cmp.Or(res.Project.ID, item.ProjectID), res.Project.Slug), gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item.ID, GoalID: item.GoalID}, sc.NodeID, Adapter(r.Harness))
	if nc, ok := NoCredentialOf(err); ok {
		// nothing issued for the adapter: the item waits, never on the
		// machine's own login; the reason is said once for the goal
		// (NoCredentials.Say), not on every release
		r.logf("item %s: no credential for %s; released", label, nc.Adapter)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, "accounts: "+nc.Error()); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if errors.Is(err, ErrNoAccount) {
		// no issued account has headroom: the item waits, not fails; an
		// ErrNoHeadroom's reason is said once for the goal
		// (NoCredentials.NoteHeadroom), not on every release
		if nh, ok := NoHeadroomOf(err); ok {
			r.logf("item %s: no issued %s credential has headroom; released", label, nh.Adapter)
		} else {
			r.logf("item %s: %v; released", label, err)
		}
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, "accounts: "+err.Error()); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if err != nil {
		return r.fail(ctx, item, err)
	}
	defer unlessDetached(ctx, acct.Cleanup)
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("item "+label, string(harness.KindBuild))
	acct.Apply(&job)
	job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct)}
	// the item's tools, beside the built-in runsheet: a required one this
	// node cannot start hands the item back for a node that can
	runtimes := r.runtimes()
	st := SelectTools(mcp, acct.Tools, tools.RoleFor(string(harness.KindBuild)), runtimes)
	if len(st.Missing) > 0 {
		reason := st.DeclineReason()
		r.Declines.Record(item.ID, st.Missing, runtimes, time.Now())
		r.logf("item %s: declined, %s; released", label, reason)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if err := st.UseTools(&job, mcp, acct); err != nil {
		return r.fail(ctx, item, fmt.Errorf("tools: %w", err))
	}
	if len(st.Use) > 0 {
		r.logf("item %s: tools %s", label, st.Names())
	}
	// the checkout's services, started in the session's pod before its
	// agent; the pod and its sidecars go on every way out
	pod, err := openOrgSessionPod(ctx, r.Sandbox, r.Roots, r.Scopes, item.OrgID, dir, label, harness.KindBuild, ident.Sandbox(dir), r.logf)
	// a session the harness detached at the node's shutdown runs on in
	// its pod, its services and HOME (Session.Detach makes this Close
	// nothing): the node that adopts it removes them
	defer pod.Close()
	if re := RuntimeErrorOf(err); re != nil {
		// the runtime itself cannot start the pod: the node's condition,
		// handed back, never an attempt charged (ADR-030)
		return r.releaseRuntime(ctx, item, label, re)
	}
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("services: %w", err))
	}
	if _, failed := pod.RequiredFailure(); failed {
		// a required service that does not come up hands the item back,
		// and no agent starts
		reason := pod.ReleaseReason()
		r.Declines.Record(item.ID, pod.Declined(), runtimes, time.Now())
		r.logf("item %s: %s; released", label, reason)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	pod.Apply(&job)
	r.toolResults(&job)
	in.Services = pod.Prompt()
	job.Prompt = harness.BuildPrompt(in)
	// a retry after a failed evaluation is the next turn of the session
	// that built the item when this node kept it, else a fresh session
	// told the same failure, saying why
	turn, retrying := retryAfterEvaluation(item, latest)
	freshPrompt, notResumed := job.Prompt, ""
	if retrying {
		turn.Branch = spec.Branch
		turn.HeadSHA, _ = gitHead(ctx, dir)
		turn.ChangedPaths = r.retryChangedPaths(ctx, label, dir, spec.BaseRef, defaultBranchOf(res))
		freshPrompt = freshRetryPrompt(job.Prompt, turn)
		rec, why := r.resumable(item.OrgID, item.ID, label, dir, acct, pod)
		if why == "" {
			job.Resume = rec.SessionID
			job.Prompt = harness.RetryTurnPrompt(turn)
			r.logf("item %s: resuming session %s with the failure", label, rec.SessionID)
		} else {
			notResumed = notResumedLine(label, why)
			job.Prompt = freshPrompt
		}
	}

	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	b := &buildRun{r: r, item: item, res: res, label: label, spec: spec, dir: dir, cred: cred, acct: acct, runtimes: runtimes, ident: ident, model: model, lastText: "session started", job: &job, pod: pod, started: time.Now(), nodeID: sc.NodeID}
	b.report(ctx, "session started: "+spec.Branch+" on "+modelName(model))
	endRetry := func() {}
	if retrying {
		endRetry = r.noteRetry(item.ID, job.Resume, job.Resume == "")
	}
	defer func() { endRetry() }()
	if notResumed != "" {
		r.logf("%s", notResumed)
		b.report(ctx, notResumed)
	}
	if note := st.OmittedNote(); note != "" {
		r.logf("item %s: %s", label, note)
		b.report(ctx, note)
	}
	if note := pod.MissingNote(); note != "" {
		r.logf("item %s: %s", label, note)
		b.report(ctx, note)
	}
	result, runErr := b.run(ctx, func(sctx context.Context, onProgress func(harness.Progress)) (harness.Result, error) {
		res, err := r.Harness.Run(sctx, job, onProgress)
		if job.Resume != "" && sctx.Err() == nil && harness.IsUnknownSession(err) {
			// the agent has no such session: fresh, once, with the failure
			line := notResumedLine(label, notResumedUnknownAgent)
			r.logf("%s", line)
			b.report(ctx, line)
			endRetry()
			endRetry = r.noteRetry(item.ID, "", true)
			job.Resume, job.Prompt = "", freshPrompt
			res, err = r.Harness.Run(sctx, job, onProgress)
		}
		return res, err
	})
	// the usage report rides the finish (afterBuild); Finish logs it when
	// nothing carried it
	defer acct.Finish(ctx, result)
	return r.afterBuild(ctx, b, result, runErr)
}

// buildRun is one build session as its tail needs it, whether this
// process started the session (RunItem) or adopted it (AdoptItem).
type buildRun struct {
	r        *Runner
	item     gateway.WorkItem
	res      *Resolved
	label    string
	spec     WorkspaceSpec
	dir      string
	cred     *ItemCred
	acct     *SessionAccount // nil for an adopted session
	runtimes []string
	cost     costReporter
	// ident is the session's commit identity, its pairers named on the
	// finish description (FinishWho); model is the session's own, named
	// there too.
	ident Identity
	model string
	// job is the session's job, for a validation retry's turn; pod its
	// sandbox, where the validation runs; started when it began. The job
	// and pod are nil for an adopted session.
	job     *harness.Job
	pod     *SessionPod
	started time.Time

	// nodeID is the node id the item is held by, for the lost-claim
	// check; watch is that check's hold on the session's ctx (lost.go),
	// set while the session runs.
	nodeID string
	watch  *claimWatch
	// lastHeard is when the session's stream last said something, in
	// unix nanoseconds, for the starve check (starve.go).
	lastHeard atomic.Int64

	mu        sync.Mutex
	lastText  string
	sessionID string
}

// session is the session's id as last read.
func (b *buildRun) session() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// podSession is the session's sandbox (nil: none, or adopted).
func (b *buildRun) podSession() *sandbox.Session {
	if b.pod == nil {
		return nil
	}
	return b.pod.Session
}

// description is the PR body FinishWorkItem and FinishWorkItemAlreadyDone
// carry (FinishDescription, every-actor-has-a-name item 3): who built the
// item, on what model and adapter, and who it was paired with — the
// evaluator is left to the verdict's own actor, not known yet here.
func (b *buildRun) description(ctx context.Context) string {
	actor, _ := gateway.ActorFrom(ctx)
	return FinishDescription(FinishWho{Actor: actor, Model: b.model, Adapter: Adapter(b.r.Harness), Pairers: b.ident.Pairers})
}

// report is the item's heartbeat: the note (the session's last text when
// empty), the cents since the last one and the session id. Its error is
// the platform's answer, for the lost-claim check (lostRefusal).
func (b *buildRun) report(ctx context.Context, note string) error {
	b.mu.Lock()
	n, sid := note, b.sessionID
	if n == "" {
		n = b.lastText
	}
	b.mu.Unlock()
	err := b.r.GW.ReportWorkItemProgress(ctx, b.item.ID, truncate(n, 2000), b.cost.delta(), sid)
	if err != nil {
		b.r.logf("item %s: progress: %v", b.label, err)
	}
	return err
}

// reportToolUse notes on the item the served tools the session never
// called or whose calls errored (SessionAccount.UseNote); nothing when
// each was called cleanly, or for an adopted session.
func (b *buildRun) reportToolUse(ctx context.Context, result harness.Result) {
	if note := b.acct.UseNote(result); note != "" {
		b.r.logf("item %s: %s", b.label, note)
		b.report(ctx, note)
	}
}

// reportHandoff sends the session's last mesh_progress note again when it
// opens docsWriterNote, after the node's own notes (the final text, the
// tool use, the attestation) and before the finish, so the item's last
// note is the hand-off the docs writer reads and not the node's.
func (b *buildRun) reportHandoff(ctx context.Context, result harness.Result) {
	if note := handoffNote(result); note != "" {
		b.report(ctx, note)
	}
}

// handoffNote is the session's last mesh_progress note when it opens
// docsWriterNote, else "".
func handoffNote(result harness.Result) string {
	call := result.WorkCalls["mesh_progress"]
	note, _ := call["note"].(string)
	note = strings.TrimSpace(note)
	if !strings.HasPrefix(strings.ToLower(note), docsWriterNote) {
		return ""
	}
	return note
}

// run runs the session through start — Harness.Run or Attach — with
// the item's heartbeat every ProgressEvery and its token renewed. Each
// tick also checks the node still holds the item (checkClaim): a claim
// lost — the item read back terminal, cancelled, ready or another
// node's, or its heartbeat refused as not held — cancels the session's
// ctx with ErrClaimLost, which stops its unit and kills what it holds.
func (b *buildRun) run(ctx context.Context, start func(ctx context.Context, onProgress func(harness.Progress)) (harness.Result, error)) (harness.Result, error) {
	every := b.r.ProgressEvery
	if every <= 0 {
		every = 60 * time.Second
	}
	sctx, watch := newClaimWatch(ctx)
	defer watch.release()
	b.watch = watch
	b.heard()
	tickCtx, stopTicker := context.WithCancel(sctx)
	defer stopTicker()
	go func() {
		defer crash.Guard()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				if !b.checkClaim(tickCtx) || !b.checkStarved() {
					return
				}
				// a session may outlive its token's hour
				if err := b.cred.Refresh(tickCtx); err != nil && tickCtx.Err() == nil {
					b.r.logf("item %s: %v", b.label, err)
				}
			}
		}
	}()
	return start(sctx, func(p harness.Progress) {
		b.heard()
		b.mu.Lock()
		if p.Text != "" {
			b.lastText = b.acct.Redact(p.Text)
		}
		if p.SessionID != "" {
			b.sessionID = p.SessionID
		}
		b.mu.Unlock()
		if p.CostUSD > 0 {
			b.cost.observe(p.CostUSD)
		}
	})
}

// afterBuild is what follows a build session, however this process came
// to hold it: the item released on a sandbox limit, failed on an error
// or an unsuccessful end, finished already done (no push: main holds the
// work), or pushed and finished — in that order: the finish is the
// node's alone (the session's mesh_finish lands on the node's own
// server and records nothing), handed in only on the head Push
// returned, and a push that fails is the item's failure with no finish
// sent, the branch left for a retry (ADR-023 Decision 2). A cancelled ctx — the
// node shutting down — leaves the item and its still-running session
// for the next node to adopt. Once the item is finished or failed the
// session directory goes.
func (r *Runner) afterBuild(ctx context.Context, b *buildRun, result harness.Result, runErr error) error {
	item, label := b.item, b.label
	// what the agent left running in the session's unit, killed at its
	// end (ADR 008, The session's end): a line in the log — logged before
	// a done ctx returns, since a deadline kills the unit too — and a
	// note on the item. A detach kills nothing.
	if why := b.watch.lost(); why != "" && ctx.Err() == nil {
		// the item is no longer the node's: stopped, and nothing sent
		return r.afterLost(b, why, result)
	}
	if why := b.watch.starved(); why != "" && ctx.Err() == nil {
		// the host starved it: stopped, and released with the overload
		return r.afterStarved(ctx, b, why, result)
	}
	killedNote := logKilled(r.logf, "item "+label, result.KilledPerSession(), result.KillError)
	if ctx.Err() != nil {
		r.logf("item %s: node shutting down; its session is left for the next node to adopt", label)
		return ctx.Err()
	}
	defer harness.RemoveSession(b.dir)
	b.cost.observe(result.CostUSD)
	if result.SessionID != "" {
		b.mu.Lock()
		b.sessionID = result.SessionID
		b.mu.Unlock()
	}
	// kept before the deferred RemoveSession takes the session dir, and
	// with it the transcript a retry resumes from
	r.keepResume(b)
	if killedNote != "" {
		b.report(ctx, killedNote)
	}
	if le := (*sandbox.LimitError)(nil); errors.As(runErr, &le) {
		// the node's condition, not the item's failure (ADR 007 Limits)
		reason := LimitReason(label, le)
		b.report(ctx, reason)
		r.Declines.Record(item.ID, []tools.Omission{{Tool: "sandbox:" + le.Limit, Runtime: le.Value}}, b.runtimes, time.Now())
		if r.Condition != nil {
			r.Condition(reason)
		}
		r.logf("item %s: %s", label, reason)
		bsc, _ := r.Scopes.ByOrg(b.item.OrgID)
		if rerr := r.GW.ReleaseWorkItem(ctx, b.item.ID, bsc.NodeID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if re := RuntimeErrorOf(runErr); re != nil {
		// the pod the harness opened could not start: handed back
		return r.releaseRuntime(ctx, item, label, re)
	}
	if runErr != nil {
		runErr = b.acct.RedactErr(runErr)
		b.report(ctx, "harness error: "+runErr.Error())
		return r.fail(ctx, item, runErr)
	}
	result.Text = b.acct.Redact(result.Text)
	b.handedBack(ctx, result)
	if !result.Success {
		b.report(ctx, "session ended without success: "+result.Text)
		b.reportToolUse(ctx, result)
		return r.fail(ctx, item, fmt.Errorf("session ended without success: %s", truncate(result.Text, 500)))
	}
	b.report(ctx, result.Text)
	b.reportToolUse(ctx, result)

	// the session's end is its mesh_finish or mesh_fail call when it made
	// one (ADR-023): the final message need not be formatted, or there at
	// all; with neither, the text is parsed as before and that is logged
	fin := finishCallOf(result)
	if fin.fail != "" {
		return r.fail(ctx, item, errors.New(b.acct.Redact(fin.fail)))
	}
	if !fin.called {
		r.logf("item %s: no mesh_finish call; parsed the final message", label)
	}
	evidence, already := fin.evidence, fin.evidence != ""
	if !fin.called {
		evidence, already = harness.ParseAlreadyDone(result.Text)
	}
	if already {
		// main already holds the work (adr-003): no push, no branch, and
		// the evaluator checks the evidence on the base ref
		if err := b.acct.Carried(r.GW.FinishWorkItemAlreadyDone(ctx, item.ID, truncate(b.acct.Redact(evidence), 4000), b.cost.delta(), b.acct.Report(result), b.description(ctx))); err != nil {
			return fmt.Errorf("finish %s: %w", label, err)
		}
		r.logf("item %s: finished already done ($%.2f): %s", label, result.CostUSD, truncate(evidence, 200))
		return nil
	}
	// the repository's validation on the commit the item finishes with
	// (runsheet ADR 032): a failing step gets the builder one more turn,
	// then fails the item naming it
	v, err := r.startValidation(ctx, b)
	if err != nil {
		b.report(ctx, "validation: "+err.Error())
		return r.fail(ctx, item, fmt.Errorf("validation: %w", err))
	}
	if v != nil {
		defer v.close()
		_, err = r.validateBuild(ctx, b, v, result)
		if errors.Is(err, ErrClaimLost) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rep := b.acct.Report(result); rep != nil {
			rep.Validation = v.usage()
		}
		var vf *validationFailure
		if errors.As(err, &vf) {
			if len(v.runs) > 0 {
				r.uploadAttestation(ctx, b, v.runs[len(v.runs)-1])
			}
			b.report(ctx, vf.Error())
			return r.fail(ctx, item, vf)
		}
		if err != nil {
			err = b.acct.RedactErr(err)
			b.report(ctx, "validation: "+err.Error())
			return r.fail(ctx, item, fmt.Errorf("validation: %w", err))
		}
	}
	// the push is part of the build: no finish is sent until it has
	// returned the head it pushed, and a refused push is the build's
	// failure, named, never a finish
	sha, err := r.pushBuild(ctx, b)
	if err != nil {
		err = b.acct.RedactErr(err)
		b.report(ctx, err.Error())
		return r.fail(ctx, item, err)
	}
	if v != nil {
		// after the last push and before the finish: the platform signs for
		// the head it can see while the item is still this node's
		r.attestPushed(ctx, b, v, sha)
		if rep := b.acct.Report(result); rep != nil {
			rep.Validation = v.usage()
		}
	}
	b.reportHandoff(ctx, result)
	if err := b.acct.Carried(r.validationUsage(r.GW.FinishWorkItem(ctx, item.ID, b.spec.Branch, sha, b.cost.delta(), b.acct.Report(result), b.description(ctx)))); err != nil {
		return fmt.Errorf("finish %s: %w", label, err)
	}
	r.logf("item %s: pushed %s; finished %s ($%.2f)", label, short(sha), b.spec.Branch, result.CostUSD)
	return nil
}

// pushBuild pushes the build's branch on the build's own write token:
// the checkout's helper is rewritten with the token issued at claim (or
// its Refresh) just before the push (ItemCred.ForPush), so a token
// issued afterwards — an evaluation's read token — is never the one
// used. A push GitHub refuses for its credential is re-issued once and
// tried again; still refused it is a *PushCredentialError naming the
// repo, the branch and the token's access, issue time and expiry, and
// logged `push refused (<token>): <git's words>; not finished`. Any
// other failure is `push <branch>: <err>`. The token is scrubbed from
// whatever git said.
func (r *Runner) pushBuild(ctx context.Context, b *buildRun) (string, error) {
	branch := b.spec.Branch
	tok, err := b.cred.ForPush(ctx)
	if err != nil {
		r.logf("item %s: %v", b.label, err) // the token held may yet serve
	}
	var seen []string
	if tok != nil {
		seen = append(seen, tok.Token)
	}
	sha, err := r.WS.Push(OnItemToken(ctx, r.WS, tok), b.dir, branch)
	if err != nil && b.cred != nil && IsCredentialRefusal(err) && ctx.Err() == nil {
		r.logf("item %s: push refused (%s): %v; re-issuing the token once", b.label, FactsOf(tok), scrubbed(err, seen))
		if rerr := b.cred.Reissue(ctx); rerr != nil {
			r.logf("item %s: %v", b.label, rerr)
		} else {
			tok = b.cred.Token()
			seen = append(seen, tok.Token)
			sha, err = r.WS.Push(OnItemToken(ctx, r.WS, tok), b.dir, branch)
		}
	}
	if err == nil {
		return sha, nil
	}
	err = scrubbed(err, seen)
	if b.cred != nil && IsCredentialRefusal(err) {
		repo := b.spec.Repo
		if tok != nil && tok.Repository != "" {
			repo = tok.Repository
		}
		r.logf("item %s: push refused (%s): %v; not finished", b.label, FactsOf(tok), err)
		return "", &PushCredentialError{Repo: repo, Branch: branch, Token: FactsOf(tok), Cause: err}
	}
	r.logf("item %s: push %s: %v; not finished", b.label, branch, err)
	return "", fmt.Errorf("push %s: %w", branch, err)
}

// finishCall is a build session's end as its mesh-work tool calls say
// it (ADR-023, sessions finish by tool call): called when it called
// mesh_finish — evidence set for outcome already_done — and fail the
// error of a mesh_fail call made with no mesh_finish.
type finishCall struct {
	called   bool
	evidence string
	fail     string
}

// finishCallOf reads result.WorkCalls for the session's finish. A
// mesh_finish with outcome already_done and no evidence is no finish:
// the final message is parsed for it instead. The branch and head_sha
// the session names are not trusted: the node pushes its own checkout
// and hands in the head it pushed.
func finishCallOf(result harness.Result) finishCall {
	if in, ok := result.WorkCalls["mesh_finish"]; ok {
		if workArg(in, "outcome") != gateway.OutcomeAlreadyDone {
			return finishCall{called: true}
		}
		if ev := workArg(in, "evidence"); ev != "" {
			return finishCall{called: true, evidence: ev}
		}
		return finishCall{}
	}
	if in, ok := result.WorkCalls["mesh_fail"]; ok {
		msg := workArg(in, "error")
		if msg == "" {
			msg = cmp.Or(workArg(in, "reason"), "no reason given")
		}
		return finishCall{called: true, fail: "mesh_fail: " + msg}
	}
	return finishCall{}
}

// workArg is a work tool input's string argument, trimmed ("" when it is
// absent or not a string).
func workArg(in map[string]any, key string) string {
	v, _ := in[key].(string)
	return strings.TrimSpace(v)
}

// handedBack records what a handed-back session spent its budget on
// under the item, for its next claim's prompt, and notes it on the item
// before the finish (ADR 012 Hand-back); a session that ran to its end
// clears the record. A handed-back build that committed finishes as any
// other; one that still did not is pushed and finished empty, which
// the platform routes back to ready (empties-go-back-to-the-builder).
func (b *buildRun) handedBack(ctx context.Context, result harness.Result) {
	r := b.r
	if !result.HandedBack {
		if result.Success {
			r.State.ClearLastSession(b.item.ID)
		}
		return
	}
	spent := b.acct.Redact(result.SpentOn)
	if err := r.State.RecordLastSession(b.item.ID, spent); err != nil {
		r.logf("item %s: last session: %v", b.label, err)
	}
	note := "handed back with nothing committed: " + spent
	r.logf("item %s: %s", b.label, note)
	b.report(ctx, note)
}

// AdoptItem takes up a build session a node process before this one
// started (ADR 009): the item is not claimed again and no session is
// started; the one in s is followed from its saved offset — or, gone,
// read from its file — with the heartbeat and token renewal RunItem
// gives its own, and then afterBuild, as RunItem ends. An item the
// platform no longer has running on this node is not this session's to
// finish: its session is stopped and its directory removed.
func (r *Runner) AdoptItem(ctx context.Context, item gateway.WorkItem, s FoundSession) error {
	ctx = actorContext(ctx, r.Cfg, gateway.RoleBuilder)
	if item.Status != "claimed" && item.Status != "running" || item.NodeID != s.State.NodeID {
		r.logf("item %s: adopted session is no longer this node's (%s); stopped", item.ID, item.Status)
		stopFound(r.logf, "item "+item.ID, s)
		return s.Remove()
	}
	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("resolve: %w", err))
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	ctx = WithLockHolder(ctx, label)
	spec, err := orgSpec(r.Roots, r.Scopes, item.OrgID, WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()})
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("workspace: %w", err))
	}
	// live on this node while this runs: no cleanup touches it (HoldWorkspace)
	defer HoldWorkspace(spec.Dir())()
	// the item's token on the checkout again: the session's pushes, and
	// the node's, need a live one
	cred, err := NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf)
	if we := WorkspaceErrorOf(err); we != nil {
		// the session runs on, unadopted: stopped, as an item no longer
		// this node's is, before the claim goes back
		stopFound(r.logf, "item "+item.ID, s)
		return r.releaseInfrastructure(ctx, item, label, we)
	}
	if err != nil {
		return r.fail(ctx, item, err)
	}
	at, ok := r.Harness.(harness.Attacher)
	if !ok {
		return r.fail(ctx, item, errNoAttach)
	}
	// the finish description names the same agent and pairers the
	// session committed as, though this process did not start it
	ident := identityFor(r.Cfg, tools.RoleFor(string(harness.KindBuild)), item, res.Goal)
	b := &buildRun{r: r, item: item, res: res, label: label, spec: spec, dir: s.State.Workspace, cred: cred, runtimes: r.runtimes(), ident: ident, model: s.State.Model, lastText: "session adopted", sessionID: s.State.SessionID, nodeID: s.State.NodeID}
	where := fmt.Sprintf("pid %d", s.State.PID)
	attachCtx := func(c context.Context) context.Context { return c }
	if pod := r.adoptedPod(s, item); pod != nil {
		// the session's pod and HOME, kept past the agent's end for the
		// validation in it, go once the build is handed in — unless this
		// node lets go of the session in turn
		b.pod, where = pod, "container "+s.State.Container
		attachCtx = harness.WithPodKept
		defer unlessDetached(ctx, pod.Close)
	}
	r.logf("item %s: adopted its %s session (%s, from byte %d)", label, s.Liveness(), where, s.State.Offset)
	result, runErr := b.run(ctx, func(sctx context.Context, onProgress func(harness.Progress)) (harness.Result, error) {
		return at.Attach(attachCtx(sctx), s.Dir, onProgress)
	})
	return r.afterBuild(ctx, b, result, runErr)
}

// adoptedPod is an adopted sandboxed build's session reattached: its
// HOME, which validation runs in, and its pod, both removed by its Close.
// A pod already gone leaves the HOME alone to reattach (validation's
// steps run in pods of their own either way, Session.Run). Nil for a
// host session, a node with no sandbox, or a HOME already gone —
// validation then opens a fresh session of its own (validator.start),
// and Attach's end removes what is left.
func (r *Runner) adoptedPod(s FoundSession, item gateway.WorkItem) *SessionPod {
	if !s.Sandboxed() || r.Sandbox == nil || s.State.Home == "" {
		return nil
	}
	if _, err := os.Stat(s.State.Home); err != nil {
		return nil
	}
	// the adopted session's own org's store, as its Open had (orgSandbox)
	sb, err := orgSandbox(r.Sandbox, r.Roots, r.Scopes, item.OrgID)
	if err != nil {
		r.logf("item %s: adopted pod: package store: %v; validating in a fresh session", s.State.Label, err)
		return nil
	}
	var sess *sandbox.Session
	if s.standingPod() {
		sess = sb.Reattach(sandbox.Pod{Name: s.State.Pod}, s.State.Home, s.State.Label)
	} else {
		sess = sb.ReattachHome(s.State.Pod, s.State.Home, s.State.Label)
	}
	return &SessionPod{Session: sess, logf: r.logf}
}

func (r *Runner) fail(ctx context.Context, item gateway.WorkItem, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// an environment failure's reason opens with its kind (adr-030)
	reason := validate.Reason(cause)
	r.logf("item %s: failed: %s", item.ID, reason)
	if err := r.GW.FailWorkItem(ctx, item.ID, truncate(reason, 2000)); err != nil && !errors.Is(err, context.Canceled) {
		r.logf("item %s: fail: %v", item.ID, err)
	}
	return cause
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// UserEnv is the environment a session inherits (the operator's).
func UserEnv() []string { return os.Environ() }

// contextBudget is the context budget for r's sessions: live, through
// r.ContextBudget, when set; else Cfg's own.
func (r *Runner) contextBudget() ContextConfig {
	if r.ContextBudget != nil {
		return r.ContextBudget()
	}
	if r.Cfg == nil {
		return ContextConfig{}.Effective()
	}
	return r.Cfg.Context.Effective()
}

// docsWriterNote opens a note an item leaves for its goal's shared-docs
// writer (gateway.BriefWithoutSharedDocs asks for it).
const docsWriterNote = "for the docs writer:"

// foldNotes is what the goal's shared-docs writer folds in (ADR-025):
// for an item whose brief marks it the writer
// (gateway.SharedDocsWriterOf), each sibling gateway.SharedDocsSiblings
// names whose last note opens "for the docs writer:", with its pull
// request, in seq order. Any other item, or a goal whose items cannot
// be read, gets none: the brief still names the siblings, and the
// session reads them with mesh_items.
func (r *Runner) foldNotes(ctx context.Context, item gateway.WorkItem, label string) []harness.ItemNote {
	if !gateway.SharedDocsWriterOf(item.Brief) {
		return nil
	}
	seqs := gateway.SharedDocsSiblings(item.Brief)
	if len(seqs) == 0 {
		return nil
	}
	items, err := r.GW.GoalWorkItems(ctx, item.GoalID)
	if err != nil {
		r.logf("item %s: siblings' notes unread, building on the brief alone: %v", label, err)
		return nil
	}
	bySeq := make(map[int]gateway.WorkItem, len(items))
	for _, it := range items {
		bySeq[it.Seq] = it
	}
	var notes []harness.ItemNote
	for _, seq := range seqs {
		it, ok := bySeq[seq]
		if !ok || !strings.HasPrefix(strings.TrimSpace(it.LastNote), docsWriterNote) {
			continue
		}
		notes = append(notes, harness.ItemNote{Seq: it.Seq, Title: it.Title, Note: strings.TrimSpace(it.LastNote), PRURL: it.PRURL})
	}
	return notes
}

// retryChangedPaths are the paths the judged head changed against base
// for a retry turn's message: against def, the repo's default branch,
// when base — a merged dependency's branch — is gone from origin, said
// in node.log; none when they cannot be read.
func (r *Runner) retryChangedPaths(ctx context.Context, label, dir, base, def string) []string {
	paths, from, err := changedPathsFrom(ctx, r.WS, dir, base, def)
	if err != nil {
		return nil
	}
	if from != base {
		r.logf("item %s: retry turn: base %s is gone (merged); changed paths from origin/%s", label, base, from)
	}
	return paths
}
