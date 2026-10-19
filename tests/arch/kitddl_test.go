package arch

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	kitdb "github.com/guangliangus/go-msvc-kit/db"
)

// The kit owns the DDL of the tables it reads and writes (outbox, inbox, task queue). The service
// keeps byte-identical copies in db/, because db/ is what the DBA applies by hand; upgrading the
// kit past a schema change fails here until the copies follow (the kit's CHANGELOG, "DDL").
func TestKitTablesAreTheKitsScripts(t *testing.T) {
	names, err := fs.Glob(kitdb.Scripts, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("the kit embeds no DDL: %v", err)
	}
	const copyHint = `cp "$(go list -m -f '{{.Dir}}' github.com/guangliangus/go-msvc-kit)/db/%s" db/`
	for _, name := range names {
		want, err := fs.ReadFile(kitdb.Scripts, name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join("..", "..", "db", name))
		switch {
		case err != nil:
			t.Errorf("db/%s is missing; copy the kit's: "+copyHint, name, name)
		case !bytes.Equal(got, want):
			t.Errorf("db/%s differs from the kit's version; copy it and have the DBA apply it: "+copyHint, name, name)
		default:
		}
	}
}
