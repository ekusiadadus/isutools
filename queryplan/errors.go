package queryplan

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"regexp"
	"strconv"
)

// PlanErrorClass is the closed set of reasons a digest has no plan.
type PlanErrorClass string

const (
	// PlanErrTimeout: the statement ran out of its slice of the budget.
	PlanErrTimeout PlanErrorClass = "timeout"
	// PlanErrBudgetExhausted: it was never issued, because the remaining
	// budget could not fit it.
	PlanErrBudgetExhausted PlanErrorClass = "budget_exhausted"
	// PlanErrPermission: the least-privilege user may not read something the
	// statement touches. Expected, and worth showing.
	PlanErrPermission PlanErrorClass = "permission_denied"
	// PlanErrSyntax: the server rejected the sample, usually because the
	// recorded text was cut short.
	PlanErrSyntax PlanErrorClass = "syntax_or_truncated"
	// PlanErrObjectMissing: a table, column or database named by the sample
	// does not exist any more.
	PlanErrObjectMissing PlanErrorClass = "object_missing"
	// PlanErrSampleUnavail: performance_schema kept no sample for the digest.
	PlanErrSampleUnavail PlanErrorClass = "sample_unavailable"
	// PlanErrSampleTruncated: the sample is as long as
	// performance_schema_max_sql_text_length, so it may end mid-statement and
	// is not sent to the server at all.
	PlanErrSampleTruncated PlanErrorClass = "sample_possibly_truncated"
	// PlanErrConnection: the connection was lost.
	PlanErrConnection PlanErrorClass = "connection_error"
	// PlanErrOther: anything else. The errno is still recorded.
	PlanErrOther PlanErrorClass = "other"
)

// PlanError is the whole of what a failed EXPLAIN contributes to a snapshot.
//
// The driver's own message is deliberately absent. MySQL's 1064 quotes the
// offending fragment of the statement back at the caller ("near '...'"), and
// the statement here is a sample containing literals, so any design that keeps
// the message and tries to scrub it has already lost: truncation, escaping and
// partial quoting all defeat a substring check. Instead the message is mapped
// to a class inside the callback that produced it and then dropped.
//
// The type is the guarantee. It has exactly two string-shaped fields, both
// drawn from closed vocabularies — a class constant and a validated SQLSTATE —
// and TestPlanErrorHasNoFreeText fails if a third is ever added.
type PlanError struct {
	Class PlanErrorClass `json:"class"`
	// Errno is the driver's numeric error code, 0 when there was none.
	Errno uint16 `json:"errno,omitempty"`
	// SQLState is the five-character SQLSTATE, empty unless it matches
	// ^[0-9A-Z]{5}$ exactly.
	SQLState string `json:"sqlstate,omitempty"`
}

// errnoClass maps the error numbers worth distinguishing. Anything absent
// becomes PlanErrOther with the number kept, which is enough to look it up
// without giving a driver a channel to write free text into a snapshot.
var errnoClass = map[uint16]PlanErrorClass{
	1317: PlanErrTimeout, // query execution was interrupted
	3024: PlanErrTimeout, // exceeded MAX_EXECUTION_TIME

	1044: PlanErrPermission, // access denied for database
	1045: PlanErrPermission, // access denied for user
	1142: PlanErrPermission, // command denied to user for table
	1143: PlanErrPermission, // command denied to user for column
	1370: PlanErrPermission, // execute command denied for routine

	1064: PlanErrSyntax, // syntax error — a truncated sample lands here
	1149: PlanErrSyntax,

	1046: PlanErrObjectMissing, // no database selected: the USE step failed
	1051: PlanErrObjectMissing, // unknown table
	1054: PlanErrObjectMissing, // unknown column
	1109: PlanErrObjectMissing, // unknown table in the statement
	1146: PlanErrObjectMissing, // table doesn't exist

	2006: PlanErrConnection, // server has gone away
	2013: PlanErrConnection, // lost connection during query
}

// errnoNoDatabase is 1046. It means the USE step did not take effect, which is
// a fault of this package rather than of the statement, so the caller records
// it in health as well as on the plan.
const errnoNoDatabase = 1046

// mysqlErrorPattern matches the prefix go-sql-driver/mysql puts on a server
// error: "Error 1064 (42000): ...". Only the two captures are ever read; the
// rest of the message, which is where a fragment of the statement would sit,
// is never touched.
var mysqlErrorPattern = regexp.MustCompile(`Error (\d{1,5}) \(([0-9A-Z]{5})\):`)

// mysqlErrorNoStatePattern matches the older "Error 1064: ..." form.
var mysqlErrorNoStatePattern = regexp.MustCompile(`Error (\d{1,5}):`)

// sqlStatePattern is the shape a SQLSTATE must have to be published.
var sqlStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// classify maps a driver error onto a PlanError. The error itself is not
// retained anywhere: this function's return value is the only thing that
// outlives it.
func classify(err error) *PlanError {
	if err == nil {
		return nil
	}
	out := &PlanError{Class: PlanErrOther}
	errno, state := errorCode(err)
	out.Errno = errno
	if sqlStatePattern.MatchString(state) {
		out.SQLState = state
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		out.Class = PlanErrTimeout
	case errors.Is(err, driver.ErrBadConn), errors.Is(err, io.EOF):
		out.Class = PlanErrConnection
	default:
		if class, ok := errnoClass[errno]; ok {
			out.Class = class
		}
	}
	return out
}

// errorCode extracts the driver's numeric code and SQLSTATE from an error's
// text.
//
// Reading them out of the text rather than out of a typed error is what keeps
// this package free of a driver dependency, and the registry has wrapped the
// original error anyway. Only the two matched groups are read; nothing else
// from the message is returned to the caller, so nothing else can be stored.
func errorCode(err error) (uint16, string) {
	message := err.Error()
	if match := mysqlErrorPattern.FindStringSubmatch(message); match != nil {
		return parseErrno(match[1]), match[2]
	}
	if match := mysqlErrorNoStatePattern.FindStringSubmatch(message); match != nil {
		return parseErrno(match[1]), ""
	}
	return 0, ""
}

func parseErrno(digits string) uint16 {
	n, err := strconv.ParseUint(digits, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}
