package identity

import (
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const (
	infoControllerPrefix = "upgradoor/controller/"

	// answerHeader is the fixed JOSE header of an answer signature.
	answerHeader = `{"alg":"` + Algorithm + `","typ":"upgradoor-answer+jwt"}`
)

// ErrBadAnswer is returned for a controller answer that is not signed, not
// signed by an accepted controller key, or not the answer to the request it
// is checked against. An agent acts on nothing of such an answer.
var ErrBadAnswer = errors.New("identity: controller answer refused")

// DeriveControllerKey derives the controller key of one network, with which
// that network's controller signs its answers to agents.
//
// controllerSeed is the controller seed: the secret of one group of devnets
// from which the controller of each of its networks derives its own key at
// start, and from which whoever issues the agents' configuration derives the
// public half. It is not the devnet seed the signer and host keys come from.
// The network name in the derivation gives every devnet of the group its
// own key. Like the devnet seed it is used as raw bytes and must be at least
// MinSeedLength long. generation rotates the key of one network without
// touching the seed.
func DeriveControllerKey(
	controllerSeed []byte, network string, generation uint64,
) (*mldsa.PrivateKey, error) {
	if err := ValidateName(network); err != nil {
		return nil, err
	}

	return derive(controllerSeed,
		infoControllerPrefix+network+"/"+strconv.FormatUint(generation, 10))
}

// KeyFingerprint is the lower-case hex SHA-256 of a public key's bytes. It
// names a key in logs, where the key itself (1750 characters) is too long.
func KeyFingerprint(key *mldsa.PublicKey) string {
	sum := sha256.Sum256(key.Bytes())

	return hex.EncodeToString(sum[:])
}

// AnswerClaims are the claims of an answer signature. The field order is the
// canonical member order of the encoded payload.
type AnswerClaims struct {
	// Network is the network of the controller.
	Network string `json:"net"`
	// Subject is the host the answer is for: the subject of the certificate
	// the request was sent with.
	Subject string `json:"sub"`
	// RequestID is the jti of the request proof this answers.
	RequestID string `json:"jti"`
	// BodyHash is BodyHash of the answer bytes.
	BodyHash string `json:"bh"`
}

// SignedAnswer is the body of a signed controller answer.
type SignedAnswer struct {
	// Payload is the answer, the JSON the endpoint would send unsigned, as
	// unpadded base64url. The signature is over these exact bytes, so nothing
	// between controller and agent can change them unnoticed and nothing has
	// to be canonicalized.
	Payload string `json:"payload"`
	// Signature is a compact JWS over AnswerClaims, signed with the
	// controller key.
	Signature string `json:"signature"`
}

// SignAnswer signs claims with the controller key. Signer.Sign is what a
// controller uses.
func SignAnswer(key *mldsa.PrivateKey, claims *AnswerClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("identity: encode answer claims: %w", err)
	}

	return signCompact(key, answerHeader, payload)
}

// AnswerSigner signs a controller's answers to the agents of one network.
type AnswerSigner struct {
	key     *mldsa.PrivateKey
	network string
}

// NewAnswerSigner returns a signer for the answers of network's controller.
func NewAnswerSigner(key *mldsa.PrivateKey, network string) (*AnswerSigner, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: no controller key", ErrInvalidKey)
	}

	if err := ValidateName(network); err != nil {
		return nil, err
	}

	return &AnswerSigner{key: key, network: network}, nil
}

// PublicKey returns the public half of the controller key.
func (s *AnswerSigner) PublicKey() *mldsa.PublicKey { return s.key.PublicKey() }

// Sign wraps body, the answer to the request of host subject whose proof had
// the id requestID, into an encoded SignedAnswer.
func (s *AnswerSigner) Sign(subject, requestID string, body []byte) ([]byte, error) {
	if subject == "" || requestID == "" {
		return nil, errors.New("identity: an answer needs a host and a request id")
	}

	sig, err := SignAnswer(s.key, &AnswerClaims{
		Network:   s.network,
		Subject:   subject,
		RequestID: requestID,
		BodyHash:  BodyHash(body),
	})
	if err != nil {
		return nil, err
	}

	out, err := json.Marshal(SignedAnswer{
		Payload:   base64.RawURLEncoding.EncodeToString(body),
		Signature: sig,
	})
	if err != nil {
		return nil, fmt.Errorf("identity: encode signed answer: %w", err)
	}

	return out, nil
}

// IsSignedAnswer reports whether answer, the body of a controller answer, is
// a SignedAnswer. It checks nothing.
func IsSignedAnswer(answer []byte) bool {
	var signed SignedAnswer

	return json.Unmarshal(answer, &signed) == nil && signed.Signature != ""
}

// AnswerVerifier checks controller answers on behalf of one host.
type AnswerVerifier struct {
	keys    []*mldsa.PublicKey
	network string
	subject string
}

// NewAnswerVerifier returns a verifier for the answers the host subject of
// network gets. keys are the accepted controller keys: more than one while a
// controller key is rotated.
func NewAnswerVerifier(
	keys []*mldsa.PublicKey, network, subject string,
) (*AnswerVerifier, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no controller public key", ErrInvalidKey)
	}

	for _, key := range keys {
		if key == nil {
			return nil, fmt.Errorf("%w: empty controller public key", ErrInvalidKey)
		}
	}

	if network == "" || subject == "" {
		return nil, errors.New("identity: an answer verifier needs a network and a host")
	}

	return &AnswerVerifier{keys: keys, network: network, subject: subject}, nil
}

// Verify checks answer, the body of a controller answer, against the request
// whose proof had the id requestID, and returns the answer it carries. The
// answer must be a SignedAnswer, signed by one of the accepted keys, for
// this network, this host and this request, over exactly the returned bytes.
// Every failure is an ErrBadAnswer.
func (v *AnswerVerifier) Verify(requestID string, answer []byte) ([]byte, error) {
	if requestID == "" {
		return nil, fmt.Errorf("%w: no request id to check it against", ErrBadAnswer)
	}

	var signed SignedAnswer

	if err := json.Unmarshal(answer, &signed); err != nil || signed.Signature == "" {
		return nil, fmt.Errorf("%w: it is not signed", ErrBadAnswer)
	}

	var (
		raw []byte
		err error
	)

	for _, key := range v.keys {
		if raw, err = verifyCompact(key, answerHeader, signed.Signature); err == nil {
			break
		}
	}

	if err != nil {
		return nil, fmt.Errorf(
			"%w: not signed by an accepted controller key (%w)", ErrBadAnswer, err)
	}

	var claims AnswerClaims
	if err = json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrBadAnswer, err)
	}

	switch {
	case claims.Network != v.network:
		return nil, fmt.Errorf("%w: it is for another network", ErrBadAnswer)
	case claims.Subject != v.subject:
		return nil, fmt.Errorf("%w: it is for another host", ErrBadAnswer)
	case claims.RequestID != requestID:
		return nil, fmt.Errorf("%w: it answers another request", ErrBadAnswer)
	}

	body, err := base64.RawURLEncoding.DecodeString(signed.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed payload", ErrBadAnswer)
	}

	if claims.BodyHash != BodyHash(body) {
		return nil, fmt.Errorf("%w: the body does not match its signature", ErrBadAnswer)
	}

	return body, nil
}
