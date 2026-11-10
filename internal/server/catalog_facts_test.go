package server

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
)

// A service's tier and SLO (phase 15): set from forge.yaml at create, and
// changed by its team like any other service setting.
func TestServiceTierAndSLO(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	s := register(t, c, "payments")
	upd := func(tier int32, slo string, paths ...string) (*forgev1.Service, error) {
		resp, err := c.UpdateService(writerCtx(), connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service:    &forgev1.Service{Name: s.GetName(), Tier: tier, Slo: slo},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetService(), nil
	}
	got, err := upd(1, "99.95%", "tier", "slo")
	if err != nil || got.GetTier() != 1 || got.GetSlo() != "99.95%" {
		t.Fatalf("update: %v %v", got, err)
	}
	for _, bad := range []struct {
		tier int32
		slo  string
	}{{4, "99.9%"}, {-1, "99.9%"}, {1, "99.9"}, {1, "101%"}} {
		if _, err := upd(bad.tier, bad.slo, "tier", "slo"); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("tier %d slo %q: %v", bad.tier, bad.slo, err)
		}
	}
	// Clearing both: not set.
	if got, err := upd(0, "", "tier", "slo"); err != nil || got.GetTier() != 0 || got.GetSlo() != "" {
		t.Errorf("clear: %v %v", got, err)
	}

	m := &manifest.Manifest{Service: manifest.Service{Team: "billing", Tier: 2, SLO: "99.9%"}}
	if from := serviceFromManifest(m); from.GetTier() != 2 || from.GetSlo() != "99.9%" {
		t.Errorf("from forge.yaml: %v", from)
	}
}
