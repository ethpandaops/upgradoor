package identity

import (
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testControllerSeed is a controller seed: another secret than testSeed.
const testControllerSeed = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func controllerKey(t *testing.T) *mldsa.PrivateKey {
	t.Helper()

	key, err := DeriveControllerKey([]byte(testControllerSeed))
	require.NoError(t, err)

	return key
}

func TestAnswerEncoding(t *testing.T) {
	key := controllerKey(t)

	signer, err := NewAnswerSigner(key, "net")
	require.NoError(t, err)

	envelope, err := signer.Sign("host-1", "request-1", []byte(`{"a":1}`))
	require.NoError(t, err)

	var signed SignedAnswer
	require.NoError(t, json.Unmarshal(envelope, &signed))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString([]byte(`{"a":1}`)), signed.Payload)

	parts := strings.Split(signed.Signature, ".")
	require.Len(t, parts, 3)

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	assert.Equal(t, `{"alg":"ML-DSA-44","typ":"upgradoor-answer+jwt"}`, string(header))

	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	assert.Equal(t, `{"net":"net","sub":"host-1","jti":"request-1","bh":"`+
		BodyHash([]byte(`{"a":1}`))+`"}`, string(claims))

	// The envelope is small enough to be no concern for any proxy: it
	// travels in the body, where a signature costs about 3.3 KB.
	assert.Less(t, len(envelope), 4096)

	_, err = signer.Sign("", "request-1", nil)
	require.Error(t, err)
	_, err = signer.Sign("host-1", "", nil)
	require.Error(t, err)
}

func TestAnswerVerify(t *testing.T) {
	const (
		host    = "host-1"
		request = "request-1"
		body    = `{"lease":{"id":"l1"}}`
	)

	key := controllerKey(t)
	pub := PublicKey(key)

	signer, err := NewAnswerSigner(key, "net")
	require.NoError(t, err)

	signedBy := func(t *testing.T, s *AnswerSigner, subject, id, payload string) []byte {
		t.Helper()

		out, serr := s.Sign(subject, id, []byte(payload))
		require.NoError(t, serr)

		return out
	}

	good := signedBy(t, signer, host, request, body)

	otherSeed, err := DeriveControllerKey([]byte(testSeed))
	require.NoError(t, err)
	otherKey, err := NewAnswerSigner(otherSeed, "net")
	require.NoError(t, err)

	otherNet, err := NewAnswerSigner(key, "net2")
	require.NoError(t, err)

	// A token of the same key that is not an answer: a proof-shaped JWS.
	foreignType, err := signCompact(key, proofHeader, []byte(`{"net":"net","sub":"host-1",`+
		`"jti":"request-1","bh":"`+BodyHash([]byte(body))+`"}`))
	require.NoError(t, err)

	reencode := func(t *testing.T, mutate func(*SignedAnswer)) []byte {
		t.Helper()

		var signed SignedAnswer
		require.NoError(t, json.Unmarshal(good, &signed))
		mutate(&signed)

		out, merr := json.Marshal(signed)
		require.NoError(t, merr)

		return out
	}

	verifier, err := NewAnswerVerifier(pub, "net", host)
	require.NoError(t, err)

	got, err := verifier.Verify(request, good)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))

	tests := []struct {
		name    string
		request string
		answer  []byte
		reason  string
	}{
		{
			name: "replayed for a later request", request: "request-2", answer: good,
			reason: "answers another request",
		},
		{
			name: "answer for another host", request: request,
			answer: signedBy(t, signer, "host-2", request, body),
			reason: "for another host",
		},
		{
			name: "answer of another network's controller (same key)", request: request,
			answer: signedBy(t, otherNet, host, request, body),
			reason: "for another network",
		},
		{
			name: "body altered in transit", request: request,
			answer: reencode(t, func(s *SignedAnswer) {
				s.Payload = base64.RawURLEncoding.EncodeToString([]byte(`{"lease":{"id":"l2"}}`))
			}),
			reason: "does not match its signature",
		},
		{
			name: "body removed", request: request,
			answer: reencode(t, func(s *SignedAnswer) { s.Payload = "" }),
			reason: "does not match its signature",
		},
		{
			name: "signed by another controller key", request: request,
			answer: signedBy(t, otherKey, host, request, body),
			reason: "not signed by the controller key",
		},
		{
			name: "signature altered", request: request,
			answer: reencode(t, func(s *SignedAnswer) {
				s.Signature = s.Signature[:len(s.Signature)-4] + "AAAA"
			}),
			reason: "not signed by the controller key",
		},
		{
			name: "a token of the key that is no answer", request: request,
			answer: reencode(t, func(s *SignedAnswer) { s.Signature = foreignType }),
			reason: "not signed by the controller key",
		},
		{
			name: "unsigned answer", request: request, answer: []byte(body),
			reason: "not signed",
		},
		{
			name: "unsigned error body", request: request,
			answer: []byte(`{"error":"x","serverTime":"2027-01-15T08:00:00Z"}`),
			reason: "not signed",
		},
		{name: "empty answer", request: request, answer: nil, reason: "not signed"},
		{name: "no request id", request: "", answer: good, reason: "no request id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := verifier.Verify(tt.request, tt.answer)
			require.ErrorIs(t, err, ErrBadAnswer)
			assert.Contains(t, err.Error(), tt.reason)
			assert.Nil(t, out, "nothing of a refused answer is handed out")
		})
	}

	t.Run("a verifier needs a key", func(t *testing.T) {
		_, err := NewAnswerVerifier(nil, "net", host)
		require.ErrorIs(t, err, ErrInvalidKey)

		_, err = NewAnswerSigner(nil, "net")
		require.ErrorIs(t, err, ErrInvalidKey)
	})
}
