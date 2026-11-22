package registry

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/registryauth"
)

func basicHeader(userPass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(userPass))
}

func TestParseChallenge(t *testing.T) {
	cases := []struct {
		name, in string
		want     map[string]string
	}{
		{
			"quoted",
			`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:a/b:pull"`,
			map[string]string{
				"realm": "https://auth.example/token", "service": "registry.example", "scope": "repository:a/b:pull",
			},
		},
		{
			"comma inside a quoted value",
			`Bearer realm="https://a/t", scope="repository:a/b:pull,push" , service=svc`,
			map[string]string{"realm": "https://a/t", "scope": "repository:a/b:pull,push", "service": "svc"},
		},
		{
			"case, bare values, escapes",
			"  bEARER\tRealm=https://a/t,SERVICE = \"a \\\"b\\\" c\",error=x ",
			map[string]string{"realm": "https://a/t", "service": `a "b" c`, "error": "x"},
		},
		{"unterminated quote", `Bearer realm="https://a/t`, map[string]string{"realm": "https://a/t"}},
		{"trailing backslash", `Bearer realm="a\`, map[string]string{"realm": "a"}},
		{"junk after the parameters", `Bearer realm="a",junk`, map[string]string{"realm": "a"}},
		{"no parameters", "Bearer", map[string]string{}},
		{"only spaces", "Bearer   ", map[string]string{}},
		{"basic", `Basic realm="x"`, nil},
		{"empty", "", nil},
		{"prefix only", "Bearerx realm=a", nil},
		{"short", "Bea", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, parseChallenge(c.in))
		})
	}
}

func TestBearerChallengePicksTheBearerHeader(t *testing.T) {
	h := http.Header{}
	require.Nil(t, bearerChallenge(h))

	h.Add("WWW-Authenticate", `Basic realm="x"`)
	require.Nil(t, bearerChallenge(h))

	h.Add("WWW-Authenticate", `Bearer realm="https://a/t"`)
	require.Equal(t, map[string]string{"realm": "https://a/t"}, bearerChallenge(h))
}

func TestTokenFlow(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.bearer = testToken })

	r := newResolver(t, f, loginFor(f.host(), testBasic))

	for range 3 {
		digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
		require.Equal(t, f.indexDigest, digest)
	}

	// One challenge, one token, reused for every later request.
	require.Equal(t, 1, f.tokens())
	require.Equal(t, 1, f.unauthorized)
	require.Equal(t, 4, f.heads(), "three resolutions plus the challenged first HEAD")

	require.Equal(t, []string{basicHeader(testBasic)}, f.tokenAuth)
	require.Equal(t, testService, f.tokenQueries[0].Get("service"))
	require.Equal(t, []string{userAgent}, f.tokenAgents, "the token service sees the same User-Agent")
	require.Equal(t, map[string]struct{}{userAgent: {}}, f.v2Agents)
	require.Equal(t, []string{"repository:" + repoPath + ":pull"}, f.tokenQueries[0]["scope"],
		"pull on the one repository, not the scope the challenge suggested")
}

func TestTokenFlowAnonymous(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) {
		f.bearer = testToken
		// A realm with its own query and a fragment, and no service.
		f.challenge = `Bearer realm="` + f.srv.URL + `/token?account=x#frag"`
		f.tokenBody = func() string { return `{"access_token":"` + testToken + `"}` }
	})

	r := newResolver(t, f, nil)

	digest, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest)

	require.Equal(t, []string{""}, f.tokenAuth, "no credentials, no Authorization header")
	require.Equal(t, "x", f.tokenQueries[0].Get("account"))
	require.False(t, f.tokenQueries[0].Has("service"))
	require.Equal(t, "repository:"+repoPath+":pull", f.tokenQueries[0].Get("scope"))
}

func TestRejectedCachedTokenIsFetchedAgainOnce(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.bearer = testToken })

	r := newResolver(t, f, nil)

	_, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, 1, f.tokens())
	require.Equal(t, 1, f.unauthorized)

	// The registry stops accepting the cached token (it expired).
	f.set(func(f *fakeRegistry) { f.bearer = "tok-2" })

	digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
	require.Equal(t, 2, f.tokens())
	require.Equal(t, 2, f.unauthorized)

	// And the new one is cached in turn.
	_, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, 2, f.tokens())
	require.Equal(t, 2, f.unauthorized)
}

func TestAnnouncedTokenExpiry(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) {
		f.bearer = testToken
		f.tokenBody = func() string { return `{"token":"` + testToken + `","expires_in":60}` }
	})

	r := newResolver(t, f, nil)

	now := time.Unix(1_800_000_000, 0)
	r.now = func() time.Time { return now }

	resolve := func() {
		t.Helper()

		_, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
	}

	resolve()
	require.Equal(t, 1, f.tokens())

	now = now.Add(49 * time.Second)

	resolve()
	require.Equal(t, 1, f.tokens(), "still valid")

	// Within the slack before expiry the token is dropped and challenged again.
	now = now.Add(time.Second)

	require.Empty(t, r.cachedToken(f.host()+"/"+repoPath))
	resolve()
	require.Equal(t, 2, f.tokens())
	require.Equal(t, 2, f.unauthorized)
}

func TestTokenThatIsNeverAccepted(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) {
		f.bearer = testToken
		f.tokenBody = func() string { return `{"token":"wrong"}` }
	})

	r := newResolver(t, f, nil)

	for i := 1; i <= 2; i++ {
		_, err := r.Resolve(t.Context(), f.ref(tagMulti))

		var status *statusError

		require.ErrorAs(t, err, &status)
		require.Equal(t, http.StatusUnauthorized, status.Code)
		require.Equal(t, i, f.tokens(), "one token request per resolution, no loop")
		require.Equal(t, 2*i, f.heads())
		require.Empty(t, r.tokens, "a rejected token is not kept")
	}
}

func TestBasicAuthRegistry(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) {
		f.basic = testBasic
		f.challenge = `Basic realm="registry"`
	})

	r := newResolver(t, f, loginFor(f.host(), testBasic))

	digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
	require.Equal(t, 0, f.tokens())
	require.Equal(t, 0, f.unauthorized)

	var status *statusError

	// Wrong and missing credentials end in a 401 without any token request.
	for _, logins := range []registryauth.Logins{loginFor(f.host(), "user:wrong"), nil} {
		r = newResolver(t, f, logins)

		_, err = r.Resolve(t.Context(), f.ref(tagMulti))
		require.ErrorAs(t, err, &status)
		require.Equal(t, http.StatusUnauthorized, status.Code)
		require.NotContains(t, err.Error(), "wrong")
	}

	require.Equal(t, 0, f.tokens())

	// Credentials for another host are not sent here.
	r = newResolver(t, f, loginFor("other.example", testBasic))

	_, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.ErrorAs(t, err, &status)
}

func TestTokenEndpointFailures(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cases := []struct {
		name string
		set  func(f *fakeRegistry)
		want string
	}{
		{"server error", func(f *fakeRegistry) { f.tokenStatus = http.StatusInternalServerError }, "/token: 500"},
		{"denied", func(f *fakeRegistry) { f.tokenStatus = http.StatusForbidden }, "/token: 403"},
		{"not json", func(f *fakeRegistry) { f.tokenBody = func() string { return "secret-ish garbage" } }, "not valid JSON"},
		{"no token", func(f *fakeRegistry) { f.tokenBody = func() string { return "{}" } }, "had no token"},
		{
			"oversized", func(f *fakeRegistry) {
				f.tokenBody = func() string { return `{"token":"` + strings.Repeat("a", maxTokenBytes) + `"}` }
			}, errBodyTooLarge.Error(),
		},
		{"no realm", func(f *fakeRegistry) { f.challenge = `Bearer service="x"` }, "unusable bearer realm"},
		{"relative realm", func(f *fakeRegistry) { f.challenge = `Bearer realm="/token"` }, "unusable bearer realm"},
		{"unparsable realm", func(f *fakeRegistry) { f.challenge = `Bearer realm="http://[::1"` }, "unusable bearer realm"},
		{"realm scheme", func(f *fakeRegistry) { f.challenge = `Bearer realm="ftp://auth.example/token"` }, "not https"},
		{"unreachable realm", func(f *fakeRegistry) { f.challenge = `Bearer realm="` + deadURL + `/token"` }, "token request for"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			f.set(func(f *fakeRegistry) {
				f.bearer = testToken
				c.set(f)
			})

			r := newResolver(t, f, loginFor(f.host(), testBasic))

			_, err := r.Resolve(t.Context(), f.ref(tagMulti))
			require.ErrorContains(t, err, c.want)
			require.NotContains(t, err.Error(), "secret")
			require.Empty(t, r.tokens)
			require.Equal(t, 1, f.heads(), "no retry without a token")
		})
	}
}

func TestStatusErrorFromTokenEndpoint(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) {
		f.bearer = testToken
		f.tokenStatus = http.StatusTooManyRequests
	})

	r := newResolver(t, f, nil)

	_, err := r.Resolve(t.Context(), f.ref(tagMulti))

	var status *statusError

	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusTooManyRequests, status.Code)
	require.Equal(t, f.host()+"/token", status.URL)
}

func TestPlainHTTPRealmNeedsAPlainHTTPRegistry(t *testing.T) {
	// A TLS registry's http realm is refused: credentials would leave TLS.
	f := newFakeRegistry(t, true)
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the plain-http realm must not be contacted")
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(realm.Close)

	f.set(func(f *fakeRegistry) {
		f.bearer = testToken
		f.challenge = `Bearer realm="` + realm.URL + `/token"`
	})

	r := buildResolver(t, Config{CAFile: caFileFor(t, f.srv)}, loginFor(f.host(), testBasic))

	_, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.ErrorContains(t, err, "not https")

	// The same registry with its own https realm works.
	f.set(func(f *fakeRegistry) { f.challenge = "" })

	digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
}

func TestConnectionDroppedAfterToken(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.bearer = testToken })

	// The retry with the fresh token hits a closed connection.
	f.handle(kindManifst+"dropped", func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot be hijacked")

			return
		}

		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)

			return
		}

		_ = conn.Close()
	})

	r := newResolver(t, f, nil)

	_, err := r.Resolve(t.Context(), f.ref("dropped"))
	require.Error(t, err)

	var status *statusError

	require.NotErrorAs(t, err, &status)
	require.Equal(t, 1, f.tokens())
}

// The token cache is bounded: expired tokens are swept, a full cache gives
// up the token that expires first, and an oversized token is not kept.
func TestTokenCacheBounds(t *testing.T) {
	r := buildResolver(t, Config{}, nil)

	now := time.Unix(1_800_000_000, 0)
	r.now = func() time.Time { return now }

	r.storeToken("big", token{value: strings.Repeat("a", maxCachedTokenBytes+1)})
	require.Empty(t, r.tokens, "an oversized token is not cached")

	r.storeToken("expiring", token{value: "e", expires: now.Add(time.Minute)})
	r.storeToken("first", token{value: "f", expires: now.Add(2 * time.Minute)})

	for i := range maxCachedTokens - 2 {
		r.storeToken("repo-"+strconv.Itoa(i), token{value: "t", expires: now.Add(time.Hour)})
	}

	require.Len(t, r.tokens, maxCachedTokens)

	// The expired token goes: room without evicting anything else.
	now = now.Add(90 * time.Second)

	r.storeToken("new", token{value: "n"})
	require.Len(t, r.tokens, maxCachedTokens)
	require.NotContains(t, r.tokens, "expiring")

	// Full: the token that expires first makes room.
	r.storeToken("newer", token{value: "n"})
	require.Len(t, r.tokens, maxCachedTokens)
	require.NotContains(t, r.tokens, "first")
	require.Contains(t, r.tokens, "new", "a token without a lifetime goes last")
}
