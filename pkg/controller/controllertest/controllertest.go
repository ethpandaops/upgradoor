// Package controllertest holds the fakes and fixtures the tests of the
// controller's packages share: an in-memory store, a recording scheduler, a
// fixed beacon service, a settable clock, signed agent requests, a stand-in
// authenticatoor and the fleet of the authorization tests.
package controllertest

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
)

const (
	Network        = "testnet-devnet-0"
	ControllerHost = "upgradoor.testnet.example"
	BaseURL        = "https://" + ControllerHost
	Seed           = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var Epoch = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// Clock is a settable time source.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// NewClock returns a clock that stands at now.
func NewClock(now time.Time) *Clock {
	return &Clock{now: now}
}

// DecodeJSON decodes a recorded body strictly into T.
func DecodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	require.Equal(t, httpjson.ContentTypeJSON, rec.Header().Get("Content-Type"), rec.Body.String())

	var v T

	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v), rec.Body.String())

	return v
}

func MustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	return raw
}
