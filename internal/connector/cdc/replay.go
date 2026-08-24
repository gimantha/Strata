package cdc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gimantha/strata/internal/domain"
)

// ReplayStream reads change events from a JSONL change log.
//
// This is the reference adapter AGENTS.md phase 10 allows in place of a live database
// connector, and it is the more useful of the two to have first. A live adapter needs a
// replication slot, a running upstream, and a network; a change log is a file, which means
// the ordering, tombstone, and replay behavior everything else depends on can be tested
// exactly and reproduced from a bug report.
//
// The format is one JSON object per line, in the shape of domain.ChangeEvent. Every real CDC
// pipeline can be exported to it — Debezium writes something close already — so "replay a
// customer's change log" is a supported way to reproduce a problem rather than an exercise.
type ReplayStream struct {
	name    string
	scanner *bufio.Scanner
	closer  io.Closer
	line    int
}

// maxChangeLine bounds one line of a change log. Row images with large text columns are
// ordinary; a line larger than this is a corrupt file rather than a big row.
const maxChangeLine = 4 << 20

// NewReplayStream reads a change log from any reader.
func NewReplayStream(name string, r io.Reader) *ReplayStream {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxChangeLine)
	return &ReplayStream{name: name, scanner: scanner}
}

// OpenReplayFile reads a change log from disk.
func OpenReplayFile(path string) (*ReplayStream, error) {
	const op = "cdc.OpenReplayFile"

	file, err := os.Open(path)
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeInvalidArgument, op,
			"cannot open the change log")
	}
	stream := NewReplayStream("replay:"+path, file)
	stream.closer = file
	return stream, nil
}

// NewReplayEvents replays events already in memory, for tests and for push connectors that
// have a batch in hand.
func NewReplayEvents(name string, events []domain.ChangeEvent) *ReplayStream {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, event := range events {
		_ = encoder.Encode(event)
	}
	return NewReplayStream(name, &buf)
}

func (s *ReplayStream) Name() string { return s.name }

// Next decodes the next line.
func (s *ReplayStream) Next(_ context.Context) (domain.ChangeEvent, error) {
	const op = "cdc.ReplayStream.Next"

	for s.scanner.Scan() {
		s.line++
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			// Blank lines and comments: a change log is often hand-edited when
			// reproducing a problem, and refusing a comment would make that worse.
			continue
		}

		var event domain.ChangeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return domain.ChangeEvent{}, domain.Wrap(err, domain.CodeInvalidArgument, op,
				"cannot decode change log line "+strconv.Itoa(s.line))
		}
		return event, nil
	}

	if err := s.scanner.Err(); err != nil {
		return domain.ChangeEvent{}, domain.Wrap(err, domain.CodeInvalidArgument, op,
			"cannot read the change log")
	}
	return domain.ChangeEvent{}, io.EOF
}

// Close releases the underlying file, if there is one.
func (s *ReplayStream) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}
