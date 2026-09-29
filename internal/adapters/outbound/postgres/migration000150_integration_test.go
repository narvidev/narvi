//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
)

// This file runs migration 000150 (session_create_idempotency_key) through
// golang-migrate against real Postgres, in a database of its own migrated
// to 149 first. The up adds the key a create is replayed by and the hash of
// the request it came with (technical plan §43.8): unique per creator,
// never one without the other. The down removes them.

func TestMigrationCreateIdempotencyKey_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 149)

	columns := func() string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name IN ('create_idempotency_key', 'create_request_sha256') ORDER BY column_name`)
		if err != nil {
			t.Fatalf("read columns: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	newUser := func(email string) string {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx, `INSERT INTO users (primary_email, display_name) VALUES ($1, 'Migration 150') RETURNING id`, email).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	// insert creates a session for creator under key, with hash as its
	// request hash (decode($3, 'hex'), NULL for an empty string).
	insert := func(creator, key, hash string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO sessions (spawn_source, created_by, create_idempotency_key, create_request_sha256)
			VALUES ('web', $1, NULLIF($2, '')::uuid, CASE WHEN $3 = '' THEN NULL ELSE decode($3, 'hex') END)`, creator, key, hash)
		return err
	}
	const (
		key   = "11111111-2222-4333-8444-555555555555"
		other = "66666666-7777-4888-9999-aaaaaaaaaaaa"
		hash  = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	)

	if got := columns(); got != "" {
		t.Fatalf("columns at 149 = %q, want none", got)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(150); err != nil {
		t.Fatalf("up to 150: %v", err)
	}
	if got := columns(); got != "create_idempotency_key,create_request_sha256" {
		t.Fatalf("columns at 150 = %q, want both", got)
	}

	alice, bob := newUser("alice-150@example.com"), newUser("bob-150@example.com")
	if err := insert(alice, key, hash); err != nil {
		t.Fatalf("a first session under a key: %v", err)
	}
	// Per creator: the same key again for alice is refused on the index, and
	// is not for bob; sessions with no key never collide.
	if err := insert(alice, key, hash); err == nil || !strings.Contains(err.Error(), "sessions_create_idempotency_key_uniq") {
		t.Fatalf("alice's same key again = %v, want a unique violation on sessions_create_idempotency_key_uniq", err)
	}
	if err := insert(bob, key, hash); err != nil {
		t.Fatalf("bob's same key: %v, want its own", err)
	}
	for range 2 {
		if err := insert(alice, "", ""); err != nil {
			t.Fatalf("a keyless session: %v", err)
		}
	}
	// The pair travels together, and the hash is a SHA-256.
	for name, tc := range map[string]struct{ key, hash string }{
		"a key without its hash":     {other, ""},
		"a hash without its key":     {"", hash},
		"a hash of the wrong length": {other, "0011"},
	} {
		if err := insert(alice, tc.key, tc.hash); err == nil || !strings.Contains(err.Error(), "sessions_create_idempotency_pair_check") {
			t.Errorf("%s = %v, want the pair check's refusal", name, err)
		}
	}

	// Down: the columns, the index and the check go; the sessions stay.
	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(149); err != nil {
		t.Fatalf("down to 149: %v", err)
	}
	if got := columns(); got != "" {
		t.Fatalf("columns after down = %q, want none", got)
	}
	var after int
	var indexLeft, checkLeft bool
	if err := db.QueryRowContext(ctx, `SELECT count(*), to_regclass('sessions_create_idempotency_key_uniq') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sessions_create_idempotency_pair_check') FROM sessions`).Scan(&after, &indexLeft, &checkLeft); err != nil {
		t.Fatal(err)
	}
	if after != before || indexLeft || checkLeft {
		t.Fatalf("after down: %d sessions (was %d), index left %v, check left %v", after, before, indexLeft, checkLeft)
	}

	// Up again works on that state.
	if err := m.Migrate(150); err != nil {
		t.Fatalf("up to 150 again: %v", err)
	}
	assertCleanVersion(t, connStr, 150)
}

// TestMigration000150_ConcurrentMigrators is two control planes booting at
// once onto 000150: one applies it while the other waits on
// golang-migrate's advisory lock, holding a snapshot. The file builds its
// unique index plainly, in its own transaction, never CONCURRENTLY -- a
// concurrent build would wait for that snapshot while its holder waits for
// the build, a deadlock (000144's own reason) -- so both boots succeed, the
// version is clean, and the index is there and valid.
func TestMigration000150_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 149)
	concurrentMigratorsUp(ctx, t, connStr, db, 150)

	var valid bool
	if err := db.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'sessions_create_idempotency_key_uniq'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatalf("sessions_create_idempotency_key_uniq valid = %v (err %v), want a valid index", valid, err)
	}
}
