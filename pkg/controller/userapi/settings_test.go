package userapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// TestSettingsKeysAreExact: a key is a setting's name byte for byte, once.
// Go's decoder would take "MinPeers" for "minPeers" and the last of two
// spellings; a body cannot name a setting in a way the audit log and a
// reviewer would not recognise.
func TestSettingsKeysAreExact(t *testing.T) {
	for name, tt := range map[string]struct {
		body     string
		contains string
	}{
		"another case":            {`{"MinPeers":7}`, `unknown setting "MinPeers"`},
		"upper case":              {`{"PAUSED":true}`, `unknown setting "PAUSED"`},
		"two spellings":           {`{"minPeers":4,"MinPeers":7}`, `unknown setting "MinPeers"`},
		"two spellings, reversed": {`{"minpeers":7,"minPeers":4}`, `unknown setting "minpeers"`},
		"the same key twice":      {`{"minPeers":4,"minPeers":7}`, "minPeers is named twice"},
		"twice with one value":    {`{"paused":true,"paused":true}`, "paused is named twice"},
		"a key with a blank":      {`{"minPeers ":7}`, `unknown setting "minPeers "`},
		"the Go field name":       {`{"ReadyPeerShare":0.5}`, `unknown setting "ReadyPeerShare"`},
		"an unknown key":          {`{"minPeers":7,"nosuch":1}`, `unknown setting "nosuch"`},
		"a share too small":       {`{"readyPeerShare":1e-400}`, "readyPeerShare: 1e-400 is too small to be told from 0"},
		"a percent too small":     {`{"maxUnavailablePercent":0.1E-999}`, "is too small to be told from 0"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)

			rec := h.sendJSON(http.MethodPut, "/api/v1/settings", tt.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error, tt.contains)
			assert.Empty(t, h.sched.Recorded())
			assert.Equal(t, fleet.DefaultSettings(), controllertest.DecodeJSON[views.SettingsBody](t, h.get("/api/v1/settings")).Settings)
		})
	}

	// What is no underflow: zero in any spelling, and small values that are
	// values.
	h := newHarness(t, nil)

	for _, body := range []string{
		`{"readyPeerShare":0}`, `{"readyPeerShare":0.0}`, `{"readyPeerShare":0e10}`, `{"readyPeerShare":-0}`,
		`{"readyPeerShare":1e-9}`, `{"readyPeerShare":0.25}`,
	} {
		rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", body, rec.Body.String())
	}

	assert.False(t, underflows(`"1e-400"`))
	assert.False(t, underflows(`true`))
	assert.True(t, underflows(`1E-400`))
	assert.True(t, underflows(`0.00001e-400`))
}

// realScheduler is a Server over the real store and scheduler, open.
func realScheduler(t *testing.T) (*harness, store.Store) {
	t.Helper()

	st := store.NewMemory(quietLogger())
	require.NoError(t, st.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, st.Stop()) })

	h := newHarness(t, func(o *Options) {
		o.Store = st
		o.Scheduler = scheduler.New(quietLogger(), scheduler.Options{
			Store:    st,
			Health:   func() fleet.NetworkHealth { return fleet.NetworkHealth{} },
			Defaults: fleet.DefaultSettings(),
		})
	})
	h.start()

	return h, st
}

