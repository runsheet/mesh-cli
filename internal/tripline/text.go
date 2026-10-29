package tripline

// Text scrubs one string as an event's fields are scrubbed: the
// literal secrets and their fragments, the tool env's values, every
// secret pattern and the operator's home. A lease's transcript is
// redacted with it before it leaves the node (projectmesh-io/api
// docs/adr/adr-001-amendment-learnings-and-transcripts.md).
func (s *Scrubber) Text(in string) string {
	if s == nil {
		s = defaultScrubber
	}
	return s.text(in)
}
