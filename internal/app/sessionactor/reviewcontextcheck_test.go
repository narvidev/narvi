package sessionactor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/autoapproval"
)

// attemptReadersExempt are the sqlc queries that read is_review_attempt
// without excluding a turn ended for a reason of its own, each with why it
// need not.
var attemptReadersExempt = map[string]string{
	// It reads the session's next turn to dispatch, a pending turn, which
	// has not ended; and a turn ended context_moved that was ahead of it
	// in the queue is a turn it waited behind, as any other.
	"GetReviewAttemptToCheck": "reads a pending turn, which has not ended",
}

// attemptReadersKnown are the readers the scan must find: if it no longer
// finds them, it is broken.
var attemptReadersKnown = []string{"ExistsNewerReviewAttempt", "GetNewestReviewAttempt", "GetSessionActivityFacts"}

var (
	// insertColumnList is an INSERT's column list, which names columns it
	// writes, never reads.
	insertColumnList = regexp.MustCompile(`(?is)\binsert\s+into\s+[a-z_."]+\s*\([^)]*\)`)
	// readsReviewAttempt is a reference to turns.is_review_attempt.
	readsReviewAttempt = regexp.MustCompile(`(?i)\bis_review_attempt\b`)
	// fromTurns starts the FROM of a statement or subquery reading turns,
	// aliased or not.
	fromTurns = regexp.MustCompile(`(?i)\bfrom\s+(?:only\s+)?(?:(?:"public"|public)\s*\.\s*)?(?:"turns"|turns\b)`)
	// newestFirst orders by creation time, newest first: the read picks a
	// turn as the newest, or the last.
	newestFirst = regexp.MustCompile(`(?is)\border\s+by\s+(?:[a-z_]+\.)?created_at\s+desc\b`)
	// subqueryEnd closes the FROM ... ORDER BY of one read of turns.
	subqueryEnd = regexp.MustCompile(`(?i)\blimit\b|;`)
	// namesEndReason excludes a turn ended for a reason of its own, the
	// only way a read of attempts may name it today.
	namesEndReason = regexp.MustCompile(`(?i)\bend_reason\s+is\s+null\b`)
)

// attemptReaderProblems returns why sql, one sqlc query with its comments
// stripped, reads attempts without excluding a turn ended context_moved:
// it reads is_review_attempt and names no end_reason IS NULL, or one of its
// reads of turns orders them newest first -- picking the newest or the last
// turn -- with no end_reason IS NULL between its FROM and its LIMIT.
func attemptReaderProblems(sql string) []string {
	var problems []string
	read := insertColumnList.ReplaceAllString(sql, "")
	if readsReviewAttempt.MatchString(read) && !namesEndReason.MatchString(read) {
		problems = append(problems, "reads is_review_attempt and keeps a turn ended context_moved")
	}
	for _, loc := range fromTurns.FindAllStringIndex(read, -1) {
		rest := read[loc[0]:]
		if end := subqueryEnd.FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		if newestFirst.MatchString(rest) && !namesEndReason.MatchString(rest) {
			problems = append(problems, "picks the newest turn and may pick one ended context_moved: "+strings.Join(strings.Fields(rest), " "))
		}
	}
	return problems
}

// TestReviewAttemptReadersExcludeContextMoved pins technical plan §24.9's
// "every reader of attempts excludes it": a turn ended context_moved never
// ran, so no sqlc query that reads review attempts -- one naming
// is_review_attempt -- or picks a turn as the newest or the last of a
// session -- one ordering turns newest first -- may read it. Each must keep
// end_reason IS NULL: the newest attempt (a pull request's review state),
// whether a newer attempt ran since a verdict was accepted, a session's
// last run and the newest turn its failure reason is read against.
// attemptReadersExempt lists the queries that need not, each with why.
// Readers in Go are the derivation's (storedTurnSummary marks the turn
// Ignored) or read only a processing turn, which a context_moved turn
// never was. SQL assembled at run time is not seen.
func TestReviewAttemptReadersExcludeContextMoved(t *testing.T) {
	t.Parallel()

	root := sandboxStatusModuleRoot(t)
	queries, err := filepath.Glob(filepath.Join(root, "internal", "adapters", "outbound", "postgres", "queries", "*.sql"))
	if err != nil || len(queries) == 0 {
		t.Fatalf("list the sqlc queries: %v (%d files)", err, len(queries))
	}
	readers := map[string]bool{}
	exemptSeen := map[string]bool{}
	for _, path := range queries {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, block := range strings.Split(string(body), "-- name:") {
			if i == 0 {
				continue
			}
			name := strings.Fields(block)[0]
			sql := sqlBlockComment.ReplaceAllString(sqlLineComment.ReplaceAllString(block, ""), "")
			read := insertColumnList.ReplaceAllString(sql, "")
			if readsReviewAttempt.MatchString(read) || (fromTurns.MatchString(read) && newestFirst.MatchString(read)) {
				readers[name] = true
			}
			problems := attemptReaderProblems(sql)
			if why, ok := attemptReadersExempt[name]; ok {
				exemptSeen[name] = true
				if len(problems) == 0 {
					t.Errorf("query %q is exempt (%s) but excludes a turn ended context_moved anyway: drop it from attemptReadersExempt", name, why)
				}
				continue
			}
			for _, problem := range problems {
				t.Errorf("%s: query %q %s (technical plan §24.9: a turn ended context_moved never ran, so it is no attempt and no last run) -- add end_reason IS NULL", path, name, problem)
			}
		}
	}
	for _, name := range attemptReadersKnown {
		if !readers[name] {
			t.Errorf("query %q is no longer read as a reader of attempts: the scan is broken, or attemptReadersKnown is stale", name)
		}
	}
	var stale []string
	for name := range attemptReadersExempt {
		if !exemptSeen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("query %q is exempt but no longer exists: drop it from attemptReadersExempt", name)
	}
}

