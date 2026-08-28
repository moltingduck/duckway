package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

const postgresDriverName = "duckway-postgres"

var registerPostgresDriver sync.Once

func ensurePostgresDriver() {
	registerPostgresDriver.Do(func() {
		sql.Register(postgresDriverName, postgresCompatDriver{base: stdlib.GetDefaultDriver()})
	})
}

type postgresCompatDriver struct{ base driver.Driver }

func (d postgresCompatDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &postgresCompatConn{Conn: c}, nil
}

type postgresCompatConn struct{ driver.Conn }

func (c *postgresCompatConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(postgresQuery(query))
}

func (c *postgresCompatConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, postgresQuery(query))
	}
	return c.Prepare(query)
}

func (c *postgresCompatConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	exec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return exec.ExecContext(ctx, postgresQuery(query), args)
}

func (c *postgresCompatConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return q.QueryContext(ctx, postgresQuery(query), args)
}

func (c *postgresCompatConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return nil, fmt.Errorf("PostgreSQL driver does not implement transaction contexts")
}

func (c *postgresCompatConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *postgresCompatConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *postgresCompatConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *postgresCompatConn) CheckNamedValue(v *driver.NamedValue) error {
	if value, ok := v.Value.(bool); ok {
		if value {
			v.Value = int64(1)
		} else {
			v.Value = int64(0)
		}
	}
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(v)
	}
	return driver.ErrSkip
}

// postgresQuery converts only syntax whose meaning is identical in both
// databases. Dialect-specific upserts and concurrency-sensitive statements
// remain explicit at their call sites.
func postgresQuery(query string) string {
	ignoreConflict := strings.Contains(strings.ToUpper(query), "INSERT OR IGNORE INTO")
	query = strings.Replace(query, "INSERT OR IGNORE INTO", "INSERT INTO", 1)
	query = strings.ReplaceAll(query, "INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY")
	var b strings.Builder
	b.Grow(len(query) + 16)
	arg := 1
	inSingle := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			b.WriteByte(ch)
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte(query[i+1])
				i++
				continue
			}
			inSingle = !inSingle
			continue
		}
		if ch == '?' && !inSingle {
			fmt.Fprintf(&b, "$%d", arg)
			arg++
			continue
		}
		b.WriteByte(ch)
	}
	result := b.String()
	// These scalar MAX calls are the SQLite spelling of PostgreSQL GREATEST.
	for _, pair := range [][2]string{
		{"MAX(input_tokens, 0)", "GREATEST(input_tokens, 0)"},
		{"MAX(output_tokens, 0)", "GREATEST(output_tokens, 0)"},
		{"MAX(cache_read_tokens, 0)", "GREATEST(cache_read_tokens, 0)"},
		{"MAX(cache_creation_tokens, 0)", "GREATEST(cache_creation_tokens, 0)"},
		{"MAX(reasoning_tokens, 0)", "GREATEST(reasoning_tokens, 0)"},
		{"MAX(1,$2)", "GREATEST(1,$2)"},
		{"MAX(1, $2)", "GREATEST(1, $2)"},
	} {
		result = strings.ReplaceAll(result, pair[0], pair[1])
	}
	if ignoreConflict {
		result = strings.TrimSpace(result) + " ON CONFLICT DO NOTHING"
	}
	return result
}
