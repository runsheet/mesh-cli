package node

import (
	"fmt"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Every org gets its share of a node (ADR-023, amending ADR-017's
// order): a node serving several orgs divides its capacity into a floor
// per org, claims first for an org holding fewer sessions than its floor
// while it has ready work, and gives the slots left to claimOrder across
// orgs. A floor is not a ceiling: an org with nothing ready leaves its
// floor to the others.

// Floors divides capacity among the served orgs by weight, keyed by org
// id. weights is keyed by the org's Name() (its login, or its slug when
// it has none); a missing or zero weight is 1, and nil weights are equal
// shares. Each org's floor is capacity×weight/total rounded down, and
// the slots the rounding leaves go one each to the largest remainders,
// the orgs in listed order on a tie, so the floors sum to capacity:
// capacity 4 over two orgs is 2 and 2, at weights 3:1 it is 3 and 1, and
// capacity 5 is 3 and 2.
func Floors(capacity int, scopes Scopes, weights map[string]int) map[string]int {
	floors := make(map[string]int, len(scopes))
	if len(scopes) == 0 {
		return floors
	}
	if capacity < 0 {
		capacity = 0
	}
	w := make([]int, len(scopes))
	total := 0
	for i, sc := range scopes {
		w[i] = weights[sc.Name()]
		if w[i] <= 0 {
			w[i] = 1
		}
		total += w[i]
	}
	rem := make([]int, len(scopes))
	given := 0
	for i, sc := range scopes {
		floors[sc.ID] += capacity * w[i] / total
		rem[i] = capacity * w[i] % total
		given += capacity * w[i] / total
	}
	for ; given < capacity; given++ {
		best := -1
		for i := range scopes {
			if rem[i] > 0 && (best < 0 || rem[i] > rem[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		floors[scopes[best].ID]++
		rem[best] = 0
	}
	return floors
}

// shareOrder puts ready items from every org into the order they are
// claimed in (ADR-023): while an org holds fewer sessions than its floor
// (held < floors) and has ready items left, its best item by claimOrder
// goes next — the org furthest below its floor first, the orgs in the
// order they were listed (their first item's place in ready) on a tie —
// and is counted as held; then everything left, in claimOrder across
// orgs (ADR-017). The floor section ignores the goal's priority by
// design. The items taken for a floor carry floor, with the floor and
// what the org held before it. Neither ready nor held is changed.
func shareOrder(ready []readyItem, floors, held map[string]int) []readyItem {
	var orgs []string
	seen := map[string]bool{}
	for _, r := range ready {
		if !seen[r.scope.ID] {
			seen[r.scope.ID] = true
			orgs = append(orgs, r.scope.ID)
		}
	}
	sorted := append([]readyItem(nil), ready...)
	claimOrder(sorted)
	byOrg := map[string][]readyItem{}
	for _, r := range sorted {
		byOrg[r.scope.ID] = append(byOrg[r.scope.ID], r)
	}
	count := map[string]int{}
	for org, n := range held {
		count[org] = n
	}
	taken := map[string]bool{}
	out := make([]readyItem, 0, len(ready))
	for {
		best, deficit := "", 0
		for _, org := range orgs {
			if d := floors[org] - count[org]; d > deficit && len(byOrg[org]) > 0 {
				best, deficit = org, d
			}
		}
		if best == "" {
			break
		}
		r := byOrg[best][0]
		byOrg[best] = byOrg[best][1:]
		r.floor, r.held = floors[best], count[best]
		count[best]++
		taken[r.item.ID] = true
		out = append(out, r)
	}
	for _, r := range sorted {
		if !taken[r.item.ID] {
			out = append(out, r)
		}
	}
	return out
}

// Weights are the shares Floors divides capacity by, keyed by the org's
// Name(): node.yaml's `shares:`, reloaded each tick (Loop.reload) over
// Cfg's before the first tick; nil is equal shares. The owner answer's
// `shares` (a later item of the goal) will take it over node.yaml's.
func (l *Loop) Weights() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sharesSet {
		return cloneShares(l.shares)
	}
	if l.Cfg == nil {
		return nil
	}
	return cloneShares(l.Cfg.Shares)
}

// cloneShares copies a weights map so Weights never hands out the
// loop's own.
func cloneShares(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sharesEqual compares two weight maps.
func sharesEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// sharesString renders a weights map for a reload log line: "a 3, b 1",
// sorted by org, or "none" when empty.
func sharesString(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	orgs := make([]string, 0, len(m))
	for org := range m {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	parts := make([]string, len(orgs))
	for i, org := range orgs {
		parts[i] = fmt.Sprintf("%s %d", org, m[org])
	}
	return strings.Join(parts, ", ")
}

// countForLocked records that the session of subject (an item, a goal's plan
// or a direction's research) counts toward org's share. l.mu is held.
func (l *Loop) countForLocked(subject, org string) {
	if org == "" {
		return
	}
	if l.sessionOrg == nil {
		l.sessionOrg = map[string]string{}
	}
	l.sessionOrg[subject] = org
}

// countFor is countForLocked taking l.mu.
func (l *Loop) countFor(subject, org string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.countForLocked(subject, org)
}

// heldByOrg counts the sessions each org holds on this node: its builds,
// plans and research. Evaluations and rebases do not count, as they do
// not count against capacity.
func (l *Loop) heldByOrg() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	held := map[string]int{}
	for _, m := range []map[string]bool{l.running, l.planning, l.researching} {
		for subject := range m {
			if org := l.sessionOrg[subject]; org != "" {
				held[org]++
			}
		}
	}
	return held
}

// orgOfNode is the org whose node id is nodeID among the served scopes,
// or "" when none is.
func (l *Loop) orgOfNode(nodeID string) string {
	for _, sc := range l.Scopes {
		if sc.NodeID == nodeID {
			return sc.ID
		}
	}
	return ""
}

// HoldingOrg is one served org's share of the node as reported: its
// floor, the sessions it holds and its ready items (ADR-023, Reporting),
// the heartbeat's holding.orgs and node.shares alike.
type HoldingOrg = gateway.HoldingOrg

// holdingOrgs is one row per served org, in listed order: its floor by
// Floors, what it holds by heldByOrg and its ready items by ready, keyed
// by org id — -1 for an org whose ready read failed or that has not
// been listed yet. Always a list, [] with no org.
func holdingOrgs(scopes Scopes, floors, held, ready map[string]int) []HoldingOrg {
	out := []HoldingOrg{}
	for _, sc := range scopes {
		n, ok := ready[sc.ID]
		if !ok {
			n = -1
		}
		out = append(out, HoldingOrg{Org: sc.Name(), OrgID: sc.ID, Floor: floors[sc.ID], Held: held[sc.ID], Ready: n})
	}
	return out
}

// shareRows is holdingOrgs on the loop as it stands: the capacity in
// force, the weights, the sessions held now and the ready counts
// claimReady last listed.
func (l *Loop) shareRows() []HoldingOrg {
	held := l.heldByOrg()
	l.mu.Lock()
	ready := l.readyByOrg
	l.mu.Unlock()
	return holdingOrgs(l.Scopes, Floors(l.Capacity(), l.Scopes, l.Weights()), held, ready)
}

// noteReady records the ready items each org's listing returned this
// claim pass, keyed by org id; an org whose read failed is left out.
func (l *Loop) noteReady(scopes Scopes, listed [][]gateway.WorkItem, failed []bool) {
	ready := make(map[string]int, len(scopes))
	for i, sc := range scopes {
		if !failed[i] {
			ready[sc.ID] = len(listed[i])
		}
	}
	l.mu.Lock()
	l.readyByOrg = ready
	l.mu.Unlock()
}
