package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/run"
)

// Body limits of the runner API.
const (
	maxRunnerSmall = 64 << 10 // claim, event, lock and unlock bodies
	// maxRunnerState is the most bytes of OpenTofu state a run may store
	// (one database is about 35 KiB).
	maxRunnerState = 16 << 20
	// maxRunnerResult fits a plan file and a plan JSON of maxRunResult each,
	// base64-encoded in JSON.
	maxRunnerResult = 2*maxRunResult*4/3 + 1<<20
)

// RunnerHandler serves api to runners over plain HTTP: three JSON calls for
// the runner program, and the OpenTofu "http" state backend. Every request
// carries the run token, as a Bearer token or as the password of HTTP basic
// auth (which is what OpenTofu sends). It is for the runner listener only:
// never mount it on the public one.
func RunnerHandler(api run.API, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &runnerHTTP{api: api, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+run.ClaimPath, h.claim)
	mux.HandleFunc("POST "+run.EventPath, h.event)
	mux.HandleFunc("POST "+run.CompletePath, h.complete)
	mux.HandleFunc(run.StatePath, h.state) // GET, POST, LOCK and UNLOCK
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

type runnerHTTP struct {
	api run.API
	log *slog.Logger
}

// tokenChecker is an API that can tell a good run token from a bad one
// without doing anything else (*Resources).
type tokenChecker interface {
	CheckRunToken(ctx context.Context, token string) error
}

// admit checks the request's run token before its body is read, so that
// only a running run can make forge-api read megabytes. It answers the
// request and returns false when the token is not good.
func (h *runnerHTTP) admit(w http.ResponseWriter, r *http.Request) bool {
	token := runToken(r)
	var err error
	if c, ok := h.api.(tokenChecker); ok {
		err = c.CheckRunToken(r.Context(), token)
	} else if token == "" {
		err = runTokenError()
	}
	if err != nil {
		h.fail(w, r, err)
		return false
	}
	return true
}

// runToken reads the run token: "Bearer <token>", or basic auth's password.
func runToken(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return t
	}
	_, password, _ := r.BasicAuth()
	return password
}

// runnerStatus maps an error of run.API to an HTTP status.
func runnerStatus(err error) int {
	if errors.Is(err, run.ErrLocked) {
		return http.StatusLocked
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return http.StatusInternalServerError
	}
	switch ce.Code() {
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeFailedPrecondition, connect.CodeAborted:
		return http.StatusConflict
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// fail answers with err as {"error": "..."}. The message is run.API's own;
// it never holds the token.
func (h *runnerHTTP) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := runnerStatus(err)
	if status >= http.StatusInternalServerError {
		h.log.Error("runner API", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	writeJSON(w, status, map[string]string{"error": errMessage(err)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// body reads at most limit bytes of the request body.
func body(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the request body is too large"))
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the request body could not be read"))
	}
	return data, nil
}

// decode reads a JSON body of at most limit bytes into v.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	data, err := body(w, r, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the request body is not the expected JSON"))
	}
	return nil
}

func (h *runnerHTTP) claim(w http.ResponseWriter, r *http.Request) {
	work, err := h.api.Claim(r.Context(), runToken(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, work)
}

func (h *runnerHTTP) event(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Step    string `json:"step"`
		Message string `json:"message"`
	}
	if !h.admit(w, r) {
		return
	}
	if err := decode(w, r, maxRunnerSmall, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.api.Event(r.Context(), runToken(r), in.Step, in.Message); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (h *runnerHTTP) complete(w http.ResponseWriter, r *http.Request) {
	var res run.Result
	if !h.admit(w, r) {
		return
	}
	if err := decode(w, r, maxRunnerResult, &res); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.api.Complete(r.Context(), runToken(r), res); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// lockID reads the "ID" of the lock info OpenTofu sends with LOCK and UNLOCK.
func lockID(info []byte) (string, error) {
	var in struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(info, &in); err != nil || in.ID == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("the lock info has no ID"))
	}
	return in.ID, nil
}

// state is the OpenTofu "http" backend.
func (h *runnerHTTP) state(w http.ResponseWriter, r *http.Request) {
	ctx, token := r.Context(), runToken(r)
	if (r.Method == http.MethodPost || r.Method == "LOCK" || r.Method == "UNLOCK") && !h.admit(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, err := h.api.ReadState(ctx, token)
		switch {
		case err != nil:
			h.fail(w, r, err)
		case data == nil:
			w.WriteHeader(http.StatusNotFound) // OpenTofu reads 404 as "no state yet"
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data) //nolint:gosec // the run's own OpenTofu state, served as JSON to its runner
		}
	case http.MethodPost:
		data, err := body(w, r, maxRunnerState)
		if err == nil {
			err = h.api.WriteState(ctx, token, r.URL.Query().Get("ID"), data)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "LOCK":
		info, err := body(w, r, maxRunnerSmall)
		var id string
		if err == nil {
			id, err = lockID(info)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		holder, err := h.api.LockState(ctx, token, id, info)
		switch {
		case errors.Is(err, run.ErrLocked):
			// OpenTofu shows the holder's lock info to the user.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusLocked)
			_, _ = w.Write(holder) //nolint:gosec // the holder's lock info, served as JSON to the runner
		case err != nil:
			h.fail(w, r, err)
		default:
			w.WriteHeader(http.StatusOK)
		}
	case "UNLOCK":
		info, err := body(w, r, maxRunnerSmall)
		var id string
		if err == nil {
			id, err = lockID(info)
		}
		if err == nil {
			err = h.api.UnlockState(ctx, token, id)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "GET, POST, LOCK, UNLOCK")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}
