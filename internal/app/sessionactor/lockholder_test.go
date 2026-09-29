package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestStatementLost proves which failed lock-connection statements presume
// the connection lost (statementLost, lockholder.go), and so close it, stop
// every actor locked on it and release all their locks: only a connection
// that is gone -- closed, a statement past its bound, an error that is not
// the server's, or a FATAL or PANIC from the server. A server-side ERROR,
// such as the shared lock table being full, leaves the backend and its
// locks as they were, and must not. Treating every error as a loss fails
// the ERROR rows.
func TestStatementLost(t *testing.T) {
	t.Parallel()

	serverErr := func(severity, unlocalized, code string) error {
		return fmt.Errorf("sessionactor: try advisory lock: %w",
			&pgconn.PgError{Severity: severity, SeverityUnlocalized: unlocalized, Code: code, Message: "injected"})
	}
	for _, tc := range []struct {
		name         string
		connClosed   bool
		boundExpired bool
		err          error
		want         bool
	}{
		{"a server ERROR on a live connection: out of shared memory", false, false, serverErr("ERROR", "ERROR", "53200"), false},
		{"a server ERROR from a server that sends no unlocalized severity", false, false, serverErr("ERROR", "", "53200"), false},
		{"a localized ERROR is read from the unlocalized severity", false, false, serverErr("FEHLER", "ERROR", "57014"), false},
		{"a server FATAL: the backend was terminated", false, false, serverErr("FATAL", "FATAL", "57P01"), true},
		{"a localized FATAL is read from the unlocalized severity", false, false, serverErr("SCHWERWIEGEND", "FATAL", "57P01"), true},
		{"a server FATAL from a server that sends no unlocalized severity", false, false, serverErr("FATAL", "", "57P01"), true},
		{"a server PANIC", false, false, serverErr("PANIC", "PANIC", "XX000"), true},
		{"a network error is not the server's", false, false, &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}, true},
		{"an unexpected EOF is not the server's", false, false, fmt.Errorf("sessionactor: probe lock connection: %w", io.ErrUnexpectedEOF), true},
		{"the connection is closed, whatever the error", true, false, serverErr("ERROR", "ERROR", "53200"), true},
		{"the statement overran its bound, whatever the error", false, true, serverErr("ERROR", "ERROR", "57014"), true},
		{"the statement overran its bound: the context's own error", false, true, context.DeadlineExceeded, true},
		{"an error with no cause at all is not the server's", false, false, errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statementLost(tc.connClosed, tc.boundExpired, tc.err); got != tc.want {
				t.Fatalf("statementLost(closed=%v, boundExpired=%v, %v) = %v, want %v", tc.connClosed, tc.boundExpired, tc.err, got, tc.want)
			}
		})
	}
}
