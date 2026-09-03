//go:build !cgo || filmstock_purego

package sqldrv

import (
	"context"
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"
)

// Connector opens path read-only, running init on EVERY connection it makes.
// See the cgo build's copy for why per-connection matters.
func Connector(path string, readOnly bool, init []string) (driver.Connector, error) {
	base, err := sqlite.NewConnector(DSN(path, readOnly))
	if err != nil {
		return nil, err
	}
	if len(init) == 0 {
		return base, nil
	}
	return &initConnector{base: base, init: init}, nil
}

type initConnector struct {
	base driver.Connector
	init []string
}

func (c *initConnector) Driver() driver.Driver { return c.base.Driver() }

func (c *initConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ex, ok := cn.(driver.ExecerContext)
	if !ok {
		cn.Close()
		return nil, fmt.Errorf("filmstock: driver connection cannot execute the attach statements")
	}
	for _, stmt := range c.init {
		if _, err := ex.ExecContext(ctx, stmt, nil); err != nil {
			cn.Close()
			return nil, err
		}
	}
	return cn, nil
}
