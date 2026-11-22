package validators

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

type memStore struct {
	mu     sync.Mutex
	ranges fleet.ValidatorRanges
	writes int
}

func (m *memStore) ValidatorRanges(context.Context) (*fleet.ValidatorRanges, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := m.ranges

	return &out, nil
}

func (m *memStore) SetValidatorRanges(_ context.Context, r *fleet.ValidatorRanges) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ranges = *r
	m.writes++

	return nil
}

func (m *memStore) get() (fleet.ValidatorRanges, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.ranges, m.writes
}

func quiet() logrus.FieldLogger {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	return log
}

func TestServiceFetchesAndKeepsTheLastGoodVersion(t *testing.T) {
	var (
		body    atomic.Pointer[string]
		status  atomic.Int32
		matched atomic.Int32
	)

	setBody := func(s string) { body.Store(&s) }

	setBody(`{"ranges": {"0-99": "node-a"}}`)
	status.Store(http.StatusOK)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := `"` + *body.Load() + `"`
		if r.Header.Get("If-None-Match") == etag {
			matched.Add(1)
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("ETag", etag)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(srv.Close)

	st := &memStore{ranges: fleet.ValidatorRanges{
		URL: "https://old.example", Ranges: []fleet.HostRange{hostRange("old", 0, 5)},
	}}

	var changes atomic.Int32

	svc := New(quiet(), Options{
		URL: srv.URL, Store: st, Interval: 10 * time.Millisecond, RetryInterval: 10 * time.Millisecond,
		Changed: func() { changes.Add(1) },
	})
	require.NoError(t, svc.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, svc.Stop()) })

	ranges := func() []fleet.HostRange {
		got, _ := st.get()

		return got.Ranges
	}

	require.Eventually(t, func() bool { return len(ranges()) == 1 && ranges()[0].Host == "node-a" },
		5*time.Second, 5*time.Millisecond, "the stored version of another URL is replaced")
	require.Eventually(t, func() bool { return matched.Load() > 1 }, 5*time.Second, 5*time.Millisecond,
		"later fetches are conditional")

	_, writes := st.get()
	assert.Equal(t, 2, writes, "the URL change and the first version")
	assert.Equal(t, int32(1), changes.Load())
	assert.Empty(t, svc.Error())

	// A broken document keeps the last good version.
	setBody(`{"ranges": {"0-10": "a", "5-20": "b"}}`)
	require.Eventually(t, func() bool { return svc.Error() != "" }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []fleet.HostRange{hostRange("node-a", 0, 100)}, ranges())

	// So does an answer that is no inventory.
	status.Store(http.StatusBadGateway)
	setBody(`{"ranges": {"0-99": "node-b"}}`)
	require.Eventually(t, func() bool { return svc.Error() == "HTTP 502 Bad Gateway" },
		5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []fleet.HostRange{hostRange("node-a", 0, 100)}, ranges())

	status.Store(http.StatusOK)
	require.Eventually(t, func() bool { return svc.Error() == "" && ranges()[0].Host == "node-b" },
		5*time.Second, 5*time.Millisecond)
	assert.Equal(t, int32(2), changes.Load())
}

func TestServiceWithoutURLAssignsNothing(t *testing.T) {
	st := &memStore{ranges: fleet.ValidatorRanges{
		URL: "https://old.example", Ranges: []fleet.HostRange{hostRange("old", 0, 5)},
	}}

	svc := New(quiet(), Options{Store: st})
	require.NoError(t, svc.Start(context.Background()))
	require.NoError(t, svc.Stop())

	got, _ := st.get()
	assert.Equal(t, fleet.ValidatorRanges{}, got)
	assert.Empty(t, svc.Error())
}

func TestServiceStatusAndRefresh(t *testing.T) {
	var body atomic.Pointer[string]

	setBody := func(s string) { body.Store(&s) }
	setBody(`{"ranges": {"0-99": "node-a"}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2026 03:04:05 GMT")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(srv.Close)

	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	var offset atomic.Int64

	now := func() time.Time { return start.Add(time.Duration(offset.Load())) }
	url := strings.Replace(srv.URL, "http://", "http://user:secret@", 1) + "/ranges.json"

	svc := New(quiet(), Options{URL: url, Store: &memStore{}, Interval: time.Hour, Now: now})
	assert.Equal(t, "the validator ranges service is not running", svc.Refresh(t.Context()).Reason)

	require.NoError(t, svc.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, svc.Stop()) })
	require.Eventually(t, func() bool { return !svc.Status().SuccessAt.IsZero() }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, Status{
		URL: srv.URL + "/ranges.json", AttemptAt: start, SuccessAt: start, ChangedAt: start,
		ETag: `"v1"`, LastModified: "Mon, 02 Jan 2026 03:04:05 GMT",
		Ranges: []fleet.HostRange{hostRange("node-a", 0, 100)},
	}, svc.Status())

	// Within the floor nothing is fetched.
	offset.Store(int64(RefreshFloor - time.Second))
	assert.Equal(t, Refreshed{Reason: "fetched within the last 10s", Next: start.Add(RefreshFloor)},
		svc.Refresh(t.Context()))

	offset.Store(int64(RefreshFloor))
	setBody(`{"ranges": {"0-49": "node-a", "50-99": "node-b"}}`)
	assert.Equal(t, Refreshed{Fetched: true, Changed: true}, svc.Refresh(t.Context()))

	got := svc.Status()
	assert.Equal(t, start.Add(RefreshFloor), got.ChangedAt)
	assert.Len(t, got.Ranges, 2)

	// Unchanged: the change time stays.
	offset.Store(int64(2 * RefreshFloor))
	assert.Equal(t, Refreshed{Fetched: true}, svc.Refresh(t.Context()))
	assert.Equal(t, start.Add(RefreshFloor), svc.Status().ChangedAt)
	assert.Equal(t, start.Add(2*RefreshFloor), svc.Status().SuccessAt)

	// A failed fetch keeps the ranges and never quotes the credentials.
	srv.Close()
	offset.Store(int64(3 * RefreshFloor))

	done := svc.Refresh(t.Context())
	assert.True(t, done.Fetched)
	assert.NotEmpty(t, done.Error)
	assert.NotContains(t, done.Error, "secret")

	got = svc.Status()
	assert.Equal(t, done.Error, got.Error)
	assert.Equal(t, start.Add(3*RefreshFloor), got.AttemptAt)
	assert.Equal(t, start.Add(2*RefreshFloor), got.SuccessAt)
	assert.Len(t, got.Ranges, 2)
}

func TestServiceRefreshWithoutURL(t *testing.T) {
	svc := New(quiet(), Options{Store: &memStore{}})
	require.NoError(t, svc.Start(context.Background()))
	require.NoError(t, svc.Stop())

	assert.Equal(t, Refreshed{Reason: "no validator ranges URL is configured"}, svc.Refresh(t.Context()))
	assert.Empty(t, svc.Status().URL)
}
