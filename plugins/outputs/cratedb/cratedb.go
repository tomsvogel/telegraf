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
	TableCreate bool `toml:"table_create"`
	DB          *sql.DB
}

var sampleConfig = `
  # A lib/pq connection string.
  # See http://godoc.org/github.com/lib/pq#hdr-Connection_String_Parameters
  url = "postgres://user:password@localhost/schema?sslmode=disable"
  # Timeout for all CrateDB queries.
  timeout = "5s"
  # Name of the table to store metrics in.
  table = "metrics"
  # If true, and the metrics table does not exist, create it automatically.
  table_create = true
`

func (c *CrateDB) Connect() error {
	db, err := sql.Open("postgres", c.URL)
	if err != nil {
		return err
	} else if c.TableCreate {
		sql := `
CREATE TABLE IF NOT EXISTS ` + c.Table + ` (
	"hash_id" LONG INDEX OFF,
	"timestamp" TIMESTAMP,
	"name" STRING,
	"tags" OBJECT(DYNAMIC),
	"fields" OBJECT(DYNAMIC),
  "day" TIMESTAMP GENERATED ALWAYS AS date_trunc('day', "timestamp"),
	PRIMARY KEY ("timestamp", "hash_id","day")
)PARTITIONED BY("day");
`
		ctx, cancel := context.WithTimeout(context.Background(), c.Timeout.Duration)
		defer cancel()
		if _, err := db.ExecContext(ctx, sql); err != nil {
			return err
		}
	}
	c.DB = db
	return nil
}

func (c *CrateDB) Write(metrics []telegraf.Metric) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout.Duration)
	defer cancel()
	if sql, err := insertSQL(c.Table, metrics, time.Local); err != nil {
		return err
	} else if _, err := c.DB.ExecContext(ctx, sql); err != nil {
		return err
	}
	return nil
}

func insertSQL(table string, metrics []telegraf.Metric, loc *time.Location) (string, error) {
	rows := make([]string, len(metrics))
	for i, m := range metrics {
		// Note: We have to convert HashID from uint64 to int64 below because
		// CrateDB only supports a signed 64 bit LONG type which would give us
		// problems, e.g.:
		//
		// CREATE TABLE my_long (val LONG);
		// INSERT INTO my_long(val) VALUES (14305102049502225714);
		// -> ERROR:  SQLParseException: For input string: "14305102049502225714"

		cols := []interface{}{
			int64(m.HashID()),
			m.Time().In(loc),
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
	// We don't handle uint, uint32 and uint64 here because CrateDB doesn't
	// seem to support unsigned types. But it seems like input plugins don't
	// produce those types, so it's hopefully ok.
	case int, int32, int64, float32, float64:
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
