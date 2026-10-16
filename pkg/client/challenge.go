package client

import (
	"fmt"
	"net/http"
)

// ChallengeError reports that a proxy in front of Forge, Cloudflare, answered
// with a challenge meant for a browser instead of passing the request on.
// Forge never saw the request.
type ChallengeError struct {
	Host  string
	RayID string // Cloudflare's Ray ID, which finds the request in its security events
}

func (e *ChallengeError) Error() string {
	msg := fmt.Sprintf("Cloudflare challenged the request to %s from your network, and a command line cannot solve the challenge; Forge never saw the request", e.Host)
	if e.RayID != "" {
		msg += fmt.Sprintf(" (Ray ID %s)", e.RayID)
	}
	return msg
}

// DetectChallenges wraps rt (http.DefaultTransport when nil) so that a
// Cloudflare challenge becomes a *ChallengeError instead of a 403 or 503
// that reads like Forge's own answer.
func DetectChallenges(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return challengeTransport{rt}
}

type challengeTransport struct{ next http.RoundTripper }

func (t challengeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.Header.Get("Cf-Mitigated") != "challenge" {
		return resp, err
	}
	_ = resp.Body.Close()
	return nil, &ChallengeError{Host: req.URL.Host, RayID: resp.Header.Get("Cf-Ray")}
}
