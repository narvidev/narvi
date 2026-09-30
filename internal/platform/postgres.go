package platform

// MinPostgresServerVersionNum is the oldest Postgres server the control
// plane runs against, in server_version_num's form: major × 10000 + minor
// from Postgres 10 on, so 160000 is 16.0. Boot reads the server's own
// before any migration runs, and refuses an older server, naming both
// versions (controlplane's requireSupportedPostgres, technical plan §5.1):
// serve, seed and routes all do. docs/PRODUCTION_CHECKLIST.md item 13,
// technical plan §1 and §5.1 and the deploy secret state the same floor, in
// one phrase ("Postgres N or later"), and internal/ops checks that each
// names this constant's major there and states no floor in another wording
// it recognises.
//
// 16 was taken as a default, not measured: the connection budget boot logs
// reads reserved_connections, which exists from 16 on
// (controlplane/dbbudget.go), and 14 reaches the end of its support in
// November 2026. No document named a floor before this constant: the
// checklist's item 10 read only "and, on Postgres 16 or later, `SHOW
// reserved_connections;`", a condition that allowed an older server. Every
// Postgres-backed test runs on 17, and the postgres adapter's suite, the
// lock connection's tests and boot's own also run on the floor (`make
// test-integration-postgres-floor`). Lowering the floor means running those
// suites on the lower version first, and restoring what was removed when it
// rose to 16: the budget's read of reserved_connections as 0 where it does
// not exist, and, below 14, a terminate of a lost lock connection's backend
// that only signals it, since pg_terminate_backend waits from 14 on.
const MinPostgresServerVersionNum = 160000
