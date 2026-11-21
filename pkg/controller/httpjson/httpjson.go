// Package httpjson writes the JSON answers of the controller's HTTP API: the
// body, the error body and the answer to a request that failed inside.
package httpjson

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

const (
	// HeaderContentType is the Content-Type header.
	HeaderContentType = "Content-Type"
	// ContentTypeJSON is the media type of every JSON answer.
	ContentTypeJSON = "application/json; charset=utf-8"
)

// ErrorBody is the body of every error answer.
type ErrorBody struct {
	Error string `json:"error"`
}

// Write writes v as the JSON answer.
func Write(log logrus.FieldLogger, w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.WithError(err).Error("Cannot encode response")

		status = http.StatusInternalServerError
		body = []byte(`{"error":"internal error"}`)
	}

	w.Header().Set(HeaderContentType, ContentTypeJSON)
	w.WriteHeader(status)

	if _, err := w.Write(append(body, '\n')); err != nil {
		log.WithError(err).Debug("Cannot write response")
	}
}

// Error writes an ErrorBody.
func Error(log logrus.FieldLogger, w http.ResponseWriter, status int, msg string) {
	Write(log, w, status, ErrorBody{Error: msg})
}

// InternalError logs err and answers 500 without detail: store and
// scheduler errors can carry anything.
func InternalError(log logrus.FieldLogger, w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		// The client went away; nobody reads the answer.
		Error(log, w, http.StatusServiceUnavailable, "request cancelled")

		return
	}

	log.WithError(err).WithFields(logrus.Fields{
		"method": r.Method,
		"path":   r.URL.Path,
	}).Error("Request failed")
	Error(log, w, http.StatusInternalServerError, "internal error")
}

// ErrBadRequest marks request validation errors, answered 400.
var ErrBadRequest = errors.New("bad request")

// BadRequestf returns a validation error.
func BadRequestf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, args...))
}

// BadRequest answers a validation error without the ErrBadRequest prefix.
func BadRequest(log logrus.FieldLogger, w http.ResponseWriter, err error) {
	Error(log, w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrBadRequest.Error()+": "))
}
