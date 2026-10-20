package postgres

import (
	"context"
	"fmt"
	"time"

	gormpg "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	kitpg "github.com/guangliangus/go-msvc-kit/postgres"
)

// OpenGorm opens GORM on the kit's database/sql handle: the same pgx pool, so the pool metrics,
// the slow-query log, statement_timeout and the traces all cover what GORM sends, and the same
// transactions (see Gorm.On). The schema is NEVER touched from here: db/*.sql is applied by hand,
// so AutoMigrate is never called and the models below are mappings of that DDL, not its source.
func OpenGorm(db *kitpg.DB) (*Gorm, error) {
	gdb, err := gorm.Open(gormpg.New(gormpg.Config{Conn: db.SQL()}), &gorm.Config{
		// Transactions belong to the kit's db.WithTx: GORM opening its own around every write would
		// commit a business row apart from the outbox row the same use case writes.
		SkipDefaultTransaction: true,
		// Name every column instead of SELECT *: a struct and its table can differ (a column GORM
		// does not map, a field the table lacks shows as an error rather than a silent zero).
		QueryFields: true,
		// Timestamps GORM fills (created_at / updated_at) in UTC at the database's precision, so a
		// value read back equals the one written.
		NowFunc: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		// The kit's pgx tracer already logs slow statements and traces every one GORM sends; GORM's
		// own logger would print each a second time. Errors are returned to the caller either way.
		Logger: logger.Discard,
		// The pool tolerates a database that is down at start (readiness reports it); GORM's ping
		// would turn that into a refused boot.
		DisableAutomaticPing: true,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: open gorm: %w", err)
	}
	return &Gorm{db: gdb}, nil
}

// Gorm is the service's GORM handle. Repositories never use it directly: On binds it to the
// transaction the context carries.
type Gorm struct {
	db *gorm.DB
}

// On returns a GORM session for ctx: inside db.WithTx it runs on that transaction — the business
// rows GORM writes commit or roll back together with the outbox, inbox and task rows the kit
// writes on the same transaction — and outside one on the pool. Never call GORM's own Transaction
// or Begin: the kit owns the transaction, and a second one would commit apart from it.
func (g *Gorm) On(ctx context.Context) *gorm.DB {
	session := g.db.Session(&gorm.Session{NewDB: true, Context: ctx})
	if tx, ok := kitpg.TxFromContext(ctx); ok {
		session.Statement.ConnPool = tx
	}
	return session
}

// Models lists every GORM model of this package, as pointers to empty slices, for the mapping
// guard in tests/integration: it runs one query per model against the schema db/*.sql builds, so a
// column the model names but the table lacks fails there rather than in production. Add every new
// model here.
func Models() map[string]any {
	return map[string]any{
		"orders":      &[]orderModel{},
		"order_items": &[]orderItemModel{},
	}
}
