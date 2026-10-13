package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/util"
)

// APIError is the unified error for daemon endpoints. It renders as
// {"error": message, "status": code}.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

func apiErr(status int, format string, args ...any) *APIError {
	return &APIError{Status: status, Message: fmt.Sprintf(format, args...)}
}

func errNotFound(format string, a ...any) *APIError { return apiErr(http.StatusNotFound, format, a...) }
func errBadRequest(format string, a ...any) *APIError {
	return apiErr(http.StatusBadRequest, format, a...)
}
func errInternal(format string, a ...any) *APIError {
	return apiErr(http.StatusInternalServerError, format, a...)
}
func errConflict(format string, a ...any) *APIError { return apiErr(http.StatusConflict, format, a...) }
func errUnauthorized(format string, a ...any) *APIError {
	return apiErr(http.StatusUnauthorized, format, a...)
}
func errForbidden(format string, a ...any) *APIError {
	return apiErr(http.StatusForbidden, format, a...)
}
func errBadGateway(format string, a ...any) *APIError {
	return apiErr(http.StatusBadGateway, format, a...)
}
func errUnavailable(format string, a ...any) *APIError {
	return apiErr(http.StatusServiceUnavailable, format, a...)
}
func errNotImplemented(format string, a ...any) *APIError {
	return apiErr(http.StatusNotImplemented, format, a...)
}

// toAPIError maps any error onto an APIError, following the Rust
// `From<ConnectorError> for ApiError` mapping for connector errors.
func toAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	var ce *connector.Error
	if errors.As(err, &ce) {
		slog.Error("connector error", "err", ce)
		msg := ce.Error()
		switch ce.Kind {
		case connector.ErrAuth:
			return &APIError{http.StatusUnauthorized, msg}
		case connector.ErrUpstreamHTTP:
			if ce.Status >= 400 && ce.Status < 500 {
				return &APIError{ce.Status, msg}
			}
			return &APIError{http.StatusBadGateway, msg}
		case connector.ErrBadInput:
			return &APIError{http.StatusUnprocessableEntity, msg}
		case connector.ErrConnection, connector.ErrQuery, connector.ErrIntrospection:
			return &APIError{http.StatusBadGateway, msg}
		case connector.ErrConfig:
			return &APIError{http.StatusInternalServerError, msg}
		case connector.ErrUnsupported:
			return &APIError{http.StatusNotImplemented, msg}
		}
	}
	return &APIError{http.StatusInternalServerError, err.Error()}
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := util.MarshalJSON(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":"failed to encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeError renders err as the standard {"error","status"} body.
func writeError(w http.ResponseWriter, err error) {
	ae := toAPIError(err)
	writeJSON(w, ae.Status, map[string]any{"error": ae.Message, "status": ae.Status})
}

// writeErrorOnly renders {"error": msg} without a status field — the shape
// many Rust handlers build by hand with `json!({"error": ...})`.
func writeErrorOnly(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// handlerFunc is an http handler that may return an error to be rendered.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (s *Server) wrap(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, err)
		}
	}
}

// maxBodyBytes caps JSON request bodies (axum's default limit is 2MB; uploads
// use their own limit).
const maxBodyBytes = 2 << 20

// decodeJSON decodes the request body into v. Malformed JSON → 400; a
// well-formed body of the wrong shape → 422 (axum's Json rejection codes).
func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return errBadRequest("failed to read request body: %v", err)
	}
	if len(body) > maxBodyBytes {
		return apiErr(http.StatusRequestEntityTooLarge, "request body too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errBadRequest("Failed to parse the request body as JSON: EOF while parsing a value")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errBadRequest("Failed to parse the request body as JSON: %v", err)
		}
		return apiErr(http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: %v", err)
	}
	return nil
}

// pathInt64 parses a path wildcard as int64 (400 on failure, like axum's Path).
func pathInt64(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errBadRequest("Invalid URL: Cannot parse `%s` with value `%s` to a `i64`", name, raw)
	}
	return v, nil
}

// queryInt64 returns a pointer to the parsed query value, nil when absent.
func queryInt64(r *http.Request, name string) (*int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, errBadRequest("Failed to deserialize query string: %s: invalid digit found in string", name)
	}
	return &v, nil
}

// queryBool returns a pointer to the parsed query bool, nil when absent.
func queryBool(r *http.Request, name string) (*bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, errBadRequest("Failed to deserialize query string: %s: provided string was not `true` or `false`", name)
	}
	return &v, nil
}

// queryString returns a pointer to the query value, nil when absent.
func queryString(r *http.Request, name string) *string {
	q := r.URL.Query()
	if !q.Has(name) {
		return nil
	}
	v := q.Get(name)
	return &v
}

// bearerToken extracts the token from `Authorization: Bearer <t>`.
func bearerToken(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok, ok
}
