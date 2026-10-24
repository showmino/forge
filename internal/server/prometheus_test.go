package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrometheusQuery(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "moved":
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		case "error":
			_, _ = w.Write([]byte(`{"status":"error","error":"bad query"}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"n":"ok"},"value":[1,"42"]},
				{"metric":{"n":"nan"},"value":[1,"NaN"]},
				{"metric":{"n":"inf"},"value":[1,"+Inf"]},
				{"metric":{"n":"neg"},"value":[1,"-5"]},
				{"metric":{"n":"huge"},"value":[1,"1e30"]}]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := &Prometheus{URL: srv.URL + "/"}

	got, err := p.query(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	// Only byte counts that fit an int64 are kept.
	if len(got) != 1 || got[0].labels["n"] != "ok" || got[0].value != 42 {
		t.Errorf("samples: %v", got)
	}
	if _, err := p.query(context.Background(), "error"); err == nil || !contains(err, "bad query") {
		t.Errorf("error answer: %v", err)
	}
	if _, err := p.query(context.Background(), "moved"); err == nil {
		t.Error("followed a redirect away from the configured server")
	}
}
