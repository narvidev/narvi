// Package postgres stands for github.com/golang-migrate/migrate/v4/
// database/postgres -- round 3 review of PR #324, finding R7: this
// driver wraps database/sql and lib/pq and can run arbitrary SQL
// (Postgres.Run) through neither name the analyzer's own prior deny-list
// ever matched, even though the module is already a direct go.mod
// dependency (controlplane/migrate.go uses it legitimately).
package postgres

type Postgres struct{}

func (p *Postgres) Open(url string) (*Postgres, error) { return p, nil }

func (p *Postgres) Run(migration []byte) error { return nil }
