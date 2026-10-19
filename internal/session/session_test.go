package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStepsRunInOrder(t *testing.T) {
	var order []string
	failBuild := errors.New("build failed")

	job := Job{
		Steps: []Step{
			{Name: "checkout", Run: func(ctx context.Context, s *State) error {
				order = append(order, "checkout")
				s.WorkspaceDir = "/work/item"
				return nil
			}},
			{Name: "build", Run: func(ctx context.Context, s *State) error {
				order = append(order, "build")
				return failBuild
			}},
			{Name: "validate", Run: func(ctx context.Context, s *State) error {
				order = append(order, "validate")
				return nil
			}},
			{Name: "finish", Run: func(ctx context.Context, s *State) error {
				order = append(order, "finish")
				return nil
			}},
			{Name: "release", Always: true, Run: func(ctx context.Context, s *State) error {
				order = append(order, "release")
				return nil
			}},
		},
	}

	state := &State{}
	records := Run(context.Background(), job, state)

	// Steps run strictly in order: checkout and build ran, the always-run
	// release ran after the failure, and the skipped main steps never ran.
	wantOrder := []string{"checkout", "build", "release"}
	if len(order) != len(wantOrder) {
		t.Fatalf("ran steps %v, want %v", order, wantOrder)
	}
	for i, name := range wantOrder {
		if order[i] != name {
			t.Fatalf("ran steps %v, want %v", order, wantOrder)
		}
	}

	if state.WorkspaceDir != "/work/item" {
		t.Fatalf("checkout's write to state did not survive: %q", state.WorkspaceDir)
	}

	if len(records) != len(job.Steps) {
		t.Fatalf("got %d records, want %d (one per step)", len(records), len(job.Steps))
	}

	wantOutcomes := []Outcome{Succeeded, Failed, Skipped, Skipped, Succeeded}
	for i, want := range wantOutcomes {
		rec := records[i]
		if rec.Name != job.Steps[i].Name {
			t.Errorf("record %d name = %q, want %q", i, rec.Name, job.Steps[i].Name)
		}
		if rec.Outcome != want {
			t.Errorf("record %d (%s) outcome = %q, want %q", i, rec.Name, rec.Outcome, want)
		}
	}

	if records[1].Err != failBuild {
		t.Errorf("build record's error = %v, want %v", records[1].Err, failBuild)
	}
	for _, i := range []int{0, 2, 3, 4} {
		if records[i].Err != nil {
			t.Errorf("record %d (%s) error = %v, want nil", i, records[i].Name, records[i].Err)
		}
	}

	// A skipped step never ran, so it never started or ended.
	for _, i := range []int{2, 3} {
		if !records[i].Started.IsZero() || !records[i].Ended.IsZero() {
			t.Errorf("skipped record %d (%s) has a started/ended time: %v / %v", i, records[i].Name, records[i].Started, records[i].Ended)
		}
	}

	// A step that ran has a started and ended time, ended not before started.
	for _, i := range []int{0, 1, 4} {
		if records[i].Started.IsZero() || records[i].Ended.IsZero() {
			t.Errorf("ran record %d (%s) missing started/ended time", i, records[i].Name)
		}
		if records[i].Ended.Before(records[i].Started) {
			t.Errorf("ran record %d (%s) ended before it started", i, records[i].Name)
		}
	}
}

func TestStepsAllPassRunEveryStep(t *testing.T) {
	var order []string
	job := Job{
		Steps: []Step{
			{Name: "checkout", Run: func(ctx context.Context, s *State) error {
				order = append(order, "checkout")
				return nil
			}},
			{Name: "build", Run: func(ctx context.Context, s *State) error {
				order = append(order, "build")
				return nil
			}},
			{Name: "release", Always: true, Run: func(ctx context.Context, s *State) error {
				order = append(order, "release")
				return nil
			}},
		},
	}

	records := Run(context.Background(), job, &State{})

	wantOrder := []string{"checkout", "build", "release"}
	if len(order) != len(wantOrder) {
		t.Fatalf("ran steps %v, want %v", order, wantOrder)
	}
	for i, name := range wantOrder {
		if order[i] != name {
			t.Fatalf("ran steps %v, want %v", order, wantOrder)
		}
	}

	for i, rec := range records {
		if rec.Outcome != Succeeded {
			t.Errorf("record %d (%s) outcome = %q, want %q", i, rec.Name, rec.Outcome, Succeeded)
		}
	}
}

func TestEndedStepSkipsTheRestButNotAlways(t *testing.T) {
	var ran []string
	step := func(name string, always bool, err error) Step {
		return Step{Name: name, Always: always, Run: func(context.Context, *State) error {
			ran = append(ran, name)
			return err
		}}
	}
	released := errors.New("released")
	recs := Run(context.Background(), Job{Steps: []Step{
		step("checkout", false, nil),
		step("run", false, End(released)),
		step("finish", false, nil),
		step("release", true, nil),
	}}, &State{})
	if got := strings.Join(ran, ","); got != "checkout,run,release" {
		t.Fatalf("ran %s", got)
	}
	want := []Outcome{Succeeded, Ended, Skipped, Succeeded}
	for i, r := range recs {
		if r.Outcome != want[i] {
			t.Errorf("step %s: %s, want %s", r.Name, r.Outcome, want[i])
		}
	}
	if recs[1].Err != released {
		t.Errorf("ended step's err %v", recs[1].Err)
	}
}