// TestSettingsPutIsOneStep: a PUT changes what it names on the settings as
// they are when it is applied, not as they were when the request began.
func TestSettingsPutIsOneStep(t *testing.T) {
	ctx := context.Background()

	settings := func(h *harness) fleet.Settings {
		return controllertest.DecodeJSON[views.SettingsBody](t, h.get("/api/v1/settings")).Settings
	}

	updates := func(st store.Store) []fleet.AuditEntry {
		entries, err := st.ListAudit(ctx, store.AuditFilter{Target: "settings"})
		require.NoError(t, err)

		return entries
	}

	t.Run("a pause that lands while the body is on its way survives", func(t *testing.T) {
		h, st := realScheduler(t)

		// The request is under way: its body has not arrived yet.
		body, feed := io.Pipe()
		req := httptest.NewRequest(http.MethodPut, controllertest.BaseURL+"/api/v1/settings", body)
		req.Header.Set("Content-Type", "application/json")

		done := make(chan *httptest.ResponseRecorder, 1)

		go func() {
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)
			done <- rec
		}()

		_, err := feed.Write([]byte(`{"minPe`))
		require.NoError(t, err)

		// Give the handler the time to do whatever it does before it reads
		// the body to its end.
		time.Sleep(100 * time.Millisecond)

		// Meanwhile: the global pause, and another PUT.
		rec := h.sendJSON(http.MethodPost, "/api/v1/actions/pause", `{"reason":"incident"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = h.sendJSON(http.MethodPut, "/api/v1/settings", `{"readyPeerShare":0.25,"pauseAfterCanaryImage":true}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		_, err = feed.Write([]byte(`ers":7}`))
		require.NoError(t, err)
		require.NoError(t, feed.Close())

		first := <-done
		require.Equal(t, http.StatusOK, first.Code, first.Body.String())

		want := fleet.DefaultSettings()
		want.MinPeers = 7
		want.Paused = true
		want.ReadyPeerShare = 0.25
		want.PauseAfterCanaryImage = true

		assert.Equal(t, want, controllertest.DecodeJSON[views.SettingsBody](t, first).Settings, "the answer is the whole set")
		assert.Equal(t, want, settings(h), "nothing that was written in between is lost")

		// The audit entry of the late PUT has the pause and the share in its
		// before: it was applied to them.
		entries := updates(st)
		require.Len(t, entries, 3)
		assert.Equal(t, "settings.update", entries[0].Action)

		before, ok := entries[0].Detail["before"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, before["paused"])
		assert.InDelta(t, 0.25, before["readyPeerShare"], 0)
		assert.InDelta(t, 3, before["minPeers"], 0)
	})

	t.Run("overlapping PUTs each change what they name", func(t *testing.T) {
		h, _ := realScheduler(t)

		const rounds = 20

		done := make(chan struct{}, 2)

		put := func(body func(i int) string) {
			defer func() { done <- struct{}{} }()

			for i := range rounds {
				rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body(i))
				assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
		}

		// A names minPeers, B names paused and the peer share.
		go put(func(i int) string {
			if i%2 == 0 {
				return `{"minPeers":7}`
			}

			return `{"minPeers":8}`
		})
		go put(func(i int) string {
			if i%2 == 0 {
				return `{"paused":false,"readyPeerShare":0.5}`
			}

			return `{"paused":true,"readyPeerShare":0.25}`
		})

		<-done
		<-done

		got := settings(h)
		assert.Equal(t, 8, got.MinPeers, "the last value A wrote")
		assert.True(t, got.Paused, "the last values B wrote")
		assert.InDelta(t, 0.25, got.ReadyPeerShare, 0)
	})

	t.Run("a body that changes nothing is no entry in the audit log", func(t *testing.T) {
		h, st := realScheduler(t)

		for _, body := range []string{`{}`, `{"minPeers":3}`, `{"paused":false,"readyPeerShare":0}`} {
			rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, fleet.DefaultSettings(), controllertest.DecodeJSON[views.SettingsBody](t, rec).Settings)
		}

		assert.Empty(t, updates(st))

		rec := h.sendJSON(http.MethodPut, "/api/v1/settings", `{"minPeers":4}`)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Len(t, updates(st), 1)

		rec = h.sendJSON(http.MethodPut, "/api/v1/settings", `{"minPeers":4}`)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Len(t, updates(st), 1, "the same again")
	})

	t.Run("a body that makes the whole invalid changes nothing", func(t *testing.T) {
		h, st := realScheduler(t)

		rec := h.sendJSON(http.MethodPut, "/api/v1/settings", `{"readyPeerShare":0.4,"minPeers":0}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error, "minPeers")
		assert.Equal(t, fleet.DefaultSettings(), settings(h))
		assert.Empty(t, updates(st))
	})
}

// quietLogger is a logger that writes nowhere.
func quietLogger() logrus.FieldLogger {
	log := logrus.New()
	log.SetOutput(io.Discard)

	return log
}
