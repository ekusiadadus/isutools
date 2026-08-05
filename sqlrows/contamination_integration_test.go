//go:build integration

package sqlrows

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

const integrationDSNEnv = "ISUTOOLS_INTEGRATION_MYSQL_DSN"

var integrationSequence atomic.Uint64

type contaminationFixture struct {
	t       *testing.T
	admin   *sql.DB
	target  sqlstats.TargetInfo
	primary *sql.DB
	other   *sql.DB
}

func newContaminationFixture(t *testing.T) *contaminationFixture {
	t.Helper()
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
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect to integration MySQL: %v", err)
	}
	var primary, other *sql.DB
	createdSchemas := []string{}
	t.Cleanup(func() {
		sqlstats.CloseDBInspectors()
		if primary != nil {
			_ = primary.Close()
		}
		if other != nil {
			_ = other.Close()
		}
		for _, schema := range createdSchemas {
			_, _ = admin.Exec("DROP DATABASE `" + schema + "`")
		}
		_ = admin.Close()
	})

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), integrationSequence.Add(1))
	primarySchema := "isutools_it_primary_" + suffix
	otherSchema := "isutools_it_other_" + suffix
	for _, schema := range []string{primarySchema, otherSchema} {
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+schema+"`"); err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
		createdSchemas = append(createdSchemas, schema)
		if _, err := admin.ExecContext(ctx, "CREATE TABLE `"+schema+"`.contamination_probe (id BIGINT PRIMARY KEY)"); err != nil {
			t.Fatalf("create table in %s: %v", schema, err)
		}
		if _, err := admin.ExecContext(ctx, "INSERT INTO `"+schema+"`.contamination_probe (id) VALUES (1)"); err != nil {
			t.Fatalf("seed %s: %v", schema, err)
		}
	}
	openSchema := func(schema string) *sql.DB {
		copyCfg := *cfg
		copyCfg.DBName = schema
		copyCfg.ParseTime = true
		copyCfg.Loc = time.UTC
		// This test is about schema attribution and collector
		// self-contamination. Use the text protocol so the assertion does not
		// also depend on a particular MySQL release's binary prepared-statement
		// digest implementation; MySQL still normalizes the literal to one
		// digest.
		copyCfg.InterpolateParams = true
		db, err := sql.Open("mysql", copyCfg.FormatDSN())
		if err != nil {
			t.Fatalf("open schema %s: %v", schema, err)
		}
		return db
	}
	primary = openSchema(primarySchema)
	other = openSchema(otherSchema)

	const requiredConsumers = 4
	if _, err := admin.ExecContext(ctx, `UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME IN (`+
		`'global_instrumentation', 'thread_instrumentation', 'events_statements_current', 'statements_digest')`); err != nil {
		t.Fatalf("enable statement consumers: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME LIKE 'statement/%'`); err != nil {
		t.Fatalf("enable statement instruments: %v", err)
	}
	var enabledConsumers int
	if err := admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.setup_consumers `+
		`WHERE NAME IN ('global_instrumentation', 'thread_instrumentation', 'events_statements_current', 'statements_digest') `+
		`AND ENABLED = 'YES'`).Scan(&enabledConsumers); err != nil {
		t.Fatalf("verify statement consumers: %v", err)
	}
	if enabledConsumers != requiredConsumers {
		t.Fatalf("enabled statement consumers = %d, want %d", enabledConsumers, requiredConsumers)
	}
	if _, err := admin.ExecContext(ctx, `TRUNCATE TABLE performance_schema.events_statements_summary_by_digest`); err != nil {
		t.Fatalf("truncate digest table: %v", err)
	}
	// Fail at fixture setup with an actionable message when the service is not
	// recording schema digests. Otherwise both interval assertions merely say
	// "got zero", which hides whether the collector or MySQL setup failed.
	var probeID int64
	if err := primary.QueryRowContext(ctx, `SELECT id FROM contamination_probe WHERE id = ?`, 1).Scan(&probeID); err != nil {
		t.Fatalf("digest readiness query: %v", err)
	}
	var recorded uint64
	if err := admin.QueryRowContext(ctx, `SELECT COALESCE(SUM(COUNT_STAR), 0) `+
		`FROM performance_schema.events_statements_summary_by_digest WHERE SCHEMA_NAME = ?`, primarySchema).Scan(&recorded); err != nil {
		t.Fatalf("verify digest readiness: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("performance_schema recorded %d statements for readiness query in %s, want 1", recorded, primarySchema)
	}
	if _, err := admin.ExecContext(ctx, `TRUNCATE TABLE performance_schema.events_statements_summary_by_digest`); err != nil {
		t.Fatalf("clear digest readiness query: %v", err)
	}

	targetID := "integration-" + suffix
	if err := sqlstats.RegisterDBTarget(targetID, "mysql", dsnForSchema(t, cfg, primarySchema)); err != nil {
		t.Fatalf("RegisterDBTarget: %v", err)
	}
	if err := sqlstats.RegisterDBInspector(targetID, sqlstats.PurposeStats, "mysql", cfg.FormatDSN()); err != nil {
		t.Fatalf("RegisterDBInspector: %v", err)
	}
	info, ok := sqlstats.Target(targetID)
	if !ok {
		t.Fatalf("target %q not found after registration", targetID)
	}
	return &contaminationFixture{t: t, admin: admin, target: info, primary: primary, other: other}
}

func dsnForSchema(t *testing.T, base *mysql.Config, schema string) string {
	t.Helper()
	copyCfg := *base
	copyCfg.DBName = schema
	copyCfg.ParseTime = true
	copyCfg.Loc = time.UTC
	copyCfg.InterpolateParams = false
	copyCfg.MultiStatements = false
	return copyCfg.FormatDSN()
}

func (f *contaminationFixture) collector() *Collector {
	c := New()
	c.targets = func() []sqlstats.TargetInfo { return []sqlstats.TargetInfo{f.target} }
	return c
}

func (f *contaminationFixture) query(db *sql.DB, n int) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range n {
		var id int64
		if err := db.QueryRowContext(ctx, `SELECT id FROM contamination_probe WHERE id = ?`, 1).Scan(&id); err != nil {
			f.t.Fatalf("application query: %v", err)
		}
	}
}

func (f *contaminationFixture) interval(primaryQueries, otherQueries int) *Section {
	f.t.Helper()
	c := f.collector()
	base, err := c.CaptureBaseline(context.Background(), "integration-run", 1)
	if err != nil {
		f.t.Fatalf("CaptureBaseline: %v", err)
	}
	f.query(f.primary, primaryQueries)
	f.query(f.other, otherQueries)
	final, err := c.CaptureFinal(context.Background(), "integration-run", 1)
	if err != nil {
		f.t.Fatalf("CaptureFinal: %v", err)
	}
	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		f.t.Fatalf("Collect: %v", err)
	}
	section, ok := value.(*Section)
	if !ok {
		f.t.Fatalf("section type = %T", value)
	}
	return section
}

func (f *contaminationFixture) nullSchemaDigests() map[string]struct{} {
	f.t.Helper()
	rows, err := f.admin.Query(`SELECT DIGEST FROM performance_schema.events_statements_summary_by_digest WHERE SCHEMA_NAME IS NULL AND DIGEST IS NOT NULL`)
	if err != nil {
		f.t.Fatalf("read NULL-schema digests: %v", err)
	}
	defer func() { _ = rows.Close() }()
	digests := map[string]struct{}{}
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			f.t.Fatalf("scan NULL-schema digest: %v", err)
		}
		digests[digest] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate NULL-schema digests: %v", err)
	}
	return digests
}

func (f *contaminationFixture) assertInspectorHasNoDefaultDatabase() {
	f.t.Helper()
	err := sqlstats.Inspect(context.Background(), f.target.ID, sqlstats.PurposeStats,
		func(ctx context.Context, q sqlstats.Querier) error {
			var database sql.NullString
			if err := q.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
				return err
			}
			if database.Valid {
				return fmt.Errorf("inspector default database = %q", database.String)
			}
			return nil
		})
	if err != nil {
		f.t.Fatal(err)
	}
}

func onlyUsableTarget(t *testing.T, section *Section) TargetSection {
	t.Helper()
	if section.Validity != runctl.ValidityValid || len(section.Targets) != 1 || !section.Targets[0].Usable {
		t.Fatalf("section = %+v", section)
	}
	return section.Targets[0]
}

func TestNoSelfContamination(t *testing.T) {
	f := newContaminationFixture(t)
	section := f.interval(7, 0)
	target := onlyUsableTarget(t, section)
	collectorDigests := f.nullSchemaDigests()
	if len(collectorDigests) == 0 {
		t.Fatal("collector created no NULL-schema digests; fixture is not observing its statements")
	}
	total := uint64(0)
	for _, digest := range target.Digests {
		if _, contaminated := collectorDigests[digest.Digest]; contaminated {
			t.Fatalf("collector digest %s leaked into application interval (%q)", digest.Digest, digest.Query)
		}
		if strings.Contains(strings.ToUpper(digest.Query), "PERFORMANCE_SCHEMA") {
			t.Fatalf("collector statement leaked into interval: %q", digest.Query)
		}
		total += digest.Count
	}
	if target.Total != 1 || total != 7 {
		t.Fatalf("target total=%d count=%d digests=%+v, want only seven application queries", target.Total, total, target.Digests)
	}
	f.assertInspectorHasNoDefaultDatabase()
}

func TestNoSelfContamination_MultiSchema(t *testing.T) {
	f := newContaminationFixture(t)
	section := f.interval(4, 6)
	target := onlyUsableTarget(t, section)
	if target.Total != 1 || len(target.Digests) != 1 || target.Digests[0].Count != 4 {
		t.Fatalf("primary interval = %+v, want 4 and not the 6 identical queries from the other schema", target)
	}
}
