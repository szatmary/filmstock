//go:build cgo && !filmstock_purego

package sqldrv

import (
	"context"
	"database/sql/driver"

	"github.com/szatmary/filmstock/internal/sqlite3"
)

// Connector opens path read-only, running init on EVERY connection it makes.
//
// Per-connection is the whole point: ATTACH is connection state, so attaching
// on one pooled connection leaves every other one unable to see the schema.
// Running it from the driver's connect hook means a caller can hold an
// ordinary *sql.DB, let the pool grow and shrink, and still have every query
// see the attached databases.
func Connector(path string, readOnly bool, init []string) (driver.Connector, error) {
	drv := &sqlite3.SQLiteDriver{}
	if len(init) > 0 {
		drv.ConnectHook = func(c *sqlite3.SQLiteConn) error {
			for _, stmt := range init {
				if _, err := c.Exec(stmt, nil); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return &dsnConnector{dsn: DSN(path, readOnly), drv: drv}, nil
}

type dsnConnector struct {
	dsn string
	drv driver.Driver
}

func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }
func (c *dsnConnector) Driver() driver.Driver                        { return c.drv }
