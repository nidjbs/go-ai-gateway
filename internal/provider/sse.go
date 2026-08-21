package provider

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// SSE event field names per the W3C Server-Sent Events spec.
var (
	sseFieldEvent = []byte("event")
	sseFieldData  = []byte("data")
	sseFieldID    = []byte("id")
)

// sseMaxLine caps a single SSE line. The bufio.Scanner default of 64 KiB is
// too small for large JSON payloads; 4 MiB matches the prior scanner cap.
const sseMaxLine = 4 << 20

// sseMaxEvent caps the size of one event's concatenated data payload.
// 16 MiB covers worst-case tool-call JSON deltas.
const sseMaxEvent = 16 << 20

// sseEvent is one parsed SSE event.
type sseEvent struct {
	Type string // value of "event:" field, "" if absent
	Data []byte // concatenated "data:" payloads joined by '\n'
	ID   string // value of "id:" field, "" if absent
}

// sseParser reads a buffered SSE stream and yields one event per blank-line
// boundary. It tolerates CR/LF and LF line endings and ignores comment lines
// (those beginning with ':').
type sseParser struct {
	reader *bufio.Reader
	done   bool
}

// newSSEParser wraps r with a bufio.Reader sized for SSE lines.
func newSSEParser(r io.Reader) *sseParser {
	return &sseParser{reader: bufio.NewReaderSize(r, 64*1024)}
}

// ErrSSEEventTooLarge is returned when a single event exceeds the size cap.
var ErrSSEEventTooLarge = errors.New("sse event exceeded size cap")

// ErrSSELineTooLarge is returned when a single line exceeds the size cap.
var ErrSSELineTooLarge = errors.New("sse line exceeded size cap")

// Next returns the next SSE event. It returns io.EOF when the stream
// completes cleanly after a blank-line boundary; otherwise it returns any
// underlying read or parse error.
func (p *sseParser) Next() (sseEvent, error) {
	if p.done {
		return sseEvent{}, io.EOF
	}
	var evt sseEvent
	var data bytes.Buffer
	for {
		line, err := p.readLine()
		if line != nil {
			// Comment line — ignore.
			if line[0] != ':' {
				field, value := splitField(line)
				switch string(field) {
				case string(sseFieldEvent):
					evt.Type = string(value)
				case string(sseFieldData):
					if data.Len() > 0 {
						if data.Len()+1+len(value) > sseMaxEvent {
							return sseEvent{}, ErrSSEEventTooLarge
						}
						data.WriteByte('\n')
					} else if len(value) > sseMaxEvent {
						return sseEvent{}, ErrSSEEventTooLarge
					}
					data.Write(value)
				case string(sseFieldID):
					evt.ID = string(value)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Flush any pending event, including an unterminated final line.
				if data.Len() > 0 || evt.Type != "" || evt.ID != "" {
					evt.Data = append(evt.Data[:0], data.Bytes()...)
					return evt, nil
				}
				p.done = true
				return sseEvent{}, io.EOF
			}
			return sseEvent{}, err
		}
		if line == nil {
			// Blank line marks end of event; emit it.
			if data.Len() > 0 || evt.Type != "" || evt.ID != "" {
				evt.Data = append(evt.Data[:0], data.Bytes()...)
				return evt, nil
			}
			continue
		}
	}
}

// readLine reads one line (LF or CRLF terminated) and trims the terminator.
// Returns (nil, nil) for a blank line; (line, nil) for content;
// (nil, io.EOF) at end of stream; or a size error if a line is too long.
func (p *sseParser) readLine() ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := p.reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if buf.Len()+len(chunk) > sseMaxLine {
				return nil, ErrSSELineTooLarge
			}
			buf.Write(chunk)
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, ErrSSELineTooLarge
			}
			// Trim trailing \n (and optional \r) for content lines.
			line := buf.Bytes()
			line = bytes.TrimRight(line, "\r\n")
			if errors.Is(err, io.EOF) {
				if len(line) == 0 {
					return nil, io.EOF
				}
				return line, io.EOF
			}
			return nil, err
		}
		line := buf.Bytes()
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			return nil, nil
		}
		return line, nil
	}
}

// splitField parses "field: value" / "field:value" / "field:" lines.
// Per spec, only the first ':' separates the field name from the value;
// a leading space in the value is stripped.
func splitField(line []byte) (field, value []byte) {
	idx := bytes.IndexByte(line, ':')
	if idx < 0 {
		// Field with no value.
		return line, nil
	}
	field = line[:idx]
	value = line[idx+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

// formatSSE encodes evt as an SSE frame (event:/data: lines + blank line).
// Empty Type is omitted; data may contain newlines.
func formatSSE(evt sseEvent) []byte {
	var buf bytes.Buffer
	if evt.Type != "" {
		buf.Write(sseFieldEvent)
		buf.WriteString(": ")
		buf.WriteString(evt.Type)
		buf.WriteString("\n")
	}
	if len(evt.Data) > 0 {
		for _, line := range bytes.Split(evt.Data, []byte("\n")) {
			buf.Write(sseFieldData)
			buf.WriteString(": ")
			buf.Write(line)
			buf.WriteString("\n")
		}
	}
	if evt.ID != "" {
		buf.Write(sseFieldID)
		buf.WriteString(": ")
		buf.WriteString(evt.ID)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	return buf.Bytes()
}

// guard against unused-import in builds that trim helpers.
var _ = fmt.Sprintf
