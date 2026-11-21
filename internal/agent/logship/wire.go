package logship

import (
	"bytes"
	"strconv"
	"time"
	"unicode/utf8"
)

// The per-line fields the sender sets (agent contract 11.3). The fields that
// say who sent a line (node_id, source, launch_id and the rest) are set by
// Potluck for the whole request; the agent never writes them.
//
// _time is RFC3339 with nanoseconds, in UTC: VictoriaLogs reads ISO8601 /
// RFC3339 with fractional seconds in its _time field, its default time
// field (https://docs.victoriametrics.com/victorialogs/data-ingestion/,
// "_time"; nanosecond precision since v1.9.0, the pinned store is v1.54.0).
// A line without _time gets the time at which the store took it.
const (
	fieldMsg        = "_msg"
	fieldTime       = "_time"
	fieldLevel      = "level"
	fieldUnit       = "unit"
	fieldStream     = "stream"
	fieldTruncated  = "truncated"
	fieldTimeSource = "time_source"

	// TimeSourceArrival marks a line whose time is when the agent read it,
	// not when it was written (a journal entry of an earlier boot).
	TimeSourceArrival = "arrival"
)

// FormatTime is the _time text of a Potluck time in Unix nanoseconds.
func FormatTime(unixNs int64) string {
	return time.Unix(0, unixNs).UTC().Format(time.RFC3339Nano)
}

// estimate is a cheap upper guess of a line's encoded size, used to fill a
// batch; the encoder cuts a line to maxLine, so no line counts more.
func estimate(maxLine int) func(*Line) int {
	return func(l *Line) int {
		n := len(l.Msg) + len(l.Level) + len(l.Unit) + len(l.Stream) + len(l.TimeSource) + 110
		if n > maxLine {
			return maxLine
		}
		return n
	}
}

// encodeLine appends one JSON line (with its newline) to buf, at most
// maxLine bytes with the newline. A message that does not fit is cut on a
// character boundary and the line gets "truncated": true: the store skips,
// it does not cut, a line over its own limit.
func encodeLine(buf *bytes.Buffer, l *Line, maxLine int) {
	tail := func(trunc bool) []byte {
		var t []byte
		if l.At != 0 {
			t = append(t, `,"`+fieldTime+`":"`...)
			t = append(t, FormatTime(l.At)...)
			t = append(t, '"')
		}
		for _, f := range [][2]string{{fieldLevel, l.Level}, {fieldUnit, l.Unit}, {fieldStream, l.Stream}, {fieldTimeSource, l.TimeSource}} {
			if f[1] != "" {
				t = append(t, `,"`+f[0]+`":`...)
				t = appendString(t, f[1], 512)
			}
		}
		if trunc || l.Truncated {
			t = append(t, `,"`+fieldTruncated+`":true`...)
		}
		return append(t, '}', '\n')
	}
	head := `{"` + fieldMsg + `":"`
	t := tail(false)
	budget := maxLine - len(head) - 1 - len(t)
	if escapedLen(l.Msg) > budget {
		t = tail(true)
		budget = maxLine - len(head) - 1 - len(t)
	}
	b := buf.AvailableBuffer()
	b = append(b, head...)
	b = appendEscaped(b, l.Msg, budget)
	b = append(b, '"')
	b = append(b, t...)
	buf.Write(b)
}

// appendString appends s as a JSON string, its content cut at max bytes.
func appendString(b []byte, s string, max int) []byte {
	b = append(b, '"')
	b = appendEscaped(b, s, max)
	return append(b, '"')
}

// escapedLen is the length of s inside a JSON string.
func escapedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += runeLen(r, size)
		i += size
	}
	return n
}

func runeLen(r rune, size int) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20:
		return 6
	case r == utf8.RuneError && size == 1:
		return 3 // written as U+FFFD
	}
	return size
}

// appendEscaped appends s escaped for a JSON string, stopping before the
// character that would pass max bytes.
func appendEscaped(b []byte, s string, max int) []byte {
	used := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n := runeLen(r, size)
		if used+n > max {
			break
		}
		used += n
		switch {
		case r == '"' || r == '\\':
			b = append(b, '\\', byte(r))
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r == '\t':
			b = append(b, '\\', 't')
		case r < 0x20:
			b = append(b, `\u00`...)
			h := strconv.FormatInt(int64(r), 16)
			if len(h) < 2 {
				b = append(b, '0')
			}
			b = append(b, h...)
		case r == utf8.RuneError && size == 1:
			b = utf8.AppendRune(b, utf8.RuneError)
		default:
			b = append(b, s[i:i+size]...)
		}
		i += size
	}
	return b
}

// cut shortens s to at most max bytes on a character boundary; ok is false
// when it had to.
func cut(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, true
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], false
}
