package gitrelay

// Minted are the tokens the grant has minted so far: a lease's own
// secrets, which its node redacts from the session's transcript before
// it leaves the node (projectmesh-io/api docs/adr/adr-001-amendment-
// learnings-and-transcripts.md, "Transcripts").
func (g *Grant) Minted() []Token {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Token(nil), g.minted...)
}
