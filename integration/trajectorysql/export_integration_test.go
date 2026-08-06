//go:build integration

package trajectorysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

const integrationDSNEnv = "ISUTOOLS_INTEGRATION_MYSQL_DSN"

var schemaSequence atomic.Uint64

func TestISUCON14TrajectoryExportExecutesOnMySQL8(t *testing.T) {
	baseDSN := os.Getenv(integrationDSNEnv)
	if baseDSN == "" {
		t.Skip(integrationDSNEnv + " is not configured")
	}
	cfg, err := mysql.ParseDSN(baseDSN)
	if err != nil {
		t.Fatalf("parse %s: %v", integrationDSNEnv, err)
	}
	if cfg.DBName != "" {
		t.Fatalf("%s must not select a default database", integrationDSNEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("connect to integration MySQL: %v", err)
	}
	var version string
	if err := admin.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatalf("read MySQL version: %v", err)
	}
	if !strings.HasPrefix(version, "8.") {
		t.Fatalf("MySQL version = %q, want 8.x", version)
	}

	schema := fmt.Sprintf("isutools_trajectory_%d_%d", time.Now().UnixNano(), schemaSequence.Add(1))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+schema+"`"); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE `" + schema + "`")
	})

	testCfg := *cfg
	testCfg.DBName = schema
	testCfg.MultiStatements = true
	testCfg.ParseTime = true
	testCfg.Loc = time.UTC
	db, err := sql.Open("mysql", testCfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	setup := []string{
		`CREATE TABLE chairs (
			id VARCHAR(64) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			model VARCHAR(255) NOT NULL
		)`,
		`CREATE TABLE rides (
			id VARCHAR(64) PRIMARY KEY,
			chair_id VARCHAR(64),
			created_at DATETIME(6) NOT NULL,
			pickup_latitude BIGINT NOT NULL,
			pickup_longitude BIGINT NOT NULL,
			destination_latitude BIGINT NOT NULL,
			destination_longitude BIGINT NOT NULL
		)`,
		`CREATE TABLE chair_locations (
			id VARCHAR(64) PRIMARY KEY,
			chair_id VARCHAR(64) NOT NULL,
			created_at DATETIME(6) NOT NULL,
			latitude BIGINT NOT NULL,
			longitude BIGINT NOT NULL
		)`,
		`CREATE TABLE ride_statuses (
			ride_id VARCHAR(64) NOT NULL,
			status VARCHAR(64) NOT NULL,
			created_at DATETIME(6) NOT NULL
		)`,
		`INSERT INTO chairs (id, name, model) VALUES ('chair-1', 'Chair 1', 'speedy')`,
		`INSERT INTO rides (
			id, chair_id, created_at, pickup_latitude, pickup_longitude,
			destination_latitude, destination_longitude
		) VALUES ('ride-1', 'chair-1', '2026-08-05 21:39:30.000000', 10, 20, 30, 40)`,
		`INSERT INTO chair_locations (id, chair_id, created_at, latitude, longitude) VALUES
			('location-1', 'chair-1', '2026-08-05 21:38:59.000000', 1, 2),
			('location-2', 'chair-1', '2026-08-05 21:40:00.000000', 3, 4)`,
		`INSERT INTO ride_statuses (ride_id, status, created_at) VALUES
			('ride-1', 'ENROUTE', '2026-08-05 21:40:30.000000'),
			('ride-1', 'COMPLETED', '2026-08-05 21:41:30.000000')`,
	}
	for _, statement := range setup {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("fixture statement failed: %v\n%s", err, statement)
		}
	}

	script := readExportSQL(t)
	t.Run("reserved alias reproduces syntax error", func(t *testing.T) {
		bad := strings.Replace(script, "AS point_rank", "AS row_number", 1)
		bad = strings.Replace(bad, "WHERE point_rank = 1", "WHERE row_number = 1", 1)
		err := executeAndDrain(ctx, db, bad)
		var myErr *mysql.MySQLError
		if !errors.As(err, &myErr) || myErr.Number != 1064 {
			t.Fatalf("reserved row_number alias error = %v, want MySQL syntax error 1064", err)
		}
		if !strings.Contains(err.Error(), "row_number") {
			t.Logf("MySQL syntax error did not echo the alias: %v", err)
		}
	})

	rows, err := db.QueryContext(ctx, script)
	if err != nil {
		t.Fatalf("execute export-trajectory.sql on MySQL %s: %v", version, err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[string]int{}
	for {
		columns, err := rows.Columns()
		if err != nil {
			t.Fatalf("read result columns: %v", err)
		}
		for rows.Next() {
			if len(columns) != 1 {
				t.Fatalf("result columns = %v, want one NDJSON record column", columns)
			}
			var record string
			if err := rows.Scan(&record); err != nil {
				t.Fatalf("scan NDJSON record: %v", err)
			}
			var value struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(record), &value); err != nil {
				t.Fatalf("invalid NDJSON record %q: %v", record, err)
			}
			counts[value.Type]++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read export result: %v", err)
		}
		if !rows.NextResultSet() {
			if err := rows.Err(); err != nil {
				t.Fatalf("advance export result: %v", err)
			}
			break
		}
	}

	want := map[string]int{"meta": 1, "agent": 1, "point": 2, "job": 1, "assignment": 1}
	for kind, count := range want {
		if counts[kind] != count {
			t.Errorf("%s records = %d, want %d (all counts: %v)", kind, counts[kind], count, counts)
		}
	}
}

func executeAndDrain(ctx context.Context, db *sql.DB, query string) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for {
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !rows.NextResultSet() {
			return rows.Err()
		}
	}
}
