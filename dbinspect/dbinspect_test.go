package dbinspect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

func init() {
	sql.Register("mysql-inspect-success", inspectDriver{})
	sql.Register("mysql-inspect-index-error", inspectDriver{indexError: true})
}

type inspectDriver struct{ indexError bool }

func (d inspectDriver) Open(string) (driver.Conn, error) { return inspectConn(d), nil }

type inspectConn inspectDriver

func (inspectConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (inspectConn) Close() error                        { return nil }
func (inspectConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }
func (c inspectConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "information_schema.tables"):
		return &inspectRows{
			columns: []string{"table_name", "engine", "table_rows", "data_length"},
			values: [][]driver.Value{
				{"comments", "InnoDB", int64(100), int64(4096)},
				{"users", "InnoDB", int64(10), int64(2048)},
			},
		}, nil
	case strings.Contains(query, "information_schema.statistics"):
		if c.indexError {
			return nil, errors.New("index query failed")
		}
		return &inspectRows{
			columns: []string{"table_name", "index_name", "non_unique", "columns"},
			values: [][]driver.Value{
				{"comments", "idx_post_created", int64(1), "post_id,created_at"},
				{"comments", "PRIMARY", int64(0), "id"},
				{"missing", "ignored", int64(1), "id"},
			},
		}, nil
	default:
		return nil, errors.New("unexpected query")
	}
}

type inspectRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *inspectRows) Columns() []string { return r.columns }
func (*inspectRows) Close() error        { return nil }
func (r *inspectRows) Next(dst []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dst, r.values[r.index])
	r.index++
	return nil
}

func TestFlavorOf(t *testing.T) {
	tests := []struct {
		driver string
		want   string
	}{
		{"mysql", "mysql"},
		{"mysql:isutools", "mysql"},
		{"pgx", "postgres"},
		{"postgres", "postgres"},
		{"sqlite3", "sqlite"},
	}
	for _, tt := range tests {
		if got := flavorOf(tt.driver); got != tt.want {
			t.Errorf("flavorOf(%q) = %q, want %q", tt.driver, got, tt.want)
		}
	}
}

func TestCollectUnsupportedFlavor(t *testing.T) {
	s := Collect(context.Background(), "sqlite3", "file.db")
	if s.Error == "" {
		t.Error("unsupported flavor must set Error")
	}
	if !strings.Contains(s.Error, "MySQL") {
		t.Errorf("Error = %q, want mention of supported flavors", s.Error)
	}
}

func TestCollectUnknownDriverFailsOpen(t *testing.T) {
	s := Collect(context.Background(), "mysql-not-registered-here", "dsn")
	if s == nil {
		t.Fatal("Collect must never return nil")
	}
	if s.Error == "" {
		t.Error("unknown driver must set Error, not panic")
	}
}

func TestCollectMySQLSchema(t *testing.T) {
	s := Collect(context.Background(), "mysql-inspect-success", "dsn")
	if s.Error != "" {
		t.Fatalf("Collect error = %q", s.Error)
	}
	if s.Flavor != "mysql" || s.CapturedAt == "" || len(s.Tables) != 2 {
		t.Fatalf("schema = %#v", s)
	}
	comments := s.Tables[0]
	if comments.Name != "comments" || comments.Rows != 100 || comments.DataBytes != 4096 {
		t.Fatalf("comments = %#v", comments)
	}
	if len(comments.Indexes) != 2 || comments.Indexes[0].Unique || !comments.Indexes[1].Unique {
		t.Fatalf("indexes = %#v", comments.Indexes)
	}
}

func TestCollectMySQLIndexErrorFailsOpen(t *testing.T) {
	s := Collect(context.Background(), "mysql-inspect-index-error", "dsn")
	if !strings.Contains(s.Error, "index query failed") {
		t.Fatalf("Error = %q", s.Error)
	}
}
