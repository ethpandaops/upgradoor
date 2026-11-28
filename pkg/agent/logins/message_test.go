package logins

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseMessage(t *testing.T) {
	t.Parallel()

	const fp = "SHA256:FZ4MZgjwAfMUDndwBOzqL8RsJ7K+WPJyrUUxRTFDINc"

	for msg, want := range map[string]event{
		"Accepted publickey for devops from 10.0.0.5 port 50001 ssh2: ED25519 " + fp: {
			kind: eventLogin, method: "publickey", user: "devops", from: "10.0.0.5", port: 50001,
			keyType: "ED25519", fingerprint: fp,
		},
		"Accepted publickey for devops from 2001:db8::1 port 22 ssh2: ED25519-CERT " + fp +
			" ID ops (serial 4) CA ED25519 SHA256:other": {
			kind: eventLogin, method: "publickey", user: "devops", from: "2001:db8::1", port: 22,
			keyType: "ED25519-CERT", fingerprint: fp,
		},
		"Accepted password for alice from 192.168.1.9 port 40000 ssh2": {
			kind: eventLogin, method: "password", user: "alice", from: "192.168.1.9", port: 40000,
		},
		"Accepted keyboard-interactive/pam for bob from 10.1.1.1 port 3 ssh2": {
			kind: eventLogin, method: "keyboard-interactive/pam", user: "bob", from: "10.1.1.1", port: 3,
		},
		"Disconnected from user devops 10.0.0.5 port 50001":                      {kind: eventEndAddr, from: "10.0.0.5", port: 50001},
		"Received disconnect from 10.0.0.5 port 50001:11: disconnected by user":  {kind: eventEndAddr, from: "10.0.0.5", port: 50001},
		"Connection closed by 10.0.0.5 port 50002":                               {kind: eventEndAddr, from: "10.0.0.5", port: 50002},
		"Connection reset by 10.0.0.5 port 50003":                                {kind: eventEndAddr, from: "10.0.0.5", port: 50003},
		"Timeout, client not responding from user devops 203.0.113.9 port 52000": {kind: eventEndAddr, from: "203.0.113.9", port: 52000},
		"pam_unix(sshd:session): session closed for user devops":                 {kind: eventEndProcess, user: "devops"},

		"pam_unix(sshd:session): session opened for user devops(uid=1000) by (uid=0)":     {},
		"Connection closed by authenticating user root 198.51.100.4 port 4444 [preauth]":  {},
		"Failed publickey for root from 198.51.100.4 port 4444 ssh2: RSA SHA256:x":        {},
		"Invalid user admin from 198.51.100.4 port 5555":                                  {},
		"Disconnected from authenticating user root 198.51.100.4 port 4444 [preauth]":     {},
		"Received disconnect from 198.51.100.4 port 4444:11: Bye Bye [preauth]":           {},
		"Server listening on 0.0.0.0 port 22.":                                            {},
		"Accepted publickey for devops from 10.0.0.5 port notanumber ssh2: ED25519 " + fp: {},
	} {
		assert.Equal(t, want, parseMessage(msg), msg)
	}
}
