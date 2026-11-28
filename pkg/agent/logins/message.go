package logins

import (
	"regexp"
	"strconv"
	"strings"
)

// eventKind is what an sshd log line says.
type eventKind int

const (
	eventNone eventKind = iota
	// eventLogin: a session was accepted.
	eventLogin
	// eventEndAddr: the connection of an address and port ended.
	eventEndAddr
	// eventEndProcess: the session of the logging process closed.
	eventEndProcess
)

// event is one parsed sshd log line.
type event struct {
	kind   eventKind
	method string
	user   string
	from   string
	port   int
	// keyType and fingerprint are set for a key login.
	keyType     string
	fingerprint string
}

var (
	// Accepted publickey for devops from 10.0.0.1 port 51234 ssh2: ED25519 SHA256:...
	acceptedLine = regexp.MustCompile(
		`^Accepted (\S+) for (\S+) from (\S+) port (\d+)(?: ssh2)?(?:: (\S+) (SHA256:\S+))?`)
	// Disconnected from user devops 10.0.0.1 port 51234
	// Received disconnect from 10.0.0.1 port 51234:11: disconnected by user
	// Connection closed by 10.0.0.1 port 51234
	// Connection reset by 10.0.0.1 port 51234
	// Timeout, client not responding from user devops 10.0.0.1 port 51234
	endLine = regexp.MustCompile(
		`^(?:Disconnected from user \S+|Received disconnect from|Connection (?:closed|reset) by|` +
			`Timeout, client not responding from user \S+) (\S+) port (\d+)`)
	// pam_unix(sshd:session): session closed for user devops
	sessionClosedLine = regexp.MustCompile(`^pam_unix\(sshd:session\): session closed for user (\S+)`)
)

// parseMessage reads one sshd log line. Lines of failed and unfinished
// authentications ([preauth]) say nothing of a session.
func parseMessage(msg string) event {
	msg = strings.TrimSpace(msg)
	if strings.HasSuffix(msg, "[preauth]") {
		return event{}
	}

	if m := acceptedLine.FindStringSubmatch(msg); m != nil {
		port, err := strconv.Atoi(m[4])
		if err != nil {
			return event{}
		}

		return event{
			kind: eventLogin, method: m[1], user: m[2], from: m[3], port: port,
			keyType: m[5], fingerprint: m[6],
		}
	}

	if m := endLine.FindStringSubmatch(msg); m != nil {
		port, err := strconv.Atoi(m[2])
		if err != nil {
			return event{}
		}

		return event{kind: eventEndAddr, from: m[1], port: port}
	}

	if m := sessionClosedLine.FindStringSubmatch(msg); m != nil {
		return event{kind: eventEndProcess, user: m[1]}
	}

	return event{}
}
