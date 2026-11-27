package userapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
)

const (
	// PathBuildHook is the route an image builder calls when a build finished.
	PathBuildHook = "/api/v1/hooks/build"

	// maxHookBody bounds the body of a hook; a GitHub workflow_run event is
	// a few dozen KiB.
	maxHookBody = 1 << 20

	headerGitHubEvent     = "X-GitHub-Event"
	headerGitHubSignature = "X-Hub-Signature-256"
	signaturePrefix       = "sha256="
	bearerPrefix          = "Bearer "

	githubEventPing  = "ping"
	githubActionDone = "completed"
)

// BuildHook is the body of the answer of POST /api/v1/hooks/build.
type BuildHook struct {
	// Refreshed says that the tags and the source branches are looked up now.
	Refreshed bool `json:"refreshed"`
	// Message says what happened, for people.
	Message string `json:"message"`
}

// handleBuildHook serves POST /api/v1/hooks/build: an image builder says
// that a build finished, so every followed tag and every source branch is
// looked up now instead of at its next interval. The caller proves the hook
// secret, either as a bearer token or the way GitHub signs a webhook (an
// HMAC-SHA256 of the body in X-Hub-Signature-256); of a GitHub webhook only
// a run or job that completed counts.
func (s *Handler) handleBuildHook(w http.ResponseWriter, r *http.Request) {
	if len(s.opts.HookSecret) == 0 {
		httpjson.Error(s.log, w, http.StatusNotFound, "no build hook is configured")

		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpjson.Error(s.log, w, http.StatusRequestEntityTooLarge, errBodyTooLarge.Error())

			return
		}

		httpjson.Error(s.log, w, http.StatusBadRequest, "the body could not be read")

		return
	}

	if !s.hookAuthorized(r, body) {
		httpjson.Error(s.log, w, http.StatusUnauthorized, "the hook secret is required")

		return
	}

	if event := r.Header.Get(headerGitHubEvent); event != "" {
		var payload struct {
			Action string `json:"action"`
		}

		// A push or a release has no action: it counts as it is.
		_ = json.Unmarshal(body, &payload)

		if event == githubEventPing || (payload.Action != "" && payload.Action != githubActionDone) {
			httpjson.Write(s.log, w, http.StatusOK, BuildHook{Message: "nothing finished: nothing is looked up"})

			return
		}
	}

	done := s.refreshImages()

	s.log.WithField("tags", done.Tags).Info("build hook: looking the tags and source branches up now")
	httpjson.Write(s.log, w, http.StatusOK, BuildHook{Refreshed: true, Message: done.Message})
}

// hookAuthorized reports whether the request proves the hook secret.
func (s *Handler) hookAuthorized(r *http.Request, body []byte) bool {
	if signature, ok := strings.CutPrefix(r.Header.Get(headerGitHubSignature), signaturePrefix); ok {
		got, err := hex.DecodeString(signature)
		if err != nil {
			return false
		}

		mac := hmac.New(sha256.New, s.opts.HookSecret)
		mac.Write(body)

		return hmac.Equal(got, mac.Sum(nil))
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), bearerPrefix)

	return ok && subtle.ConstantTimeCompare([]byte(token), s.opts.HookSecret) == 1
}
