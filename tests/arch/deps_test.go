// Package arch holds the guards that keep the dependency direction and the DDL conventions
// honest. They run with `go test ./...`; a violation fails the build, not the review.
package arch

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const module = "example.com/msvcservice"

type pkgInfo struct {
	ImportPath string
	Imports    []string
}

func listPackages(t *testing.T, pattern string) []pkgInfo {
	t.Helper()
	cmd := exec.Command("go", "list", "-json=ImportPath,Imports", pattern)
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pattern, err)
	}
	var pkgs []pkgInfo
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// forbid checks DIRECT imports: what a package's own code reaches for. Transitive dependencies
// through the kit (the business package imports pkg/messaging, which imports the database) are
// the kit's business, not a layering violation.
func forbid(t *testing.T, pkgs []pkgInfo, forbiddenPrefix, why string) {
	t.Helper()
	for _, p := range pkgs {
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, forbiddenPrefix) {
				t.Errorf("%s imports %s: %s", p.ImportPath, imp, why)
			}
		}
	}
}

// The kit (pkg/) is what will be extracted into a shared module one day: it must never reach
// into the service.
func TestKitDoesNotDependOnTheService(t *testing.T) {
	forbid(t, listPackages(t, module+"/pkg/..."), module+"/internal", "pkg/ must not import internal/")
}

// The business package declares its ports and imports no driver and no adapter: every dependency
// arrow points at it, none points out of it.
func TestBusinessPackageImportsNoDriverOrAdapter(t *testing.T) {
	business := listPackages(t, module+"/internal/order/...")
	for _, other := range []string{"postgres", "inventory", "httpapi", "consumers", "tasks", "app"} {
		forbid(t, business, module+"/internal/"+other, "internal/order must not import internal/"+other)
	}
	forbid(t, business, "github.com/jackc/pgx", "no database driver in the business package")
	forbid(t, business, "github.com/rabbitmq/amqp091-go", "no broker client in the business package")
	forbid(t, business, "github.com/go-chi", "no router in the business package")
}

// Handlers, consumers and tasks go through order.Service; they never see a repository or a driver.
func TestEntryPointsUseTheService(t *testing.T) {
	for _, entry := range []string{"httpapi", "consumers", "tasks"} {
		pkgs := listPackages(t, module+"/internal/"+entry+"/...")
		forbid(t, pkgs, module+"/internal/postgres", entry+" uses order.Service, never a repository")
		forbid(t, pkgs, module+"/internal/inventory", entry+" uses order.Service, never a downstream adapter")
		forbid(t, pkgs, "github.com/jackc/pgx", entry+" never touches the database")
	}
}

// Only the composition root may know every adapter.
func TestOnlyTheCompositionRootWiresAdapters(t *testing.T) {
	adapters := []string{module + "/internal/postgres", module + "/internal/inventory"}
	for _, p := range listPackages(t, module+"/internal/...") {
		if strings.HasPrefix(p.ImportPath, module+"/internal/app") {
			continue
		}
		for _, imp := range p.Imports {
			for _, adapter := range adapters {
				if imp == adapter {
					t.Errorf("%s imports %s directly; adapters are wired in internal/app", p.ImportPath, imp)
				}
			}
		}
	}
}
