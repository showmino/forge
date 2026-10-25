package server

import (
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The forge chart routes the API by one path per service on the host it
// shares with the dashboard; a service missing there answers with the
// dashboard's 404.
func TestChartRoutesEveryService(t *testing.T) {
	helpers, err := os.ReadFile("../../deploy/platform/chart/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("forge.v1", func(f protoreflect.FileDescriptor) bool {
		for i := range f.Services().Len() {
			n++
			path := "- /" + string(f.Services().Get(i).FullName()) + "\n"
			if !strings.Contains(string(helpers), path) {
				t.Errorf("forge.apiPaths in the chart's _helpers.tpl has no %q", strings.TrimSpace(path))
			}
		}
		return true
	})
	if n == 0 {
		t.Fatal("no forge.v1 services registered")
	}
}
