// Package logins turns the sshd entries of the journal into the host's SSH
// login history: sessions paired by process and address, grouped by
// identity, login user and source address, queued until the controller
// acknowledges them. It does no I/O and reads no clock: the journal, the
// key map, the process table and the time are its arguments.
package logins

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Entry is one journal entry of sshd.
type Entry struct {
	Cursor string
	Time   time.Time
	Boot   string
	PID    int
	// Message is the log line without the identifier.
	Message string
}

// journalLine is one line of journalctl --output=json.
//
//nolint:tagliatelle // the journal's field names
type journalLine struct {
	Cursor    string          `json:"__CURSOR"`
	Realtime  string          `json:"__REALTIME_TIMESTAMP"`
	Boot      string          `json:"_BOOT_ID"`
	PID       string          `json:"_PID"`
	SyslogPID string          `json:"SYSLOG_PID"`
	Message   json.RawMessage `json:"MESSAGE"`
}

// maxJournalLine bounds one line of the journal output.
const maxJournalLine = 1 << 20

// ParseJournal reads the output of journalctl --output=json: one object per
// line. Lines without a cursor, a time or a message are skipped.
func ParseJournal(raw []byte) ([]Entry, error) {
	out := make([]Entry, 0, bytes.Count(raw, []byte{'\n'})+1)

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), maxJournalLine)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var j journalLine
		if err := json.Unmarshal(line, &j); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}

		micros, err := strconv.ParseInt(j.Realtime, 10, 64)
		if err != nil || j.Cursor == "" {
			continue
		}

		msg, ok := message(j.Message)
		if !ok {
			continue
		}

		pid, _ := strconv.Atoi(j.PID)
		if pid == 0 {
			pid, _ = strconv.Atoi(j.SyslogPID)
		}

		out = append(out, Entry{
			Cursor: j.Cursor, Time: time.UnixMicro(micros).UTC(), Boot: j.Boot, PID: pid, Message: msg,
		})
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}

	return out, nil
}

// message decodes MESSAGE: a string, or an array of bytes for a message
// that is not valid UTF-8.
func message(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}

	var b []byte
	if json.Unmarshal(raw, &b) == nil {
		return string(b), true
	}

	var ints []int

	if json.Unmarshal(raw, &ints) != nil {
		return "", false
	}

	buf := make([]byte, 0, len(ints))
	for _, v := range ints {
		buf = append(buf, byte(v))
	}

	return string(buf), true
}
