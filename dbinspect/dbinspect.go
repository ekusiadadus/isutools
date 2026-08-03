// Package dbinspect captures the database schema state (tables, row counts,
// indexes) so every benchmark snapshot records what indexes existed BEFORE
// the run. Inspection uses the raw (unproxied) driver so its own queries
// never pollute the SQL statistics.
package dbinspect

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const collectTimeout = 5 * time.Second

// Index is one index on a table.
type Index struct {
	Name    string `json:"name"`
	Columns string `json:"columns"`
	Unique  bool   `json:"unique"`
}

// Table is one table with its indexes.
type Table struct {
	Name      string  `json:"name"`
	Engine    string  `json:"engine"`
	Rows      int64   `json:"rows"`
	DataBytes int64   `json:"data_bytes"`
	Indexes   []Index `json:"indexes"`
}

// Schema is the captured database state. Error is set instead of failing:
// inspection is best-effort and must never break measurement.
type Schema struct {
	Flavor     string  `json:"flavor"`
	CapturedAt string  `json:"captured_at"`
	Error      string  `json:"error,omitempty"`
	Tables     []Table `json:"tables"`
}

func flavorOf(driverName string) string {
	name := strings.ToLower(driverName)
	switch {
	case strings.Contains(name, "mysql"):
		return "mysql"
	case strings.Contains(name, "pg") || strings.Contains(name, "postgres"):
		return "postgres"
	default:
		return "unknown"
	}
}

// Collect inspects the schema through driverName+dsn. It always returns a
// non-nil Schema; failures are reported via Schema.Error.
func Collect(ctx context.Context, driverName, dsn string) *Schema {
	s := &Schema{
		Flavor:     flavorOf(driverName),
		CapturedAt: time.Now().Format(time.RFC3339),
	}
	if s.Flavor != "mysql" {
		s.Error = "schema inspection supports MySQL/MariaDB only for now (flavor: " + s.Flavor + ")"
		return s
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()

	tables, err := collectMySQLTables(ctx, db)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Tables = tables
	return s
}

func collectMySQLTables(ctx context.Context, db *sql.DB) ([]Table, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT table_name, COALESCE(engine, ''), COALESCE(table_rows, 0), COALESCE(data_length, 0)
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byName := map[string]*Table{}
	names := []string{}
	for rows.Next() {
		t := Table{}
		if err := rows.Scan(&t.Name, &t.Engine, &t.Rows, &t.DataBytes); err != nil {
			return nil, err
		}
		byName[t.Name] = &t
		names = append(names, t.Name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	idxRows, err := db.QueryContext(ctx, `
		SELECT table_name, index_name, non_unique,
		       GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
		FROM information_schema.statistics
		WHERE table_schema = DATABASE()
		GROUP BY table_name, index_name, non_unique
		ORDER BY table_name, index_name`)
	if err != nil {
		return nil, err
	}
	defer idxRows.Close()

	for idxRows.Next() {
		var tableName, indexName, columns string
		var nonUnique int64
		if err := idxRows.Scan(&tableName, &indexName, &nonUnique, &columns); err != nil {
			return nil, err
		}
		t, ok := byName[tableName]
		if !ok {
			continue
		}
		t.Indexes = append(t.Indexes, Index{
			Name:    indexName,
			Columns: columns,
			Unique:  nonUnique == 0,
		})
	}
	if err := idxRows.Err(); err != nil {
		return nil, err
	}

	tables := make([]Table, 0, len(names))
	for _, n := range names {
		tables = append(tables, *byName[n])
	}
	return tables, nil
}
