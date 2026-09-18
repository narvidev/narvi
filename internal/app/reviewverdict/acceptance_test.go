package reviewverdict

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestAcceptance_NilStore pins finding F9 (adversarial review): a nil
// *postgres.ReviewVerdictAcceptanceStore is documented (Deps.Acceptances'
// own doc comment) as degrading Accept/RevokeAcceptance/GetAcceptance/
// ListAcceptances to a plain error -- before this fix, only
// GetActiveAcceptance actually checked for nil; every other function here
// called straight through to a nil store and panicked on the nil-pointer
// field access inside it. A caller wiring a deployment/test that never
// constructs this store (Deps.Acceptances' own established "a caller that
// doesn't need this rollup simply never wires its own store" convention)
// must get an honest error here, never a crash.
func TestAcceptance_NilStore(t *testing.T) {
	ctx := context.Background()

	t.Run("Accept", func(t *testing.T) {
		_, err := Accept(ctx, nil, AcceptInput{RepoFullName: "acme/widgets", PRNumber: 1})
		if !errors.Is(err, ErrAcceptanceStoreNotConfigured) {
			t.Fatalf("Accept(nil store) error = %v, want ErrAcceptanceStoreNotConfigured (never a panic)", err)
		}
	})

	t.Run("RevokeAcceptance", func(t *testing.T) {
		_, ok, err := RevokeAcceptance(ctx, nil, pgtype.UUID{}, pgtype.UUID{}, "acme/widgets")
		if ok {
			t.Error("RevokeAcceptance(nil store) ok = true, want false")
		}
		if !errors.Is(err, ErrAcceptanceStoreNotConfigured) {
			t.Fatalf("RevokeAcceptance(nil store) error = %v, want ErrAcceptanceStoreNotConfigured (never a panic)", err)
		}
	})

	t.Run("GetAcceptance", func(t *testing.T) {
		_, ok, err := GetAcceptance(ctx, nil, pgtype.UUID{}, "acme/widgets")
		if ok {
			t.Error("GetAcceptance(nil store) ok = true, want false")
		}
		if !errors.Is(err, ErrAcceptanceStoreNotConfigured) {
			t.Fatalf("GetAcceptance(nil store) error = %v, want ErrAcceptanceStoreNotConfigured (never a panic)", err)
		}
	})

	t.Run("ListAcceptances", func(t *testing.T) {
		_, err := ListAcceptances(ctx, nil, "acme/widgets", 1, 10)
		if !errors.Is(err, ErrAcceptanceStoreNotConfigured) {
			t.Fatalf("ListAcceptances(nil store) error = %v, want ErrAcceptanceStoreNotConfigured (never a panic)", err)
		}
	})

	// GetActiveAcceptance already handled this correctly before F9 --
	// pinned here anyway so this file is the ONE place asserting the
	// full, consistent nil-store contract across every function in
	// acceptance.go.
	t.Run("GetActiveAcceptance", func(t *testing.T) {
		_, ok, err := GetActiveAcceptance(ctx, nil, "acme/widgets", 1)
		if ok {
			t.Error("GetActiveAcceptance(nil store) ok = true, want false")
		}
		if err != nil {
			t.Fatalf("GetActiveAcceptance(nil store) error = %v, want nil (this one degrades to ok=false, not an error -- see its own doc comment)", err)
		}
	})
}
