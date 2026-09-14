package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openRawSQLite opens a sqlite database at path WITHOUT running migrations.
// The tests below need to seed legacy schemas before the migration ledger
// exists, which the normal Open() flow does not allow.
func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("raw sqlite open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newSQLiteStore(t *testing.T, db *sql.DB) *sqlStore {
	t.Helper()
	return &sqlStore{db: db, dialect: "sqlite"}
}

func countLedger(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

// A pre-1.0 Antares binary shipped vps_hosts without a host_key column.
// Adopting such a database must keep every existing row intact AND add the
// column, without relying on error-message pattern matching.
func TestMigrateAdoptsLegacyVpsHostsSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openRawSQLite(t, path)

	// Seed the legacy vps_hosts schema (no host_key column) and one row.
	legacy := `CREATE TABLE vps_hosts (
		id           TEXT PRIMARY KEY,
		label        TEXT NOT NULL DEFAULT '',
		host         TEXT NOT NULL,
		port         INTEGER NOT NULL DEFAULT 22,
		username     TEXT NOT NULL DEFAULT 'root',
		auth_method  TEXT NOT NULL DEFAULT 'password',
		password     TEXT NOT NULL DEFAULT '',
		private_key  TEXT NOT NULL DEFAULT '',
		passphrase   TEXT NOT NULL DEFAULT '',
		created_at   BIGINT NOT NULL,
		updated_at   BIGINT NOT NULL
	)`
	if _, err := db.ExecContext(ctx, legacy); err != nil {
		t.Fatalf("seed legacy vps_hosts: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO vps_hosts(id, host, created_at, updated_at) VALUES ('h1', '10.0.0.1', 1, 1)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	s := newSQLiteStore(t, db)
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Row survived and host_key defaulted to ''.
	var host, hostKey string
	if err := db.QueryRowContext(ctx, `SELECT host, host_key FROM vps_hosts WHERE id = 'h1'`).Scan(&host, &hostKey); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if host != "10.0.0.1" || hostKey != "" {
		t.Fatalf("row not preserved: host=%q host_key=%q", host, hostKey)
	}
	wantLedger := len(baselineMigrations("sqlite"))
	if got := countLedger(t, db); got != wantLedger {
		t.Fatalf("ledger rows: want %d, got %d", wantLedger, got)
	}

	// Second run is a no-op — the ALTER is skipped by the column probe, not by
	// swallowing a duplicate-column error.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if got := countLedger(t, db); got != wantLedger {
		t.Fatalf("ledger rows after re-migrate: want %d, got %d", wantLedger, got)
	}
}

// A fresh Open() followed by another Open() on the same file must be a
// no-op — the ledger already records every baseline version with matching
// checksums.
func TestMigrateRepeatStartupIsNoop(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "repeat.db")

	s1, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	s1.Close()

	s2, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	defer s2.Close()

	raw := openRawSQLite(t, dsn)
	wantLedger := len(baselineMigrations("sqlite"))
	if got := countLedger(t, raw); got != wantLedger {
		t.Fatalf("ledger rows: want %d, got %d", wantLedger, got)
	}
}

// A registry whose checksum for an already-applied version disagrees with
// the ledger must fail loudly — the schema on disk no longer matches the
// statements this binary would apply, and continuing silently is unsafe.
func TestMigrateRejectsChecksumMismatch(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "mismatch.db")

	s, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	impl := s.(*sqlStore)
	// Clone baseline and mutate v1 only. Passing a partial registry
	// (v1 alone) against a DB that recorded every baseline version would
	// instead trip the unknown-newer-version guard.
	mutated := append([]migration(nil), baselineMigrations("sqlite")...)
	mutated[0] = migration{version: mutated[0].version, statements: []string{"CREATE TABLE IF NOT EXISTS totally_different (id INTEGER)"}}
	err = impl.runMigrations(ctx, mutated)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch error, got %v", err)
	}
}

