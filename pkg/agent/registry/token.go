package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"

	// tokenExpirySlack is how long before its announced expiry a token is no
	// longer used.
	tokenExpirySlack = 10 * time.Second
)

// token is a cached bearer token.
type token struct {
	value string
	// expires is zero when the token service announced no lifetime; such a
	// token is used until the registry rejects it.
	expires time.Time
}

// cachedToken returns the usable bearer token for a repository, or "".
func (r *Resolver) cachedToken(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	tok, ok := r.tokens[key]
	if !ok {
		return ""
	}

	if !tok.expires.IsZero() && !r.now().Before(tok.expires) {
		delete(r.tokens, key)

		return ""
	}

	return tok.value
}

// storeToken caches a bearer token for a repository.
func (r *Resolver) storeToken(key string, tok token) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tokens[key] = tok
}

// dropToken forgets the cached token of a repository if it is still value.
func (r *Resolver) dropToken(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if tok, ok := r.tokens[key]; ok && tok.value == value {
		delete(r.tokens, key)
	}
}

// fetchToken follows a Bearer challenge as the distribution token
// specification describes. The scope asked for is always pull on the one
// repository, whatever the challenge suggests.
func (r *Resolver) fetchToken(ctx context.Context, ref Reference, challenge map[string]string) (token, error) {
	realm, err := url.Parse(challenge["realm"])
	if err != nil || realm.Host == "" {
		return token{}, fmt.Errorf("registry: %s sent an unusable bearer realm", ref.APIHost())
	}

	// Credentials may travel to the realm, so it gets the transport security
	// of the registry itself: plain http only for a plain-http registry.
	if realm.Scheme != schemeHTTPS && (realm.Scheme != schemeHTTP || !r.isPlainHTTP(ref)) {
		return token{}, fmt.Errorf("registry: %s sent a bearer realm that is not https", ref.APIHost())
	}

	q := realm.Query()
	if service := challenge["service"]; service != "" {
		q.Set("service", service)
	}

	q.Set("scope", "repository:"+ref.Path+":pull")
	realm.RawQuery = q.Encode()
	realm.Fragment = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), http.NoBody)
	if err != nil {
		return token{}, fmt.Errorf("registry: build token request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	if creds, ok := r.creds[ref.Host]; ok {
		req.Header.Set("Authorization", creds.basic())
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return token{}, fmt.Errorf("registry: token request for %s: %w", ref.Repository(), scrubURLError(err))
	}

	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return token{}, &StatusError{Method: http.MethodGet, URL: realm.Host + realm.Path, Code: resp.StatusCode}
	}

	body, err := readCapped(resp.Body, maxTokenBytes)
	if err != nil {
		return token{}, fmt.Errorf("registry: read token for %s: %w", ref.Repository(), err)
	}

	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"` //nolint:tagliatelle // OAuth wire format
		ExpiresIn   int64  `json:"expires_in"`   //nolint:tagliatelle // OAuth wire format
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		// A JSON error can quote the input, which may hold a token.
		return token{}, fmt.Errorf("registry: token response for %s is not valid JSON", ref.Repository())
	}

	tok := token{value: answer.Token}
	if tok.value == "" {
		tok.value = answer.AccessToken
	}

	if tok.value == "" {
		return token{}, fmt.Errorf("registry: token response for %s had no token", ref.Repository())
	}

	if answer.ExpiresIn > 0 {
		tok.expires = r.now().Add(time.Duration(answer.ExpiresIn)*time.Second - tokenExpirySlack)
	}

	return tok, nil
}

// scrubURLError removes the URL from a transport error: a token realm's query
// is not secret, but there is no reason to repeat it in every log line.
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}

// bearerChallenge returns the parameters of the first Bearer challenge in the
// WWW-Authenticate headers of a response, or nil when there is none.
func bearerChallenge(header http.Header) map[string]string {
	for _, value := range header.Values("WWW-Authenticate") {
		if params := parseChallenge(value); params != nil {
			return params
		}
	}

	return nil
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`. Values may be
// quoted strings (with backslash escapes, and commas inside) or bare tokens.
// It returns nil for anything that is not a Bearer challenge.
func parseChallenge(h string) map[string]string {
	h = strings.TrimSpace(h)

	const scheme = "bearer"
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return nil
	}

	rest := h[len(scheme):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return nil
	}

	out := make(map[string]string, 3)

	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			return out
		}

		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return out
		}

		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = strings.TrimLeft(rest[eq+1:], " \t")

		var value string

		if strings.HasPrefix(rest, `"`) {
			value, rest = unquote(rest[1:])
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}

			value, rest = strings.TrimSpace(rest[:end]), rest[end:]
		}

		out[key] = value
	}
}

// unquote reads a quoted-string after its opening quote and returns its value
// and what follows the closing quote. An unterminated string runs to the end.
func unquote(s string) (value, rest string) {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}

	return b.String(), ""
}
