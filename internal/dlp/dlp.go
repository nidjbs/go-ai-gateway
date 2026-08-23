// Package dlp provides output-side data-loss prevention: scanning model
// responses for PII / sensitive patterns and masking them or rejecting the
// response, in both streaming and non-streaming handlers. Detection is
// regex-based and conservative by design.
package dlp

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
)

// Pattern is one named detection rule.
type Pattern struct {
	ID    string
	Regex *regexp.Regexp
}

// builtinPatterns is the full catalogue; categories are activated by ID.
var builtinPatterns = []Pattern{
	{ID: "email", Regex: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	{ID: "phone", Regex: regexp.MustCompile(`(?:\+?1[-.\s]?)?(?:\(?\d{3}\)?[-.\s]?)\d{3}[-.\s]?\d{4}`)},
	{ID: "credit_card", Regex: regexp.MustCompile(`(?:\d[ -]?){13,19}`)},
	{ID: "ip_address", Regex: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
	{ID: "secret_key", Regex: regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}\b|\bAKIA[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{ID: "ssn", Regex: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{ID: "url", Regex: regexp.MustCompile(`https?://[^\s"']+`)}, // off by default (false-positive-prone)
}

// DefaultPatternIDs is the active set when Patterns is empty (URL excluded:
// URLs are common in legitimate output).
var DefaultPatternIDs = []string{"email", "phone", "credit_card", "ip_address", "secret_key", "ssn"}

// Config mirrors the YAML surface. Enabled=false disables processing.
type Config struct {
	Enabled   bool
	Mode      string // "mask" | "reject"
	MaskText  string
	Patterns  []string
	CarrySize int // stream boundary window in bytes
}

// Hit names one detected pattern.
type Hit struct {
	Pattern string `json:"pattern"`
}

// Detector is the compiled, immutable rule set.
type Detector struct {
	cfg      Config
	patterns []Pattern
}

// New compiles the config; unknown pattern IDs fail startup rather than
// silently disabling protection.
func New(cfg Config) (*Detector, error) {
	if !cfg.Enabled {
		return &Detector{cfg: cfg}, nil
	}
	ids := cfg.Patterns
	if len(ids) == 0 {
		ids = DefaultPatternIDs
	}
	byID := make(map[string]Pattern, len(builtinPatterns))
	for _, p := range builtinPatterns {
		byID[p.ID] = p
	}
	var selected []Pattern
	for _, id := range ids {
		p, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("dlp: unknown pattern %q (available: %v)", id, knownIDs())
		}
		selected = append(selected, p)
	}
	if cfg.MaskText == "" {
		cfg.MaskText = "[REDACTED]"
	}
	if cfg.CarrySize <= 0 {
		cfg.CarrySize = 256
	}
	return &Detector{cfg: cfg, patterns: selected}, nil
}

func knownIDs() []string {
	out := make([]string, 0, len(builtinPatterns))
	for _, p := range builtinPatterns {
		out = append(out, p.ID)
	}
	sort.Strings(out)
	return out
}

func (d *Detector) Enabled() bool    { return d.cfg.Enabled }
func (d *Detector) RejectMode() bool { return d.cfg.Enabled && d.cfg.Mode == "reject" }
func (d *Detector) MaskMode() bool   { return d.cfg.Enabled && d.cfg.Mode == "mask" }
func (d *Detector) MaskText() string { return d.cfg.MaskText }

// Scan returns one Hit per matching pattern.
func (d *Detector) Scan(text string) []Hit {
	if !d.Enabled() || text == "" {
		return nil
	}
	var hits []Hit
	for _, p := range d.patterns {
		if p.Regex.MatchString(text) {
			hits = append(hits, Hit{Pattern: p.ID})
		}
	}
	return hits
}

// Mask replaces every match in text with MaskText. Overlapping matches are
// merged so no fragment of a detected secret survives.
func (d *Detector) Mask(text string) (string, []Hit) {
	if !d.Enabled() || text == "" {
		return text, nil
	}
	out, hits := d.maskBytes([]byte(text))
	return string(out), hits
}

type span struct{ s, e int }

func (d *Detector) maskBytes(data []byte) ([]byte, []Hit) {
	var spans []span
	seen := make(map[string]bool)
	for _, p := range d.patterns {
		for _, loc := range p.Regex.FindAllIndex(data, -1) {
			spans = append(spans, span{loc[0], loc[1]})
			seen[p.ID] = true
		}
	}
	if len(spans) == 0 {
		return data, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s < spans[j].s })
	merged := mergeSpans(spans)

	var buf bytes.Buffer
	last := 0
	for _, sp := range merged {
		buf.Write(data[last:sp.s])
		buf.WriteString(d.cfg.MaskText)
		last = sp.e
	}
	buf.Write(data[last:])

	hits := make([]Hit, 0, len(seen))
	for id := range seen {
		hits = append(hits, Hit{Pattern: id})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Pattern < hits[j].Pattern })
	return buf.Bytes(), hits
}

func mergeSpans(spans []span) []span {
	out := []span{spans[0]}
	for _, sp := range spans[1:] {
		last := &out[len(out)-1]
		if sp.s <= last.e {
			if sp.e > last.e {
				last.e = sp.e
			}
		} else {
			out = append(out, sp)
		}
	}
	return out
}

// StreamMasker applies Mask across an incremental byte stream, keeping a
// trailing window so patterns split across chunk boundaries are detected.
// Not safe for concurrent use.
type StreamMasker struct {
	detector *Detector
	carry    []byte
	carryCap int
}

func (d *Detector) NewStreamMasker() *StreamMasker {
	return &StreamMasker{detector: d, carryCap: d.cfg.CarrySize}
}

// Process masks chunk and returns the bytes safe to emit now; the tail is
// held back as carry for the next call. Call Flush at stream end.
func (m *StreamMasker) Process(chunk []byte) []byte {
	if !m.detector.Enabled() || len(chunk) == 0 {
		return chunk
	}
	buf := make([]byte, 0, len(m.carry)+len(chunk))
	buf = append(buf, m.carry...)
	buf = append(buf, chunk...)
	masked, _ := m.detector.maskBytes(buf)

	keep := m.carryCap
	if len(masked) < keep {
		keep = len(masked)
	}
	out := masked[:len(masked)-keep]
	m.carry = append(m.carry[:0], masked[len(masked)-keep:]...)
	return out
}

// Flush returns the held-back tail and resets the masker.
func (m *StreamMasker) Flush() []byte {
	out := m.carry
	m.carry = nil
	return out
}

// Reject reports any matching pattern in chunk (reject mode), advancing the
// carry window with raw bytes. A masker is used in either Process or
// Reject mode, never both.
func (m *StreamMasker) Reject(chunk []byte) []Hit {
	if !m.detector.Enabled() || len(chunk) == 0 {
		return nil
	}
	buf := make([]byte, 0, len(m.carry)+len(chunk))
	buf = append(buf, m.carry...)
	buf = append(buf, chunk...)
	hits := m.detector.Scan(string(buf))

	keep := m.carryCap
	if len(buf) < keep {
		keep = len(buf)
	}
	m.carry = append(m.carry[:0], buf[len(buf)-keep:]...)
	return hits
}
