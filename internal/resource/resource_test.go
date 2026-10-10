package resource

import (
	"strings"
	"testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func decl(name, storage string, instances int32) *forgev1.ResourceDeclaration {
	return &forgev1.ResourceDeclaration{Name: name, Type: TypePostgres, Storage: storage, Instances: instances}
}

func TestResolveDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	d := decl("main", "5Gi", 0)
	d.Environments = map[string]*forgev1.ResourceOverride{"production": {Storage: "20Gi", Instances: 3}}

	stg, err := Resolve([]*forgev1.ResourceDeclaration{d}, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(stg) != 1 || stg[0].GetName() != "main" || stg[0].GetType() != "postgres" || stg[0].GetStorageGi() != 5 || stg[0].GetInstances() != 1 {
		t.Errorf("staging: %v", stg)
	}
	prod, err := Resolve([]*forgev1.ResourceDeclaration{d}, "production")
	if err != nil {
		t.Fatal(err)
	}
	if prod[0].GetStorageGi() != 20 || prod[0].GetInstances() != 3 {
		t.Errorf("production: %v", prod)
	}

	// No storage and no instances: 1Gi, and 1 instance in staging, 2 in production.
	bare := []*forgev1.ResourceDeclaration{decl("main", "", 0)}
	if s, _ := Resolve(bare, "staging"); s[0].GetStorageGi() != 1 || s[0].GetInstances() != 1 {
		t.Errorf("bare staging: %v", s)
	}
	if s, _ := Resolve(bare, "production"); s[0].GetStorageGi() != 1 || s[0].GetInstances() != 2 {
		t.Errorf("bare production: %v", s)
	}

	// An override with only one value keeps the other from the top level.
	half := decl("main", "5Gi", 2)
	half.Environments = map[string]*forgev1.ResourceOverride{"production": {Instances: 3}, "staging": nil}
	if s, _ := Resolve([]*forgev1.ResourceDeclaration{half}, "production"); s[0].GetStorageGi() != 5 || s[0].GetInstances() != 3 {
		t.Errorf("half override: %v", s)
	}
	if s, err := Resolve([]*forgev1.ResourceDeclaration{half}, "staging"); err != nil || s[0].GetInstances() != 2 {
		t.Errorf("nil override: %v %v", s, err)
	}

	// An empty list is valid: it plans the removal of everything.
	if s, err := Resolve(nil, "staging"); err != nil || len(s) != 0 {
		t.Errorf("empty: %v %v", s, err)
	}
}

func TestResolveRejects(t *testing.T) {
	t.Parallel()
	over := func(env, storage string, instances int32) *forgev1.ResourceDeclaration {
		d := decl("main", "5Gi", 1)
		d.Environments = map[string]*forgev1.ResourceOverride{env: {Storage: storage, Instances: instances}}
		return d
	}
	for name, tc := range map[string]struct {
		decls []*forgev1.ResourceDeclaration
		want  string
	}{
		"storage without a unit":  {[]*forgev1.ResourceDeclaration{decl("main", "5", 1)}, `main: storage "5" must be whole gibibytes such as 5Gi`},
		"storage in GB":           {[]*forgev1.ResourceDeclaration{decl("main", "5GB", 1)}, `main: storage "5GB"`},
		"zero storage":            {[]*forgev1.ResourceDeclaration{decl("main", "0Gi", 1)}, `main: storage "0Gi"`},
		"a 20-digit size":         {[]*forgev1.ResourceDeclaration{decl("main", "99999999999999999999Gi", 1)}, `main: storage "99999999999999999999Gi"`},
		"fractional storage":      {[]*forgev1.ResourceDeclaration{decl("main", "1.5Gi", 1)}, `main: storage "1.5Gi"`},
		"negative instances":      {[]*forgev1.ResourceDeclaration{decl("main", "5Gi", -1)}, "main: instances -1 must be 1 to 3"},
		"four instances":          {[]*forgev1.ResourceDeclaration{decl("main", "5Gi", 4)}, "main: instances 4 must be 1 to 3"},
		"an unknown environment":  {[]*forgev1.ResourceDeclaration{over("prod", "5Gi", 1)}, "main: environments.prod must be staging or production"},
		"a bad override":          {[]*forgev1.ResourceDeclaration{over("production", "big", 2)}, `main: environments.production: storage "big"`},
		"a bad override instance": {[]*forgev1.ResourceDeclaration{over("staging", "5Gi", 9)}, "main: environments.staging: instances 9 must be 1 to 3"},
		"an empty entry":          {[]*forgev1.ResourceDeclaration{nil}, "entry 1 is empty"},
		"a duplicate name":        {[]*forgev1.ResourceDeclaration{decl("main", "", 0), decl("main", "", 0)}, `name "main" is used twice`},
		"two databases":           {[]*forgev1.ResourceDeclaration{decl("a", "", 0), decl("b", "", 0)}, "only one postgres resource per service is supported"},
		"an uppercase name":       {[]*forgev1.ResourceDeclaration{decl("Main", "", 0)}, `name "Main" must be a lowercase DNS label of at most 30 characters`},
		"a long name":             {[]*forgev1.ResourceDeclaration{decl(strings.Repeat("a", 31), "", 0)}, "must be a lowercase DNS label of at most 30 characters"},
		"no name":                 {[]*forgev1.ResourceDeclaration{decl("", "", 0)}, `name "" must be`},
		"another type":            {[]*forgev1.ResourceDeclaration{{Name: "cache", Type: "redis"}}, `cache: type "redis" must be postgres`},
		"no type":                 {[]*forgev1.ResourceDeclaration{{Name: "main"}}, `main: type "" must be postgres`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A bad value is refused whichever environment is asked for.
			for _, env := range []string{"staging", "production"} {
				_, err := Resolve(tc.decls, env)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: %v, want it to contain %q", env, err, tc.want)
				}
			}
			if err := Validate(tc.decls); err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
	if _, err := Resolve(nil, "dev"); err == nil || !strings.Contains(err.Error(), `environment "dev" must be staging or production`) {
		t.Errorf("unknown environment: %v", err)
	}
}

func TestDigest(t *testing.T) {
	t.Parallel()
	a := []*forgev1.ResourceSettings{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}
	b := []*forgev1.ResourceSettings{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}
	if Digest(a) != Digest(b) || len(Digest(a)) != 64 {
		t.Errorf("equal settings: %s %s", Digest(a), Digest(b))
	}
	b[0].StorageGi = 6
	if Digest(a) == Digest(b) {
		t.Error("a changed size must change the digest")
	}
	if Digest(nil) == Digest(a) {
		t.Error("the empty list must differ")
	}
}
