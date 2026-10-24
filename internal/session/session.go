package session

import (
	"context"
	"errors"
	"time"
)

// State carries what one step hands the next: the workspace a checkout step
// prepares, the pod a run step opens, the base ref a validate step diffs
// against, and each step's own results keyed by step name. Later items add
// fields as later steps need them; this stays the small, shared scratchpad.
type State struct {
	WorkspaceDir string
	Pod          any
	BaseRef      string
	// BaseSHA is the commit BaseRef was at when the checkout was made —
	// the base the builder's change was built on — "" when the checkout
	// did not record it (an adopted session); the finish step then reads
	// it back as the merge-base of HEAD and BaseRef.
	BaseSHA string
	Results map[string]any
}

// Outcome is a step's result, as the node logs it.
type Outcome string

const (
	// Succeeded is a step whose Run returned nil.
	Succeeded Outcome = "succeeded"
	// Failed is a main step whose Run returned an error, or an always-run
	// step whose Run returned an error.
	Failed Outcome = "failed"
	// Skipped is a main step never run because an earlier main step failed
	// or ended the job.
	Skipped Outcome = "skipped"
	// Ended is a main step that brought the job to its end itself — the
	// item released, failed by the node or finished already done — by
	// returning End: the main steps after it are skipped, the always-run
	// ones still run, and the record's Err is what End was given.
	Ended Outcome = "ended"
)

// endError is a step's End.
type endError struct{ err error }

func (e *endError) Error() string {
	if e.err == nil {
		return "ended"
	}
	return e.err.Error()
}

func (e *endError) Unwrap() error { return e.err }

// End is a step's way to end the job without failing it: Run records the
// step Ended, carrying err (nil for none), and skips the main steps after
// it.
func End(err error) error { return &endError{err: err} }

// Step is one unit of a Job. Always marks a step that runs whatever earlier
// steps did — passed or failed — the way an Actions `post`/`if: always()`
// step does; a cleanup or release step sets it. A step with Always false
// runs only while every earlier main step has passed.
type Step struct {
	Name   string
	Always bool
	Run    func(ctx context.Context, state *State) error
}

// Job is an ordered list of steps.
type Job struct {
	Steps []Step
}

// Record is one step's outcome, returned by Run in step order.
type Record struct {
	Name    string
	Started time.Time
	Ended   time.Time
	Outcome Outcome
	Err     error
}

// Run runs job's steps in order against state: main steps stop at the first
// failure, and every always-run step still runs afterwards, in order,
// whatever came before. It returns one Record per step, in step order.
func Run(ctx context.Context, job Job, state *State) []Record {
	records := make([]Record, 0, len(job.Steps))
	failed := false

	for _, step := range job.Steps {
		if failed && !step.Always {
			records = append(records, Record{Name: step.Name, Outcome: Skipped})
			continue
		}

		started := time.Now()
		err := step.Run(ctx, state)
		ended := time.Now()

		outcome := Succeeded
		var end *endError
		switch {
		case errors.As(err, &end):
			outcome, err = Ended, end.err
			failed = true
		case err != nil:
			outcome = Failed
			failed = true
		}

		records = append(records, Record{
			Name:    step.Name,
			Started: started,
			Ended:   ended,
			Outcome: outcome,
			Err:     err,
		})
	}

	return records
}
