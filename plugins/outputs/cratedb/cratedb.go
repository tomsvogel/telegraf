package cratedb

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/outputs"
	_ "github.com/lib/pq"
)

type CrateDB struct {
	URL         string
	Timeout     internal.Duration
	Table       string
	TableCreate bool
	DB          *sql.DB
}

var sampleConfig = `
  # A lib/pq connection string.
  # See http://godoc.org/github.com/lib/pq#hdr-Connection_String_Parameters
  url = "postgres://user:password@localhost/?sslmode=disable.
  # The timouet for writing metrics.
  timeout = "5s"
`

func (c *CrateDB) Connect() error {
	db, err := sql.Open("postgres", c.URL)
	if err != nil {
		return err
	} else if c.TableCreate {
		sql := `
CREATE TABLE IF NOT EXISTS ` + c.Table + ` (
	"hash_id" LONG,
	"timestamp" TIMESTAMP NOT NULL,
	"name" STRING,
	"tags" OBJECT(DYNAMIC),
	"fields" OBJECT(DYNAMIC),
	 PRIMARY KEY ("timestamp", "hash_id")
);
`
		ctx, _ := context.WithTimeout(context.Background(), c.Timeout.Duration)
		if _, err := db.ExecContext(ctx, sql); err != nil {
			return err
		}
	}
	c.DB = db
	return nil
}

func (c *CrateDB) Write(metrics []telegraf.Metric) error {
	// TODO(fg) test timeouts
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout.Duration)
	defer cancel()

	if sql, err := insertSQL(c.Table, metrics); err != nil {
		return err
	} else if _, err := c.DB.ExecContext(ctx, sql); err != nil {
		fmt.Printf("%s\n", sql)
		return err
	}
	return nil
}

func insertSQL(table string, metrics []telegraf.Metric) (string, error) {
	rows := make([]string, len(metrics))
	for i, m := range metrics {
		m.HashID()
		cols := []interface{}{
			m.HashID(),
			m.Time(),
			m.Name(),
			m.Tags(),
			m.Fields(),
		}

		escapedCols := make([]string, len(cols))
		for i, col := range cols {
			escaped, err := escapeValue(col)
			if err != nil {
				return "", err
			}
			escapedCols[i] = escaped
		}
		rows[i] = `(` + strings.Join(escapedCols, ", ") + `)`
	}
	sql := `INSERT INTO ` + table + ` ("hash_id", "timestamp", "name", "tags", "fields")
VALUES
` + strings.Join(rows, " ,\n") + `;`
	return sql, nil
}

// escapeValue returns a string version of val that is suitable for being used
// inside of a VALUES expression or similar. Unsupported types return an error.
//
// Warning: This is not ideal from a security perspective, but unfortunately
// CrateDB does not support enough of the PostgreSQL wire protocol to allow
// using lib/pq with $1, $2 placeholders. Security conscious users of this
// plugin should probably refrain from using it in combination with untrusted
// inputs.
func escapeValue(val interface{}) (string, error) {
	switch t := val.(type) {
	case string:
		return escapeString(t, `'`), nil
	case int, uint, int32, uint32, int64, uint64, float32, float64:
		return fmt.Sprint(t), nil
	case time.Time:
		// see https://crate.io/docs/crate/reference/sql/data_types.html#timestamp
		return escapeValue(t.Format("2006-01-02T15:04:05.999-0700"))
	case map[string]string:
		return escapeObject(convertMap(t))
	case map[string]interface{}:
		return escapeObject(t)
	default:
		// This might be panic worthy under normal circumstances, but it's probably
		// better to not shut down the entire telegraf process because of one
		// misbehaving plugin.
		return "", fmt.Errorf("unexpected type: %T: %#v", t, t)
	}
}

// convertMap converts m from map[string]string to map[string]interface{} by
// copying it. Generics, oh generics where art thou?
func convertMap(m map[string]string) map[string]interface{} {
	c := make(map[string]interface{}, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func escapeObject(m map[string]interface{}) (string, error) {
	// There is a decent chance that the implementation below doesn't catch all
	// edge cases, but it's hard to tell since the format seems to be a bit
	// underspecified.
	// See https://crate.io/docs/crate/reference/sql/data_types.html#object

	// We find all keys and sort them first because iterating a map in go is
	// randomized and we need consistent output for our unit tests.
	keys := make([]string, 0, len(m))
	for k, _ := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Now we build our key = val pairs
	pairs := make([]string, 0, len(m))
	for _, k := range keys {
		// escape the value of our key k (potentially recursive)
		val, err := escapeValue(m[k])
		if err != nil {
			return "", err
		}
		pairs = append(pairs, escapeString(k, `"`)+" = "+val)
	}
	return `{` + strings.Join(pairs, ", ") + `}`, nil
}

// escapeString wraps s in the given quote string and replaces all occurences
// of it inside of s with a double quote.
func escapeString(s string, quote string) string {
	return quote + strings.Replace(s, quote, quote+quote, -1) + quote
}

func (c *CrateDB) SampleConfig() string {
	return sampleConfig
}

func (c *CrateDB) Description() string {
	return "Configuration for CrateDB to send metrics to."
}

func (c *CrateDB) Close() error {
	return c.DB.Close()
}

func init() {
	outputs.Add("cratedb", func() telegraf.Output {
		return &CrateDB{
			Timeout: internal.Duration{Duration: time.Second * 5},
		}
	})
}