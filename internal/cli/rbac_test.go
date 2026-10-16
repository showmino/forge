package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/server"
)

// TestTeamRoles runs the spec's table through the CLI: a member of the
// owner team works on the team's services, and only an admin does the rest.
func TestTeamRoles(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("service", "register", "payments", "--team", "billing")
	h.ok("service", "register", "web", "--team", "frontend")

	h.env["FORGE_TOKEN"] = teamToken
	h.ok("service", "update", "payments", "--lifecycle", "production")
	r := h.fails(5, "service", "update", "web", "--lifecycle", "production")
	if !strings.Contains(r.err, "service.update") || !strings.Contains(r.err, "billing") && !strings.Contains(r.err, "frontend") {
		t.Errorf("refusal names neither action nor team: %v", r)
	}
	h.fails(5, "service", "update", "payments", "--team", "frontend")
	h.fails(5, "service", "delete", "payments", "--yes")
	h.fails(5, "service", "register", "other", "--team", "billing")

	if r := h.ok("auth", "can", "deploy.promote", "payments"); r.out != "yes\n" {
		t.Errorf("can promote own: %v", r)
	}
	r = h.fails(5, "auth", "can", "deploy.promote", "web")
	if r.out != "no\n" || !strings.Contains(r.err, "deploy.promote") {
		t.Errorf("can promote other: %v", r)
	}
	h.fails(5, "auth", "can", "service.delete", "payments")
	h.ok("auth", "can", "service.create", "--team", "billing")
	h.fails(5, "auth", "can", "service.create", "--team", "frontend")
	h.fails(2, "auth", "can", "deploy.everything", "payments")
	h.fails(2, "auth", "can")
	h.fails(3, "auth", "can", "deploy.promote", "nope")

	r = h.fails(5, "auth", "can", "deploy.promote", "web", "-o", "json")
	var can authCan
	if err := json.Unmarshal([]byte(r.out), &can); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if can != (authCan{Action: "deploy.promote", Service: "services/web", Allowed: false, Role: "reader"}) {
		t.Errorf("json: %+v", can)
	}

	r = h.ok("service", "get", "payments")
	if !strings.Contains(r.out, "you can") || !strings.Contains(r.out, "deploy.promote") || strings.Contains(r.out, "service.delete") {
		t.Errorf("service get:\n%s", r.out)
	}
	if r := h.ok("service", "get", "payments", "-o", "json"); strings.Contains(r.out, "deploy.promote") {
		t.Errorf("json is the resource itself:\n%s", r.out)
	}

	r = h.ok("auth", "status")
	if !strings.Contains(r.out, "role") || !strings.Contains(r.out, "team: billing") {
		t.Errorf("status:\n%s", r.out)
	}
	r = h.ok("auth", "status", "-o", "json")
	var st authStatus
	if err := json.Unmarshal([]byte(r.out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Admin || len(st.Teams) != 1 || st.Teams[0] != "billing" {
		t.Errorf("status json: %+v", st)
	}

	h.env["FORGE_TOKEN"] = writerToken
	if r := h.ok("auth", "can", "service.delete", "web"); r.out != "yes\n" {
		t.Errorf("admin: %v", r)
	}
	if r := h.ok("auth", "status"); !strings.Contains(r.out, "admin") {
		t.Errorf("admin status:\n%s", r.out)
	}
}

// TestAuthCanHelpListsEveryAction keeps the help of 'forge auth can' in step
// with the API's actions.
func TestAuthCanHelpListsEveryAction(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.ok("auth", "can", "--help")
	help := strings.Join(strings.Fields(r.out), " ")
	for _, act := range server.Actions() {
		if !strings.Contains(help, " "+act+",") && !strings.Contains(help, " "+act+" ") {
			t.Errorf("help does not list %s", act)
		}
	}
}
