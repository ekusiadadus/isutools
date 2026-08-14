// Package dbcap publishes credential-free, per-target database capabilities.
package dbcap

import (
	"sort"
	"strings"

	"github.com/ekusiadadus/isutools/sqlstats"
)

const SchemaVersion = "isutools.db-capabilities/v1"

type State string

const (
	Supported          State = "supported"
	Partial            State = "partial"
	Unsupported        State = "unsupported"
	ConfigMissing      State = "config-missing"
	VersionUnsupported State = "version-unsupported"
	Unverified         State = "unverified"
	Failed             State = "failed"
)

type Capability struct {
	Feature string `json:"feature"`
	State   State  `json:"state"`
	Reason  string `json:"reason"`
}

type Target struct {
	SchemaVersion string       `json:"schema_version"`
	TargetID      string       `json:"target_id"`
	Dialect       string       `json:"dialect"`
	Flavor        string       `json:"flavor"`
	Version       string       `json:"version,omitempty"`
	Capabilities  []Capability `json:"capabilities"`
}

// Evidence is optional, already-sanitized server evidence. Unknown fields do
// not override fail-closed static capability states.
type Evidence struct {
	Flavor             string
	Version            string
	Schema             State
	RowEfficiency      State
	QueryPlan          State
	CapabilityProbeRan bool
}

type MatrixRow struct {
	Feature     string `json:"feature"`
	MySQL       State  `json:"mysql"`
	MariaDB     State  `json:"mariadb"`
	PostgreSQL  State  `json:"postgresql"`
	SQLite      State  `json:"sqlite"`
	Requirement string `json:"requirement"`
}

// CanonicalMatrix is the single feature-by-dialect support contract rendered
// by the UI and copied into the integration guide.
func CanonicalMatrix() []MatrixRow {
	return []MatrixRow{
		{Feature: "sql_aggregate", MySQL: Supported, MariaDB: Supported, PostgreSQL: Supported, SQLite: Supported, Requirement: "database/sql proxy driver"},
		{Feature: "db_pool", MySQL: Supported, MariaDB: Supported, PostgreSQL: Supported, SQLite: Supported, Requirement: "registered *sql.DB"},
		{Feature: "schema", MySQL: Supported, MariaDB: Supported, PostgreSQL: Unsupported, SQLite: Unsupported, Requirement: "MySQL-compatible information_schema"},
		{Feature: "row_efficiency", MySQL: Supported, MariaDB: Unsupported, PostgreSQL: Unsupported, SQLite: Unsupported, Requirement: "MySQL performance_schema capability probe"},
		{Feature: "query_plan", MySQL: Supported, MariaDB: Unsupported, PostgreSQL: Unsupported, SQLite: Unsupported, Requirement: "MySQL 8.0.17+ and dedicated explain credential"},
		{Feature: "advisor", MySQL: Supported, MariaDB: Partial, PostgreSQL: Unsupported, SQLite: Unsupported, Requirement: "dialect-specific rules"},
	}
}

// ForTargets builds deterministic per-target records. Target Display and all
// credentials are intentionally ignored.
func ForTargets(infos []sqlstats.TargetInfo, evidence map[string]Evidence) []Target {
	result := make([]Target, 0, len(infos))
	for _, info := range infos {
		result = append(result, forTarget(info, evidence[info.ID]))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TargetID < result[j].TargetID })
	return result
}

