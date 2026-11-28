package api

import (
	"bytes"
	"net/http"
	"strconv"
)

// pprofGate serves the web UI's page as the login gate of the profiles. The
// UI loads its assets relative to the page, so the page gets a base of "/":
// below /debug/pprof/ they would be profile requests.
func pprofGate(ui http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := &bufferedResponse{header: make(http.Header, 8), status: http.StatusOK}
		ui.ServeHTTP(buf, r)

		body := buf.body.Bytes()
		if i := bytes.Index(body, []byte("<head>")); i >= 0 {
			at := i + len("<head>")
			body = append(body[:at:at], append([]byte(`<base href="/">`), body[at:]...)...)
		}

		for k, v := range buf.header {
			w.Header()[k] = v
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(buf.status)
		_, _ = w.Write(body)
	})
}

// bufferedResponse keeps a response in memory.
type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

func (b *bufferedResponse) WriteHeader(code int) { b.status = code }
