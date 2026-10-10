package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mkdirs(t *testing.T, root string, names ...string) map[string]string {
	t.Helper()
	paths := map[string]string{}
	for _, n := range names {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Join(p, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		paths[n] = p
	}
	return paths
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A node restarted with one session still running (its HOME mounted by
// its running agent, its pod live) and one an earlier process left
// (its HOME unmounted, its pod with only the infra container running,
// its agent exited): only the second's HOME, pod and container go.
// Directories and pods that are not a session's are never touched.
func TestSweepRemovesOnlyWhatNoRunningContainerHolds(t *testing.T) {
	tmp := t.TempDir()
	d := mkdirs(t, tmp, "mesh-home-live", "mesh-home-stale", "mesh-node-image-1", "other")
	if err := os.WriteFile(filepath.Join(tmp, "mesh-home-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &Fake{
		Running: []Container{
			{Name: "mesh-build-live-agent", Pod: "mesh-build-live", Running: true, Mounts: []string{"/work/3", d["mesh-home-live"]}},
			{Name: "live-infra", Pod: "mesh-build-live", Running: true, Infra: true},
			{Name: "mesh-build-stale-agent", Pod: "mesh-build-stale", Labels: map[string]string{}},
			{Name: "stale-infra", Pod: "mesh-build-stale", Running: true, Infra: true},
			{Name: "labelled-exited", Labels: map[string]string{SessionLabel: "item 9"}},
			{Name: "postgres-dev"},
			{Name: "webapp", Pod: "dev-pod", Running: true, Mounts: []string{d["other"]}},
		},
		Standing: []PodInfo{
			{Name: "mesh-build-live", Labels: map[string]string{SessionLabel: "item 3"}},
			{Name: "mesh-build-stale", Labels: map[string]string{SessionLabel: "item 2"}},
			{Name: "labelled-only", Labels: map[string]string{SessionLabel: "item 7"}},
			{Name: "dev-pod"},
			{Name: "dev-empty"},
		},
	}
	res, err := Sweep(context.Background(), f, tmp)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	want := SweepResult{Homes: 1, Pods: 2, Containers: 2, HomesInUse: 1}
	if res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if exists(d["mesh-home-stale"]) {
		t.Fatal("the unmounted mesh-home is still there")
	}
	for _, keep := range []string{d["mesh-home-live"], d["mesh-node-image-1"], d["other"], filepath.Join(tmp, "mesh-home-file")} {
		if !exists(keep) {
			t.Fatalf("%s was removed", keep)
		}
	}
	if want := []string{"mesh-build-stale", "labelled-only"}; !reflect.DeepEqual(f.Removed, want) {
		t.Fatalf("pods removed %v, want %v", f.Removed, want)
	}
	if want := []string{"mesh-build-stale-agent", "labelled-exited"}; !reflect.DeepEqual(f.RemovedContainers, want) {
		t.Fatalf("containers removed %v, want %v", f.RemovedContainers, want)
	}
	if got := res.Line(); got != "sandbox: swept 1 homes, 2 pods, 2 containers at start (1 homes in use)" {
		t.Fatalf("line %q", got)
	}
}

// A HOME whose files a running container mounts one by one (not the
// directory itself) is in use too.
func TestSweepKeepsAHomeAPathInsideIsMounted(t *testing.T) {
	tmp := t.TempDir()
	d := mkdirs(t, tmp, "mesh-home-a")
	f := &Fake{Running: []Container{{Name: "mesh-x-agent", Running: true, Mounts: []string{filepath.Join(d["mesh-home-a"], ".claude")}}}}
	res, err := Sweep(context.Background(), f, tmp)
	if err != nil || res.HomesInUse != 1 || res.Homes != 0 || !exists(d["mesh-home-a"]) {
		t.Fatalf("res %+v err %v", res, err)
	}
}

// A runtime that cannot list its containers sweeps nothing: a HOME in
// use could not be told from a stale one.
func TestSweepWithNoListingRemovesNothing(t *testing.T) {
	tmp := t.TempDir()
	d := mkdirs(t, tmp, "mesh-home-a")
	f := &Fake{ListErr: errors.New("podman: cannot connect")}
	res, err := Sweep(context.Background(), f, tmp)
	if err == nil || !strings.Contains(err.Error(), "list containers") {
		t.Fatalf("err %v", err)
	}
	if res != (SweepResult{}) || !exists(d["mesh-home-a"]) {
		t.Fatalf("swept %+v without a listing", res)
	}
}

// A stale HOME the node cannot remove itself goes by the runtime's
// removal in its user namespace, as Close's does.
func TestSweepRemovesAStaleHomeInTheUserNamespace(t *testing.T) {
	tmp := t.TempDir()
	d := mkdirs(t, tmp, "mesh-home-a")
	failRemoveAll(t)
	f := &Fake{}
	res, err := Sweep(context.Background(), f, tmp)
	if err != nil || res.Homes != 1 || exists(d["mesh-home-a"]) {
		t.Fatalf("res %+v err %v", res, err)
	}
	if !reflect.DeepEqual(f.Trees, []string{d["mesh-home-a"]}) {
		t.Fatalf("trees %v", f.Trees)
	}
}

func TestParsePodmanListings(t *testing.T) {
	cs, err := ParseContainers(`[{"Names":["mesh-a-agent"],"PodName":"mesh-a","State":"exited","IsInfra":false,"Labels":null},
		{"Names":["abc-infra"],"PodName":"mesh-a","State":"running","IsInfra":true,"Labels":{"x":"y"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Container{{Name: "mesh-a-agent", Pod: "mesh-a"}, {Name: "abc-infra", Pod: "mesh-a", Running: true, Infra: true, Labels: map[string]string{"x": "y"}}}
	if !reflect.DeepEqual(cs, want) {
		t.Fatalf("containers %+v", cs)
	}
	m, err := ParseInspectMounts(`[{"Name":"mesh-a-agent","Mounts":[{"Type":"bind","Source":"/tmp/mesh-home-1","Destination":"/home/mesh"}]}]`)
	if err != nil || !reflect.DeepEqual(m, map[string][]string{"mesh-a-agent": {"/tmp/mesh-home-1"}}) {
		t.Fatalf("mounts %v %v", m, err)
	}
	ps, err := ParsePods(`[{"Name":"mesh-a","Labels":{"mesh.session":"item 1"},"Status":"Degraded"}]`)
	if err != nil || !reflect.DeepEqual(ps, []PodInfo{{Name: "mesh-a", Labels: map[string]string{SessionLabel: "item 1"}}}) {
		t.Fatalf("pods %v %v", ps, err)
	}
	if c, _ := ParseContainers(""); c != nil {
		t.Fatal("empty listing")
	}
}