func forTarget(info sqlstats.TargetInfo, ev Evidence) Target {
	dialect, flavor := dialectFor(info.Driver)
	if safeFlavor(ev.Flavor) != "" {
		flavor = safeFlavor(ev.Flavor)
	}
	target := Target{SchemaVersion: SchemaVersion, TargetID: info.ID, Dialect: dialect, Flavor: flavor, Version: safeVersion(ev.Version)}
	add := func(feature string, state State, reason string) {
		target.Capabilities = append(target.Capabilities, Capability{Feature: feature, State: state, Reason: reason})
	}
	switch dialect {
	case "mysql":
		add("sql_aggregate", Supported, "database-sql-proxy")
		add("db_pool", Supported, "database-sql-pool")
		add("schema", evidenceState(ev.Schema, Supported), evidenceReason(ev.Schema, "mysql-information-schema"))
		rowsState := ev.RowEfficiency
		if rowsState == "" {
			if flavor == "mariadb" {
				rowsState = Unsupported
			} else {
				rowsState = Unverified
			}
		}
		add("row_efficiency", rowsState, stateReason(rowsState, "performance-schema-probe-required"))
		planState := ev.QueryPlan
		if planState == "" {
			if flavor == "mariadb" {
				planState = Unsupported
			} else if !hasPurpose(info.Purposes, sqlstats.PurposeExplain) {
				planState = ConfigMissing
			} else {
				planState = Unverified
			}
		}
		add("query_plan", planState, stateReason(planState, "mysql-version-probe-required"))
		advisorState := Supported
		if flavor == "mariadb" {
			advisorState = Partial
		}
		add("advisor", advisorState, stateReason(advisorState, "mysql-rule-set"))
	case "postgresql":
		add("sql_aggregate", Supported, "database-sql-proxy")
		add("db_pool", Supported, "database-sql-pool")
		for _, feature := range []string{"schema", "row_efficiency", "query_plan", "advisor"} {
			add(feature, Unsupported, "postgresql-deep-adapter-not-installed")
		}
	case "sqlite":
		add("sql_aggregate", Supported, "database-sql-proxy")
		add("db_pool", Supported, "database-sql-pool")
		for _, feature := range []string{"schema", "row_efficiency", "query_plan", "advisor"} {
			add(feature, Unsupported, "sqlite-deep-adapter-not-installed")
		}
	default:
		for _, feature := range []string{"sql_aggregate", "db_pool", "schema", "row_efficiency", "query_plan", "advisor"} {
			add(feature, Unverified, "unknown-dialect")
		}
	}
	return target
}

func dialectFor(driver string) (string, string) {
	value := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(driver, sqlstats.DriverSuffix)))
	switch {
	case strings.Contains(value, "mysql"), strings.Contains(value, "maria"):
		return "mysql", "mysql-compatible"
	case value == "pgx", strings.Contains(value, "postgres"), strings.Contains(value, "pq"):
		return "postgresql", "postgresql"
	case strings.Contains(value, "sqlite"):
		return "sqlite", "sqlite"
	default:
		return "unknown", "unknown"
	}
}

func hasPurpose(values []sqlstats.Purpose, want sqlstats.Purpose) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func validState(state State) bool {
	switch state {
	case Supported, Partial, Unsupported, ConfigMissing, VersionUnsupported, Unverified, Failed:
		return true
	}
	return false
}

func evidenceState(value, fallback State) State {
	if validState(value) {
		return value
	}
	return fallback
}

func evidenceReason(value State, fallback string) string {
	if validState(value) {
		return stateReason(value, fallback)
	}
	return fallback
}

func stateReason(state State, fallback string) string {
	switch state {
	case Unsupported:
		return "dialect-unsupported"
	case ConfigMissing:
		return "credential-missing"
	case VersionUnsupported:
		return "server-version-unsupported"
	case Failed:
		return "capability-probe-failed"
	case Unverified:
		return fallback
	case Partial:
		return "partial-rule-set"
	default:
		return fallback
	}
}

func safeFlavor(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "mysql":
		return "mysql"
	case "mariadb":
		return "mariadb"
	case "postgresql", "postgres":
		return "postgresql"
	default:
		return ""
	}
}

func safeVersion(value string) string {
	if len(value) > 32 {
		return ""
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			if r < 'A' || r > 'Z' {
				if r < 'a' || r > 'z' {
					if !strings.ContainsRune("._-+", r) {
						return ""
					}
				}
			}
		}
	}
	return value
}
