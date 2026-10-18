package node

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecordBudget is how many characters of an item's record.jsonl a fresh
// session is given (ItemRecord): past it, the oldest whole entries are
// dropped until what remains fits, the record's first turn entry always
// kept whatever its age. No entry is ever shortened or reworded to fit.
const RecordBudget = 6000

// RecordEntry is one verbatim line of an item's record: a note, a
// question, an answer, a check result, a hand-back or a turn's start or
// end, in the order it happened. Text is the session's or the tool's
// own words — never an env value, a token or a credential.
type RecordEntry struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

func (r ResumeStore) recordJSONLPath(itemID string) string {
	return filepath.Join(r.itemDir(itemID), "record.jsonl")
}

// AppendRecord appends entry to itemID's record.jsonl as one JSON line,
// opened O_APPEND: the file is never rewritten, so a line already read
// stays byte-equal. The item's dir is made if Keep has not made it yet —
// the record may start before any session is kept.
func (r ResumeStore) AppendRecord(itemID string, entry RecordEntry) error {
	if itemID == "" {
		return errors.New("resume store: no item id")
	}
	if err := os.MkdirAll(r.itemDir(itemID), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.recordJSONLPath(itemID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// ItemRecord reads itemID's record.jsonl and returns the entries kept
// under RecordBudget oldest first, their raw source lines (byte-equal to
// what AppendRecord wrote), how many whole entries were dropped from the
// front to fit, and whether a record was read at all (false when the
// file does not exist or cannot be read). Over budget, the oldest
// entries are dropped first — except the record's first turn entry,
// never dropped whatever its age — until the total of what remains fits;
// an entry is never shortened or reworded.
func ItemRecord(store ResumeStore, itemID string) (entries []RecordEntry, lines []string, dropped int, ok bool) {
	data, err := os.ReadFile(store.recordJSONLPath(itemID))
	if err != nil {
		return nil, nil, 0, false
	}
	var allLines []string
	var allEntries []RecordEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e RecordEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		allLines = append(allLines, line)
		allEntries = append(allEntries, e)
	}
	if len(allEntries) == 0 {
		return nil, nil, 0, true
	}
	entries, lines, dropped = clipRecord(allEntries, allLines)
	return entries, lines, dropped, true
}

// clipRecord holds a record's entries and their source lines under
// RecordBudget: the oldest whole entries are dropped first, the first
// turn entry never, until what remains fits; an entry is never
// shortened. It returns what is kept, oldest first, and how many went.
func clipRecord(allEntries []RecordEntry, allLines []string) (entries []RecordEntry, lines []string, dropped int) {
	total := 0
	for _, l := range allLines {
		total += len(l)
	}
	keep := make([]bool, len(allEntries))
	for i := range keep {
		keep[i] = true
	}
	if total > RecordBudget {
		firstTurn := -1
		for i, e := range allEntries {
			if e.Kind == "turn" {
				firstTurn = i
				break
			}
		}
		for i := 0; i < len(allEntries) && total > RecordBudget; i++ {
			if i == firstTurn {
				continue
			}
			total -= len(allLines[i])
			keep[i] = false
			dropped++
		}
	}
	for i, k := range keep {
		if k {
			entries = append(entries, allEntries[i])
			lines = append(lines, allLines[i])
		}
	}
	return entries, lines, dropped
}
