package splitter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

func TestControllerWorkerPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool string
	}{
		{name: "public"},
		{name: "private", pool: "private-worker-pool"},
		{name: "literal", pool: "pool-\"${not_an_expression}\"\nnext-line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"worker_pool_id": tc.pool})
			if err != nil {
				t.Fatal(err)
			}
			spec := layout{SourceStackID: "source-admin", SpaceID: "root", RoleID: "space-admin-role", Repository: "admin-config", Branch: "main", ToolVersion: "1.10.6"}
			if err := json.Unmarshal(raw, &spec); err != nil {
				t.Fatal(err)
			}
			repo := t.TempDir()
			writeTest(t, filepath.Join(repo, "versions.tf"), versions)
			groups := []preparedGroup{{group: group{Name: "team_a"}}, {group: group{Name: "team_b"}}}
			if _, err := renderController(repo, spec, groups); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(repo, "admin_split_controller.tf")
			raw, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			file, diags := hclsyntax.ParseConfig(raw, path, hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			stacks := 0
			for _, block := range file.Body.(*hclsyntax.Body).Blocks {
				if block.Type != "resource" || block.Labels[0] != "spacelift_stack" {
					continue
				}
				stacks++
				attr := block.Body.Attributes["worker_pool_id"]
				if tc.pool == "" {
					if attr != nil {
						t.Fatal("omitted worker pool must preserve the public-pool default")
					}
					continue
				}
				if attr == nil {
					t.Fatalf("child %s has no worker pool", block.Labels[1])
				}
				value, diags := attr.Expr.Value(nil)
				if diags.HasErrors() || !value.RawEquals(cty.StringVal(tc.pool)) {
					t.Fatalf("child %s did not preserve the literal worker pool", block.Labels[1])
				}
			}
			if stacks != len(groups) {
				t.Fatalf("rendered %d children, want %d", stacks, len(groups))
			}
		})
	}
}
