// Package client is the Go client for the Forge API. It wraps the generated
// Connect clients with authentication, version headers, request IDs and
// retries, so CLI commands never build requests by hand.
package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"

	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
)

// Header names shared by client and server.
const (
	HeaderClientVersion    = "Forge-Client-Version"
	HeaderServerVersion    = "Forge-Server-Version"
	HeaderMinClientVersion = "Forge-Min-Client-Version"
	HeaderRequestID        = "X-Request-Id"
)

// Options configure a Client.
type Options struct {
	// HTTPClient defaults to a client with sane timeouts.
	HTTPClient *http.Client
	// TokenSource supplies bearer tokens. Nil sends no credentials.
	TokenSource oauth2.TokenSource
	// ClientVersion is sent in the Forge-Client-Version header.
	ClientVersion string
	// UserAgent defaults to "forge-cli/<ClientVersion>".
	UserAgent string
	// MaxRetries for side-effect-free calls that fail with Unavailable. Default 3; negative disables.
	MaxRetries int
	// OnResponse, if set, is called with the response headers of every call
	// (for version-skew warnings and -v output).
	OnResponse func(procedure string, h http.Header)
	// GRPC selects the gRPC protocol instead of the Connect protocol.
	GRPC bool
}

// Client bundles the Forge API service clients.
type Client struct {
	Catalog      forgev1connect.ServiceCatalogServiceClient
	Auth         forgev1connect.AuthServiceClient
	Templates    forgev1connect.TemplateServiceClient
	Operations   forgev1connect.OperationServiceClient
	Environments forgev1connect.EnvironmentServiceClient
	Deploys      forgev1connect.DeployServiceClient
	BaseURL      string
}

// New returns a client for the API at baseURL, for example "https://forge.example.com".
func New(baseURL string, o Options) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("API URL %q must start with http:// or https://", baseURL)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 3
	}
	if o.UserAgent == "" {
		o.UserAgent = "forge-cli/" + o.ClientVersion
	}
	opts := []connect.ClientOption{
		connect.WithInterceptors(&interceptor{o: o}),
		connect.WithHTTPGet(), // side-effect-free RPCs use GET, which proxies can cache and retry.
	}
	if o.GRPC {
		opts = append(opts, connect.WithGRPC())
	}
	return &Client{
		Catalog:      forgev1connect.NewServiceCatalogServiceClient(hc, baseURL, opts...),
		Auth:         forgev1connect.NewAuthServiceClient(hc, baseURL, opts...),
		Templates:    forgev1connect.NewTemplateServiceClient(hc, baseURL, opts...),
		Operations:   forgev1connect.NewOperationServiceClient(hc, baseURL, opts...),
		Environments: forgev1connect.NewEnvironmentServiceClient(hc, baseURL, opts...),
		Deploys:      forgev1connect.NewDeployServiceClient(hc, baseURL, opts...),
		BaseURL:      baseURL,
	}, nil
}

type interceptor struct{ o Options }

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h := req.Header()
		h.Set(HeaderClientVersion, i.o.ClientVersion)
		h.Set("User-Agent", i.o.UserAgent)
		if h.Get(HeaderRequestID) == "" {
			h.Set(HeaderRequestID, NewRequestID())
		}
		if i.o.TokenSource != nil {
			tok, err := i.o.TokenSource.Token()
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("get credentials: %w", err))
			}
			h.Set("Authorization", "Bearer "+tok.AccessToken)
		}

		retryable := req.Spec().IdempotencyLevel == connect.IdempotencyNoSideEffects
		var (
			resp connect.AnyResponse
			err  error
		)
		for attempt := 0; ; attempt++ {
			resp, err = next(ctx, req)
			if err == nil || !retryable || attempt >= i.o.MaxRetries || connect.CodeOf(err) != connect.CodeUnavailable {
				break
			}
			if werr := sleep(ctx, backoff(attempt)); werr != nil {
				return nil, errors.Join(err, werr)
			}
		}
		if i.o.OnResponse != nil {
			var rh http.Header
			if resp != nil {
				rh = resp.Header()
			} else {
				var ce *connect.Error
				if errors.As(err, &ce) {
					rh = ce.Meta()
				}
			}
			if rh != nil {
				i.o.OnResponse(req.Spec().Procedure, rh)
			}
		}
		return resp, err
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		h := conn.RequestHeader()
		h.Set(HeaderClientVersion, i.o.ClientVersion)
		h.Set("User-Agent", i.o.UserAgent)
		h.Set(HeaderRequestID, NewRequestID())
		if i.o.TokenSource != nil {
			if tok, err := i.o.TokenSource.Token(); err == nil {
				h.Set("Authorization", "Bearer "+tok.AccessToken)
			}
		}
		return conn
	}
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// backoff is full-jitter exponential backoff: random in [0, 200ms * 2^attempt), capped at 3s.
func backoff(attempt int) time.Duration {
	ceil := min(200*time.Millisecond<<attempt, 3*time.Second)
	n, err := rand.Int(rand.Reader, big.NewInt(int64(ceil)))
	if err != nil {
		return ceil / 2
	}
	return time.Duration(n.Int64())
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewRequestID returns a random 16-byte hex ID, also used as an idempotency key.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
