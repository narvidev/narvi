package platform

// MinPostgresServerVersionNum is the oldest Postgres server the control
// plane runs against, in server_version_num's form: major × 10000 + minor
// from Postgres 10 on, so 160000 is 16.0. Boot reads the server's own
// before any migration runs and refuses an older server, naming both
// versions (controlplane's requireSupportedPostgres, technical plan §5.1).
// docs/PRODUCTION_CHECKLIST.md item 13 and technical plan §1 state the same
// floor, and internal/ops checks that every document stating it agrees with
// this constant.
//
// 16 was taken as a default, not measured: the connection budget boot logs
// reads reserved_connections, which exists from 16 on
// (controlplane/dbbudget.go); the checklist already asked for 16 or later;
// and 14 reaches the end of its support in November 2026. Every
// Postgres-backed test runs on 17, and the postgres adapter's suite, the
// lock connection's tests and boot's own also run on 16 (`make
// test-integration-postgres-floor`). Lowering the floor means running those
// suites on the lower version first, and restoring what was removed when it
// rose to 16: the budget's read of reserved_connections as 0 where it does
// not exist, and, below 14, a terminate of a lost lock connection's backend
// that only signals it, since pg_terminate_backend waits from 14 on.
const MinPostgresServerVersionNum = 160000