// TestAttemptReaderProblems pins the shapes the guard reads as a reader of
// attempts that keeps a turn ended context_moved, and the ones it must not.
func TestAttemptReaderProblems(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sql  string
		want int
	}{
		{"an attempt read with the exclusion", `SELECT id FROM turns WHERE session_id = $1 AND is_review_attempt = true AND end_reason IS NULL ORDER BY created_at DESC LIMIT 1`, 0},
		{"an attempt read without it", `SELECT id FROM turns WHERE session_id = $1 AND is_review_attempt = true ORDER BY created_at DESC LIMIT 1`, 2},
		{"an attempt count without it", `SELECT EXISTS(SELECT 1 FROM turns WHERE session_id = $1 AND is_review_attempt = true AND created_at > $2)`, 1},
		{"the last terminal turn without it", `SELECT t.id FROM turns t WHERE t.session_id = $1 AND t.status IN ('completed', 'failed', 'cancelled') ORDER BY t.created_at DESC, t.id DESC LIMIT 1`, 1},
		{"the last terminal turn with it", `SELECT t.id FROM turns t WHERE t.session_id = $1 AND t.end_reason IS NULL ORDER BY t.created_at DESC LIMIT 1`, 0},
		{"one lateral excludes, another does not", `SELECT a.id, b.id FROM sessions s
			LEFT JOIN LATERAL (SELECT t.id FROM turns t WHERE t.session_id = s.id AND t.end_reason IS NULL ORDER BY t.created_at DESC LIMIT 1) a ON true
			LEFT JOIN LATERAL (SELECT t.id FROM turns t WHERE t.session_id = s.id ORDER BY t.created_at DESC LIMIT 1) b ON true`, 1},
		{"oldest first", `SELECT * FROM turns WHERE session_id = $1 ORDER BY created_at ASC, id ASC`, 0},
		{"an insert naming the column", `INSERT INTO turns (session_id, status, is_review_attempt) VALUES ($1, $2, $3) RETURNING *`, 0},
		{"another table newest first", `SELECT id FROM plans WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`, 0},
		{"a merely mentioned end_reason does not exclude", `SELECT id, end_reason FROM turns WHERE session_id = $1 AND is_review_attempt ORDER BY created_at DESC LIMIT 1`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := attemptReaderProblems(tc.sql); len(got) != tc.want {
				t.Errorf("attemptReaderProblems(%q) = %q, want %d problems", tc.sql, got, tc.want)
			}
		})
	}
}

// TestReviewContextOutcomeFor pins how the context check reads the one
// comparison's answer (technical plan §24.9): a fact that differs is a
// move, a fact that cannot be established is unconfirmed, and only a
// passing comparison is fresh -- an unknown is never read as fresh.
func TestReviewContextOutcomeFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		reason autoapproval.Reason
		want   reviewContextOutcome
	}{
		{autoapproval.ReasonNone, reviewContextFresh},
		{autoapproval.ReasonStaleVerdict, reviewContextMoved},
		{autoapproval.ReasonBaseMoved, reviewContextMoved},
		{autoapproval.ReasonAncestorChainChanged, reviewContextMoved},
		{autoapproval.ReasonContextUnknown, reviewContextUnconfirmed},
		{autoapproval.ReasonBaseSHAUnknown, reviewContextUnconfirmed},
		{autoapproval.ReasonAncestorChainUnknown, reviewContextUnconfirmed},
		{autoapproval.ReasonNotAssessed, reviewContextUnconfirmed},
		{autoapproval.Reason("a_reason_this_binary_does_not_know"), reviewContextUnconfirmed},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			if got := reviewContextOutcomeFor(tc.reason); got != tc.want {
				t.Errorf("reviewContextOutcomeFor(%q) = %s, want %s", tc.reason, got, tc.want)
			}
		})
	}
}
