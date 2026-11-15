package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// settingsPatch is the body of PUT /api/v1/settings: the settings it names,
// each with the JSON value it gives. A key that is not named keeps the value
// it has, so a client that does not know a setting (a page of an earlier
// build) cannot reset it by leaving it out.
//
// The keys are read exactly as written. Go's decoder would take "MinPeers"
// for "minPeers" and, of two spellings in one body, the last; here a key that
// is not a setting's name byte for byte is refused, and so is a key that is
// named twice.
type settingsPatch struct {
	keys   []string
	values map[string]json.RawMessage
}

// UnmarshalJSON reads one JSON object and keeps its keys as they are written.
func (p *settingsPatch) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))

	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("the settings must be an object")
	}

	p.values = make(map[string]json.RawMessage, 16)

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}

		key, ok := tok.(string)
		if !ok {
			return errors.New("the settings must be an object")
		}

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}

		if _, dup := p.values[key]; dup {
			return fmt.Errorf("%s is named twice", key)
		}

		p.keys = append(p.keys, key)
		p.values[key] = value
	}

	return nil
}

// settingNames are the JSON names of the settings.
func settingNames() []string {
	raw, err := json.Marshal(fleet.Settings{})
	if err != nil {
		return nil
	}

	fields := make(map[string]json.RawMessage, 16)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// check refuses what cannot be a value of a setting before anything is
// applied: a key that is no setting, null, and a number that is written as
// more than nothing and cannot be told from 0 (1e-400 for a share).
func (p *settingsPatch) check() error {
	names := settingNames()

	for _, key := range p.keys {
		if !slices.Contains(names, key) {
			return badRequestf("invalid JSON body: unknown setting %q", key)
		}

		literal := string(bytes.TrimSpace(p.values[key]))

		if literal == "null" {
			return badRequestf("invalid JSON body: %s must not be null", key)
		}

		if underflows(literal) {
			return badRequestf("invalid JSON body: %s: %s is too small to be told from 0", key, literal)
		}
	}

	return nil
}

// underflows reports whether literal is a JSON number with a digit other
// than 0 in it that is read as 0.
func underflows(literal string) bool {
	value, err := strconv.ParseFloat(literal, 64)
	if literal == "" || (err != nil && !errors.Is(err, strconv.ErrRange)) || value != 0 {
		return false
	}

	mantissa, _, _ := strings.Cut(strings.ToLower(literal), "e")

	return strings.ContainsAny(mantissa, "123456789")
}

// apply puts the named values onto settings and validates the result as a
// whole. It runs under the scheduler's lock, on the settings as they are
// then. Nothing is changed when it fails.
func (p *settingsPatch) apply(settings *fleet.Settings) error {
	raw, err := json.Marshal(p.values)
	if err != nil {
		return badRequestf("invalid JSON body: %s", err.Error())
	}

	next := *settings

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&next); err != nil {
		return badRequestf("invalid JSON body: %s", err.Error())
	}

	if err := next.Validate(); err != nil {
		return badRequestf("%s", err.Error())
	}

	*settings = next

	return nil
}

// handlePutSettings serves PUT /api/v1/settings. The body is applied to the
// stored settings: every key it names is replaced, the others stay. The
// result is validated as a whole; the scheduler stores and audits the change.
//
// It needs the feature admin, whatever the body says: the settings carry the
// global pause, the budget, the thresholds of the network gate and the
// policies.
//
// The body is read first and applied in one step under the scheduler's lock
// (PatchSettings), to the settings as they are at that moment. Two requests
// that overlap, or a pause that lands between the read and the write of
// another request, lose nothing: each changes what it names. A body that
// names nothing, or only what is set already, changes nothing and writes no
// audit entry.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request, u *user) {
	ctx, ok := s.allow(w, r, u, &requirement{feature: FeatureAdmin})
	if !ok {
		return
	}

	var patch settingsPatch
	if err := decodeBody(w, r, &patch); err != nil {
		s.writeBodyError(w, err)

		return
	}

	if err := patch.check(); err != nil {
		s.writeBodyError(w, err)

		return
	}

	stored, _, err := s.opts.Scheduler.PatchSettings(ctx, u.actor, patch.apply)

	switch {
	case err == nil:
	case errors.Is(err, errBadRequest):
		s.writeBadRequest(w, err)

		return
	default:
		s.writeSchedulerError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, SettingsBody{
		Settings: stored,
		Defaults: s.opts.DefaultSettings,
	})
}

// A patch is what decodeBody decodes the body into.
var _ json.Unmarshaler = (*settingsPatch)(nil)
