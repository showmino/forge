// Package pgtest gives tests a PostgreSQL database of their own.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// URL returns FORGE_TEST_DATABASE_URL pointed at a new, empty schema that is
// dropped when the test ends. It skips the test when the variable is unset.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("FORGE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("FORGE_TEST_DATABASE_URL is not set (make db)")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "forge_test_" + hex.EncodeToString(b)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Errorf("drop %s: %v", schema, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop %s: %v", schema, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema) // pgx sends unknown URL parameters as session settings
	u.RawQuery = q.Encode()
	return u.String()
}
