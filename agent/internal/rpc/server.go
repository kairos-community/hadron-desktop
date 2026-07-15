package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
)

// Handler implements the behavior behind the four RPC endpoints. Server
// wires it to an already-created net.Listener and performs no tool-name
// validation of its own: routing an unsupported tool name to a proper error
// result is Handler.Call's job (see package api's error codes).
type Handler interface {
	// Call executes req.Tool with req.Arguments. authHeader is the
	// caller's raw, unmodified "Authorization: Bearer ..." header value,
	// or "" if the caller sent none. Server never inspects, parses, or
	// trusts authHeader itself -- it only requires its presence when the
	// Server was constructed with Config.RequireAuth, and forwards
	// whatever the caller sent. Authenticating (and re-verifying) it is
	// entirely Handler's responsibility.
	Call(ctx context.Context, req CallRequest, authHeader string) (*CallResponse, error)
	// Health reports the current status of whatever this Handler fronts.
	Health(ctx context.Context) (HealthStatus, error)
	// Pause and Resume are idempotent: Server calls them directly on
	// every POST /v1/pause or /v1/resume with no de-duplication of its
	// own, so a Handler implementation must tolerate (and succeed on)
	// repeated calls in any order.
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

// Config configures a Server.
type Config struct {
	// RequireAuth, when true, means every request must carry a non-empty
	// Authorization header or Server rejects it with 401 before ever
	// calling into Handler. Set this true only for a Server bound to
	// RootSocketPath; leave it false for the session and control
	// sockets. This is the *only* authority Server enforces: it never
	// accepts a boolean "admin" field, a forwarded scope, or any other
	// signal as a substitute for the raw header being present. Handler
	// (the root helper, in a later Phase-2 task) is responsible for
	// actually verifying the bearer's validity.
	RequireAuth bool
}

// Server serves the fixed four-endpoint RPC protocol over an
// already-created net.Listener. Server never creates, chmods, or removes
// any socket file itself.
type Server struct {
	handler Handler
	httpSrv *http.Server
}

// NewServer builds a Server backed by handler. It does not start serving
// until Serve is called.
func NewServer(handler Handler, cfg Config) *Server {
	s := &Server{handler: handler}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathCall, s.handleCall)
	mux.HandleFunc("GET "+PathHealth, s.handleHealth)
	mux.HandleFunc("POST "+PathPause, s.handlePause)
	mux.HandleFunc("POST "+PathResume, s.handleResume)

	var top http.Handler = mux
	if cfg.RequireAuth {
		top = requireAuthorization(top)
	}

	s.httpSrv = &http.Server{Handler: top}
	return s
}

// Serve accepts connections on l until Shutdown is called (or l/Serve fails
// for some other reason). It blocks until then. A clean shutdown via
// Shutdown is reported as a nil error, matching net/http.Server.Serve's
// documented http.ErrServerClosed convention.
func (s *Server) Serve(l net.Listener) error {
	err := s.httpSrv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server, waiting for in-flight requests to
// finish or ctx to expire, whichever comes first.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// ---------------------------------------------------------------------------
// Authorization gate
// ---------------------------------------------------------------------------

// requireAuthorization rejects any request with no Authorization header
// before it reaches next. It does not interpret the header's value in any
// way -- presence is the only thing it checks; Handler.Call is given the
// raw value and is solely responsible for verifying it.
func requireAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			writeError(w, http.StatusUnauthorized, "authorization required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusBadRequest, "content-type must be application/json")
		return
	}

	authHeader := r.Header.Get("Authorization")

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req CallRequest
	if err := dec.Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "unexpected trailing data after JSON body")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}

	resp, err := s.handler.Call(r.Context(), req, authHeader)
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone; nothing useful to write back, and
			// attempting to write to a canceled request's
			// ResponseWriter is harmless but pointless.
			return
		}
		writeError(w, http.StatusInternalServerError, "call failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status, err := s.handler.Health(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeError(w, http.StatusInternalServerError, "health check failed")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.handler.Pause(r.Context()); err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeError(w, http.StatusInternalServerError, "pause failed")
		return
	}
	writeJSON(w, http.StatusOK, StatusResponse{OK: true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.handler.Resume(r.Context()); err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeError(w, http.StatusInternalServerError, "resume failed")
		return
	}
	writeJSON(w, http.StatusOK, StatusResponse{OK: true})
}

// ---------------------------------------------------------------------------
// Wire helpers
// ---------------------------------------------------------------------------

// isJSONContentType reports whether v names the application/json media
// type, ignoring any parameters (e.g. "application/json; charset=utf-8").
// An empty or malformed Content-Type is rejected.
func isJSONContentType(v string) bool {
	if v == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(v)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// writeDecodeError classifies a json.Decoder.Decode error and writes the
// appropriate clean protocol error. It never echoes the decoder's raw
// message (which can include internal field/offset detail) back to the
// caller; every case gets a short, fixed, safe description. An oversized
// body -- detected via http.MaxBytesReader's dedicated error type -- gets
// 413; every other decode failure (truncated JSON, malformed JSON, unknown
// field, wrong type, empty body) gets 400.
func writeDecodeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds the maximum allowed size")
		return
	}
	writeError(w, http.StatusBadRequest, "malformed request body")
}

// writeJSON writes v as a JSON body with the given status code. It never
// panics: a marshal failure (which should not happen for the fixed wire
// types this package produces) falls back to a generic 500 with no body
// leaked from the marshal error itself.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error encoding response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeError writes an ErrorResponse with the given status code and a
// short, safe message.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}
