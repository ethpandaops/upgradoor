package protocol

import "time"

const (
	// PathHello is the endpoint (POST) that answers the controller's
	// encryption keys. Its request carries no host state.
	PathHello = "/api/agent/v1/hello"

	// ContentTypeSealed marks a sealed body (pkg/identity): a request to
	// PathSync or PathInventory, or the answer to one.
	ContentTypeSealed = "application/vnd.upgradoor.sealed"

	// MaxHelloBody bounds the accepted hello body size.
	MaxHelloBody = 4 << 10
)

// EncryptionKey is a public encryption key of the controller.
type EncryptionKey struct {
	// ID names the key: lower-case hex of the first 8 bytes of the SHA-256
	// of PublicKey's bytes.
	ID string `json:"id"`
	// PublicKey is the MLKEM768-X25519 public key, unpadded base64url.
	PublicKey string `json:"publicKey"`
	// ActiveFrom is when the controller makes it its current key; an agent
	// seals to the newest key active by its clock.
	ActiveFrom time.Time `json:"activeFrom"`
}

// HelloRequest is the body of POST /api/agent/v1/hello.
type HelloRequest struct{}

// HelloResponse is the answer to a hello.
type HelloResponse struct {
	ServerTime time.Time `json:"serverTime"`
	// Keys are the current key and, when announced, the next one.
	Keys []EncryptionKey `json:"keys"`
}

// RekeyResponse is the answer to a sealed request to a key the controller
// does not hold: unsealed, and nothing of the request was processed. The
// agent takes Keys and sends the request again.
type RekeyResponse struct {
	ServerTime time.Time       `json:"serverTime"`
	Rekey      bool            `json:"rekey"`
	Keys       []EncryptionKey `json:"keys"`
}
