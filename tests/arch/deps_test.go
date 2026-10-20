// Package arch holds the guards that keep the dependency direction, the DDL conventions and the
// copies of the kit's tables honest. They run with `go test ./...`; a violation fails the build,
// not the review. (That the kit never imports the service needs no guard: it is another module,
// and Go does not let it import this one's internal/.)
package arch

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "example.com/msvcservice"

// The packages the guards reason about, by role — edit this when you add/rename a context, an
// adapter or an entry point. Every name is a directory under internal/ and TestGuardedPackagesExist
// fails when one is missing: `go list` prints only a warning for a pattern that matches nothing,
// and a rule checked over zero packages passes without checking anything.
var (
	// businessContexts declare their ports and import no driver and no adapter.
	businessContexts = []string{"order"}
	// adapters implement the contexts' ports; only the composition root may import them.
	adapters = []string{"postgres", "inventory"}
	// entryPoints drive the contexts' services: handlers, consumers, tasks.
	entryPoints = []string{"httpapi", "consumers", "tasks"}
	// compositionRoot is the one package that sees ports and implementations together.
	compositionRoot = "app"
)

type pkgInfo struct {
	ImportPath string
	Imports    []string
}

func internalPkg(name string) string { return module + "/internal/" + name }

// TestGuardedPackagesExist keeps the table above honest: a renamed directory must be renamed here
// too, or the rules below would silently stop covering it.
func TestGuardedPackagesExist(t *testing.T) {
	names := slices.Concat(businessContexts, adapters, entryPoints, []string{compositionRoot})
	for _, name := range names {
		dir := filepath.Join("..", "..", "internal", name)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("internal/%s is in the guard table but is not a directory; update the table", name)
		}
	}
}

// listPackages resolves a pattern with `go list`, which exits 0 with only a warning when nothing
// matches; a guard over an empty set would pass vacuously, so that is a failure here.
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
	if len(pkgs) == 0 {
		t.Fatalf("go list %s matched no packages; the guard would pass without checking anything", pattern)
	}
	return pkgs
}

// forbid checks DIRECT imports: what a package's own code reaches for. Transitive dependencies
// through the kit (the business package imports the kit's messaging, which imports the database)
// are the kit's business, not a layering violation.
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

// The business package declares its ports and imports no driver and no adapter: every dependency
// arrow points at it, none points out of it.
func TestBusinessPackageImportsNoDriverOrAdapter(t *testing.T) {
	others := slices.Concat(adapters, entryPoints, []string{compositionRoot})
	for _, business := range businessContexts {
		pkgs := listPackages(t, internalPkg(business)+"/...")
		for _, other := range others {
			forbid(t, pkgs, internalPkg(other), "internal/"+business+" must not import internal/"+other)
		}
		forbid(t, pkgs, "github.com/jackc/pgx", "no database driver in the business package")
		forbid(t, pkgs, "github.com/rabbitmq/amqp091-go", "no broker client in the business package")
		forbid(t, pkgs, "github.com/twmb/franz-go", "no broker client in the business package")
		forbid(t, pkgs, "gorm.io", "no ORM in the business package: GORM is internal/postgres's detail")
		forbid(t, pkgs, "github.com/go-chi", "no router in the business package")
	}
}

// Handlers, consumers and tasks go through the context's service; they never see a repository or
// a driver.
func TestEntryPointsUseTheService(t *testing.T) {
	for _, entry := range entryPoints {
		pkgs := listPackages(t, internalPkg(entry)+"/...")
		for _, adapter := range adapters {
			forbid(t, pkgs, internalPkg(adapter), entry+" uses the context's service, never internal/"+adapter)
		}
		forbid(t, pkgs, "github.com/jackc/pgx", entry+" never touches the database")
		forbid(t, pkgs, "gorm.io", entry+" never touches the database")
	}
}

// Only the composition root may know every adapter.
func TestOnlyTheCompositionRootWiresAdapters(t *testing.T) {
	for _, p := range listPackages(t, module+"/internal/...") {
		if strings.HasPrefix(p.ImportPath, internalPkg(compositionRoot)) {
			continue
		}
		for _, imp := range p.Imports {
			for _, adapter := range adapters {
				if imp == internalPkg(adapter) {
					t.Errorf("%s imports %s directly; adapters are wired in internal/%s",
						p.ImportPath, imp, compositionRoot)
				}
			}
		}
	}
}
