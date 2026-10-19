package arch

import (
	"testing"

	"github.com/guangliangus/go-msvc-kit/ddlguard"
)

// The team's PostgreSQL conventions (text not varchar, TIMESTAMPTZ, SERIAL, no PG enum, no physical
// delete, IF NOT EXISTS, plural table names, no reserved words, NOT NULL foreign keys, money as
// numeric) are the kit's ddlguard rules; they run on every db/*.sql here. An intentional deviation
// is declared on the offending line: `-- ddl-allow: <rule>  <reason>`; a vendored file whole:
// `-- ddl-allow-file: <reason>`.
func TestDDLConventions(t *testing.T) {
	ddlguard.Check(t, "../../db")
}
