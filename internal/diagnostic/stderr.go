package diagnostic

import (
	"bytes"
	"strings"
	"sync"
)

// Stderr retains complete redacted lines and never retains a raw truncated tail.
type Stderr struct {
	mu                                      sync.Mutex
	redactor                                *Redactor
	partial                                 []byte
	lines                                   []string
	bytes                                   int
	discard, pemBlock, truncated, oversized bool
}

func NewStderr(redactor *Redactor, attached bool) *Stderr {
	return &Stderr{redactor: redactor, discard: attached, truncated: attached}
}

func (s *Stderr) Append(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(chunk) > 0 {
		end := bytes.IndexByte(chunk, '\n')
		piece := chunk
		if end >= 0 {
			piece = chunk[:end]
		}
		if !s.discard {
			if len(s.partial)+len(piece) > MaxInputBytes {
				s.partial = nil
				s.discard, s.truncated, s.oversized = true, true, true
			} else {
				s.partial = append(s.partial, piece...)
			}
		}
		if end < 0 {
			return
		}
		if !s.discard {
			s.line(string(s.partial))
		}
		s.partial = s.partial[:0]
		s.discard = false
		chunk = chunk[end+1:]
	}
}

func (s *Stderr) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.discard && len(s.partial) > 0 {
		s.line(string(s.partial))
	}
	s.partial = nil
}

func (s *Stderr) line(value string) {
	// A PEM block can span multiple stream records, unlike an RPC message.
	value = normalize(value)
	if len(value) > MaxInputBytes {
		s.truncated, s.oversized = true, true
		return
	}
	begin, end := strings.Contains(value, "-----BEGIN "), strings.Contains(value, "-----END ")
	if s.pemBlock || begin {
		s.pemBlock = !end
		value = redacted
	} else if base64Line.MatchString(strings.TrimSpace(value)) {
		value = redacted
	}
	clean, ok := s.redactor.Clean(value)
	if !ok {
		s.lines, s.bytes = nil, 0
		return
	}
	clean += "\n"
	if len(clean) > MaxStderrBytes {
		s.truncated = true
		return
	}
	for s.bytes+len(clean) > MaxStderrBytes {
		s.bytes -= len(s.lines[0])
		s.lines[0] = ""
		s.lines = s.lines[1:]
		s.truncated = true
	}
	s.lines = append(s.lines, clean)
	s.bytes += len(clean)
}

func (s *Stderr) Snapshot() (*string, bool, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redactor.Blocked() {
		s.lines, s.partial, s.bytes = nil, nil, 0
		return nil, s.truncated, []string{"redaction_limit"}
	}
	var omitted []string
	if s.oversized {
		omitted = append(omitted, "stderr_line_oversize")
	}
	// A credential may have rotated after a line was buffered. Apply the
	// current registry again before publishing the snapshot.
	clean, ok := s.redactor.Clean(strings.Join(s.lines, ""))
	if !ok {
		s.lines, s.partial, s.bytes = nil, nil, 0
		return nil, s.truncated, append(omitted, "redaction_limit")
	}
	for len(clean) > MaxStderrBytes {
		_, clean, _ = strings.Cut(clean, "\n")
		s.truncated = true
	}
	return &clean, s.truncated, omitted
}
