package identity

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClock is a settable time source.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = t
}

// sealTo seals a body to key and reports what opening it in keyring gives.
func sealTo(t *testing.T, keyring *Keyring, key *SealKey) error {
	t.Helper()

	wire, _, err := SealRequest(key, KeyID{}, testBinding(), []byte("body"))
	require.NoError(t, err)

	req, err := ParseSealedRequest(wire)
	require.NoError(t, err)

	_, _, err = keyring.Open(req, testBinding())

	return err
}

func announcedIDs(t *testing.T, keyring *Keyring) []KeyID {
	t.Helper()

	keys, err := keyring.Announced()
	require.NoError(t, err)

	out := make([]KeyID, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.ID)
	}

	return out
}

// Key i is current for one period; the next is announced a sixth of a
// period ahead, the previous accepted for one period after.
func TestKeyringRotation(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	clk := &testClock{t: start.Add(time.Minute)}
	keyring := NewKeyring(WithKeyringClock(clk.now))

	keys, err := keyring.Announced()
	require.NoError(t, err)
	require.Len(t, keys, 1)

	first := keys[0]
	assert.True(t, first.ActiveFrom.Equal(start))
	assert.Equal(t, KeyIDSize*2, len(first.ID.String()))

	up, err := keyring.UpToDate(first.ID, KeyID{})
	require.NoError(t, err)
	assert.True(t, up)

	// 49 minutes in: no next key yet.
	clk.set(start.Add(49 * time.Minute))
	assert.Equal(t, []KeyID{first.ID}, announcedIDs(t, keyring))

	// 50 minutes in: the next key is announced and accepted.
	clk.set(start.Add(50 * time.Minute))

	keys, err = keyring.Announced()
	require.NoError(t, err)
	require.Len(t, keys, 2)

	next := keys[1]
	assert.True(t, next.ActiveFrom.Equal(start.Add(time.Hour)))
	require.NoError(t, sealTo(t, keyring, next), "a sender ahead of the clock")

	for _, tt := range []struct {
		used, known KeyID
		want        bool
	}{
		{used: first.ID, known: KeyID{}, want: false},
		{used: first.ID, known: next.ID, want: true},
		{used: next.ID, known: first.ID, want: false},
	} {
		up, err = keyring.UpToDate(tt.used, tt.known)
		require.NoError(t, err)
		assert.Equal(t, tt.want, up)
	}

	// Rotated: next is current, first still opens.
	clk.set(start.Add(time.Hour))
	assert.Equal(t, []KeyID{next.ID}, announcedIDs(t, keyring))
	require.NoError(t, sealTo(t, keyring, first))

	up, err = keyring.UpToDate(next.ID, first.ID)
	require.NoError(t, err)
	assert.True(t, up)

	up, err = keyring.UpToDate(first.ID, next.ID)
	require.NoError(t, err)
	assert.False(t, up)

	// One more period: first is gone.
	clk.set(start.Add(2*time.Hour - time.Nanosecond))
	require.NoError(t, sealTo(t, keyring, first))

	clk.set(start.Add(2 * time.Hour))
	require.ErrorIs(t, sealTo(t, keyring, first), ErrUnknownSealKey)
	require.NoError(t, sealTo(t, keyring, next))

	// A long silence: every earlier key is gone.
	clk.set(start.Add(10 * time.Hour))
	require.ErrorIs(t, sealTo(t, keyring, next), ErrUnknownSealKey)
	assert.Len(t, announcedIDs(t, keyring), 1)
}

func TestKeyringPeriod(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	clk := &testClock{t: start}
	keyring := NewKeyring(WithKeyringClock(clk.now), WithKeyPeriod(time.Minute))

	first := announcedIDs(t, keyring)[0]

	clk.set(start.Add(50 * time.Second))
	assert.Len(t, announcedIDs(t, keyring), 2)

	clk.set(start.Add(2 * time.Minute))
	assert.NotContains(t, announcedIDs(t, keyring), first)
}

// A restarted controller holds none of its earlier keys.
func TestKeyringIsNotPersisted(t *testing.T) {
	t.Parallel()

	key := currentKey(t, NewKeyring())

	require.ErrorIs(t, sealTo(t, NewKeyring(), key), ErrUnknownSealKey)
}

// syncBody is a gzipped sync body of a host with a few dozen containers.
func syncBody(b *testing.B) []byte {
	b.Helper()

	containers := make([]map[string]any, 0, 40)
	for i := range 40 {
		containers = append(containers, map[string]any{
			"name": fmt.Sprintf("container-%d", i), "image": "ethpandaops/geth:master",
			"imageId": fmt.Sprintf("sha256:%064d", i), "managed": true, "kind": "el",
			"state": "running", "startedAt": "2026-11-14T12:00:00Z",
		})
	}

	raw, err := json.Marshal(map[string]any{"protocolVersion": 1, "containers": containers})
	require.NoError(b, err)

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, err = zw.Write(raw)
	require.NoError(b, err)
	require.NoError(b, zw.Close())

	return buf.Bytes()
}

// BenchmarkOpen is the controller's work for a sealed sync: parse, X-Wing
// decapsulation, decryption, and sealing the answer.
func BenchmarkOpen(b *testing.B) {
	keyring := NewKeyring()

	keys, err := keyring.Announced()
	require.NoError(b, err)

	body := syncBody(b)

	wire, _, err := SealRequest(keys[0], KeyID{}, testBinding(), body)
	require.NoError(b, err)

	answer := bytes.Repeat([]byte("a"), 2048)

	b.SetBytes(int64(len(wire)))
	b.ReportAllocs()

	for b.Loop() {
		req, perr := ParseSealedRequest(wire)
		if perr != nil {
			b.Fatal(perr)
		}

		_, seal, oerr := keyring.Open(req, testBinding())
		if oerr != nil {
			b.Fatal(oerr)
		}

		_ = seal.Seal(answer)
	}
}