// A ledger entry for a version this binary does not know about means the
// database was upgraded by a newer Antares. Refuse to touch it.
func TestMigrateRejectsUnknownNewerVersion(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "newer.db")

	s, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.(*sqlStore).db.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, checksum, applied_at) VALUES (999, 'x', 0)`); err != nil {
		t.Fatalf("seed newer version: %v", err)
	}

	err = s.(*sqlStore).migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "unknown migration version 999") {
		t.Fatalf("want unknown-version error, got %v", err)
	}
}

// A pending migration whose second statement fails must roll the whole tx
// back — the version does NOT get a ledger row, and the first (successful)
// DDL of the same version disappears. Baseline rows applied before this run
// stay because they were committed in an earlier tx.
func TestMigrateRollsBackFailedSecondStatement(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "rollback.db")

	s, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	impl := s.(*sqlStore)

	base := baselineMigrations("sqlite")
	badVersion := base[len(base)-1].version + 1
	bad := append(base,
		migration{version: badVersion, statements: []string{
			"CREATE TABLE partial_ok (id INTEGER PRIMARY KEY)",
			"THIS IS NOT VALID SQL",
		}})

	if err := impl.runMigrations(ctx, bad); err == nil {
		t.Fatalf("want migration failure, got nil")
	}

	raw := openRawSQLite(t, dsn)
	// Ledger holds exactly the baseline versions — the bad batch rolled back.
	rows, qerr := raw.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if qerr != nil {
		t.Fatalf("read ledger: %v", qerr)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		versions = append(versions, v)
	}
	if len(versions) != len(base) {
		t.Fatalf("ledger not rolled back cleanly: %v (want %d baseline rows)", versions, len(base))
	}
	for i, v := range versions {
		if v != base[i].version {
			t.Fatalf("ledger version %d: got %d, want %d", i, v, base[i].version)
		}
	}

	// The first (successful) statement of the failed pending version
	// was rolled back too.
	var name string
	err = raw.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='partial_ok'").Scan(&name)
	if err != sql.ErrNoRows {
		t.Fatalf("partial_ok survived rollback: err=%v name=%q", err, name)
	}
}

// Two Open()s racing the same SQLite file must both succeed and must leave
// the ledger with exactly one row per baseline version. Before the writer
// lock was taken inside the migration tx, this could double-apply pending
// versions and hit "table already exists" on the second opener.
func TestMigrateConcurrentSQLiteOpen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "concurrent.db")

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, 2)
	stores := make([]Store, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
			results[i] = err
			stores[i] = s
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		defer stores[i].Close()
	}

	raw := openRawSQLite(t, dsn)
	wantLedger := len(baselineMigrations("sqlite"))
	if got := countLedger(t, raw); got != wantLedger {
		t.Fatalf("ledger rows after concurrent open: want %d, got %d", wantLedger, got)
	}
}

// The registry validator refuses a build-time mistake before touching the DB.
func TestValidateRegistryRejectsBadOrdering(t *testing.T) {
	cases := []struct {
		name string
		reg  []migration
	}{
		{"empty", nil},
		{"nonpositive", []migration{{version: 0, statements: []string{"SELECT 1"}}}},
		{"descending", []migration{
			{version: 2, statements: []string{"SELECT 1"}},
			{version: 1, statements: []string{"SELECT 1"}},
		}},
		{"duplicate", []migration{
			{version: 1, statements: []string{"SELECT 1"}},
			{version: 1, statements: []string{"SELECT 1"}},
		}},
		{"no-statements", []migration{{version: 1}}},
		{"gap-interior", []migration{
			{version: 1, statements: []string{"SELECT 1"}},
			{version: 3, statements: []string{"SELECT 1"}},
		}},
		{"skips-v1", []migration{
			{version: 2, statements: []string{"SELECT 1"}},
			{version: 3, statements: []string{"SELECT 1"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRegistry(tc.reg); err == nil {
				t.Fatalf("want validation error, got nil")
			}
		})
	}
}

// openPostgresForTest returns a DSN scoped to a fresh, uniquely named
// schema on TEST_POSTGRES_DSN. Cleanup drops ONLY that schema. The base
// database and its `public` schema are never touched — pointing this at a
// shared/dev Postgres never risks other data.
//
// If TEST_POSTGRES_DSN is unset the test is skipped.
func openPostgresForTest(t *testing.T) string {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if base == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin postgres: %v", err)
	}
	defer admin.Close()

	// 8 bytes of randomness is plenty to keep parallel test runs from
	// colliding on the same schema name without needing coordination.
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "antares_test_" + hex.EncodeToString(buf[:])

	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %q", schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer cleanup.Close()
		// Best-effort; test may already have failed.
		_, _ = cleanup.ExecContext(context.Background(), fmt.Sprintf("DROP SCHEMA %q CASCADE", schema))
	})

	return appendPostgresParam(t, base, "search_path", schema)
}

// appendPostgresParam adds or replaces the given libpq/pgx parameter on the
// DSN, whether the DSN is a URI (postgres://…?k=v) or a keyword string
// (host=… user=…). The migration code does not care which form it gets.
func appendPostgresParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		q := u.Query()
		q.Set(key, value)
		u.RawQuery = q.Encode()
		return u.String()
	}
	// Keyword form: drop any prior key= then append.
	fields := strings.Fields(dsn)
	out := fields[:0]
	prefix := key + "="
	for _, f := range fields {
		if !strings.HasPrefix(f, prefix) {
			out = append(out, f)
		}
	}
	out = append(out, fmt.Sprintf("%s=%s", key, value))
	return strings.Join(out, " ")
}

// TestMigrateAgainstPostgres exercises the real Postgres path end-to-end
// against a disposable schema (see openPostgresForTest). It Opens the store,
// runs the full baseline registry, verifies the ledger, opens again to
// confirm idempotency under the advisory lock, and probes that the
// historical rag_chunks BLOB column landed as BYTEA — the one intentional
// dialect rewrite in baselineV1. Skips when TEST_POSTGRES_DSN is unset;
// source-shape tests can't prove Postgres actually accepts the DDL.
func TestMigrateAgainstPostgres(t *testing.T) {
	dsn := openPostgresForTest(t)
	ctx := context.Background()

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw postgres open: %v", err)
	}
	t.Cleanup(func() { raw.Close() })

	s1, err := Open(ctx, "postgres", dsn, 4, 5000, false)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	s1.Close()

	// Second Open must be a full no-op — checksums match and the ledger is
	// already populated. Exercises the advisory-lock path against a
	// database that already holds every migration.
	s2, err := Open(ctx, "postgres", dsn, 4, 5000, false)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	defer s2.Close()

	wantLedger := len(baselineMigrations("postgres"))
	var got int
	if err := raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&got); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if got != wantLedger {
		t.Fatalf("ledger rows: want %d, got %d", wantLedger, got)
	}

	// rag_chunks.embedding must have landed as BYTEA (not BLOB, which
	// Postgres does not know). Prove it against the live catalog rather
	// than string-matching the source DDL.
	var udt string
	err = raw.QueryRowContext(ctx, `SELECT udt_name FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'rag_chunks'
		  AND column_name = 'embedding'`).Scan(&udt)
	if err != nil {
		t.Fatalf("probe rag_chunks.embedding: %v", err)
	}
	if udt != "bytea" {
		t.Fatalf("rag_chunks.embedding: want bytea, got %q", udt)
	}
}

// A ledger with an interior gap (say v1 + v3 with v2 missing) means someone
// hand-edited the ledger or restored from an incompatible backup. runMigrations
// must refuse to touch such a database rather than silently re-run v2 (which
// would collide with v3-created objects) or ignore the gap.
func TestMigrateRejectsGapInAppliedHistory(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "gap.db")

	s, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	impl := s.(*sqlStore)

	// Simulate a hand-edited ledger: drop v1, leave v2. Fresh Open just
	// applied both, so we know both rows are there to remove.
	if _, err := impl.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 1"); err != nil {
		t.Fatalf("delete v1: %v", err)
	}

	err = impl.migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "missing version 1") {
		t.Fatalf("want missing-version error, got %v", err)
	}
}
