package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Paths of the runner API on forge-api's runner listener.
const (
	ClaimPath    = "/runner/v1/claim"
	EventPath    = "/runner/v1/event"
	CompletePath = "/runner/v1/complete"
	// StatePath is the OpenTofu "http" backend: GET reads the state, POST
	// writes it (?ID=<lock id>), LOCK and UNLOCK take a JSON body with "ID".
	StatePath = "/runner/v1/state"
)

// StateUser is the user name OpenTofu sends with the run token as password.
const StateUser = "forge-runner"

// HTTPError is an answer of the runner API that is not a success.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("runner API: %s (HTTP %d)", e.Message, e.Status)
}

// Client is the runner API over HTTP. It implements API.
type Client struct {
	// BaseURL is forge-api's runner listener, e.g. http://forge-api-runner.forge.svc:8081.
	BaseURL string
	// HTTP defaults to a client with a two-minute timeout.
	HTTP *http.Client
}

var defaultHTTP = &http.Client{Timeout: 2 * time.Minute}

// StateURL is the address OpenTofu's "http" backend is given.
func (c *Client) StateURL() string { return strings.TrimRight(c.BaseURL, "/") + StatePath }

// do sends one request with the run token and returns the answer's body.
// lockedOK makes HTTP 423 an answer rather than an error.
func (c *Client) do(ctx context.Context, method, path, token string, query url.Values, body []byte, lockedOK bool) (int, []byte, error) {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error holds the URL, never the header: safe to return.
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil, err
	}
	switch {
	case resp.StatusCode/100 == 2, resp.StatusCode == http.StatusNotFound && method == http.MethodGet,
		resp.StatusCode == http.StatusLocked && lockedOK:
		return resp.StatusCode, data, nil
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = http.StatusText(resp.StatusCode)
	}
	return resp.StatusCode, nil, &HTTPError{Status: resp.StatusCode, Message: e.Error}
}

func (c *Client) post(ctx context.Context, path, token string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, data, err := c.do(ctx, http.MethodPost, path, token, nil, body, false)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// Claim implements API.
func (c *Client) Claim(ctx context.Context, token string) (*Work, error) {
	var w Work
	if err := c.post(ctx, ClaimPath, token, struct{}{}, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// Event implements API.
func (c *Client) Event(ctx context.Context, token, step, message string) error {
	return c.post(ctx, EventPath, token, map[string]string{"step": step, "message": message}, nil)
}

// Complete implements API.
func (c *Client) Complete(ctx context.Context, token string, r Result) error {
	return c.post(ctx, CompletePath, token, r, nil)
}

// ReadState implements API.
func (c *Client) ReadState(ctx context.Context, token string) ([]byte, error) {
	status, data, err := c.do(ctx, http.MethodGet, StatePath, token, nil, nil, false)
	if err != nil || status == http.StatusNotFound {
		return nil, err
	}
	return data, nil
}

// WriteState implements API.
func (c *Client) WriteState(ctx context.Context, token, lockID string, data []byte) error {
	q := url.Values{}
	if lockID != "" {
		q.Set("ID", lockID)
	}
	status, _, err := c.do(ctx, http.MethodPost, StatePath, token, q, data, true)
	if err == nil && status == http.StatusLocked {
		return ErrLocked
	}
	return err
}

// lockBody is the JSON OpenTofu sends with LOCK and UNLOCK: info when it
// already holds the id, otherwise just the id.
func lockBody(lockID string, info []byte) []byte {
	var have struct {
		ID string `json:"ID"`
	}
	if json.Unmarshal(info, &have) == nil && have.ID == lockID && lockID != "" {
		return info
	}
	b, _ := json.Marshal(map[string]string{"ID": lockID})
	return b
}

// LockState implements API.
func (c *Client) LockState(ctx context.Context, token, lockID string, info []byte) ([]byte, error) {
	status, data, err := c.do(ctx, "LOCK", StatePath, token, nil, lockBody(lockID, info), true)
	if err != nil {
		return nil, err
	}
	if status == http.StatusLocked {
		return data, ErrLocked
	}
	return nil, nil
}

// UnlockState implements API.
func (c *Client) UnlockState(ctx context.Context, token, lockID string) error {
	status, _, err := c.do(ctx, "UNLOCK", StatePath, token, nil, lockBody(lockID, nil), true)
	if err == nil && status == http.StatusLocked {
		return ErrLocked
	}
	return err
}

// IsStatus reports whether err is an HTTPError with status.
func IsStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}
