package queryplan

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// leakMarker stands in for a literal that must never reach a snapshot. MySQL's
// 1064 quotes the offending part of a statement back ("near '...'"), so an
// error carrying it is the realistic shape of the leak this package's error
// handling exists to close.
const leakMarker = "ZZ_ISUTOOLS_MARKER_ZZ"

// driverErrorf builds an error the way the registry hands one over: the
// driver's own "Error <n> (<sqlstate>): <message>" wrapped in the registry's
// own context.
func driverErrorf(errno int, sqlstate, message string) error {
	return fmt.Errorf("isutools: database driver failed: inspect query for %q/%s (tcp(127.0.0.1:3306)/isuconp): Error %d (%s): %s",
		"db1", "explain", errno, sqlstate, message)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass PlanErrorClass
		wantErrno uint16
		wantState string
	}{
		{
			name:      "syntax error quoting the statement",
			err:       driverErrorf(1064, "42000", "You have an error in your SQL syntax; check the manual near '"+leakMarker+"' at line 1"),
			wantClass: PlanErrSyntax, wantErrno: 1064, wantState: "42000",
		},
		{
			name:      "table denied to the least-privilege user",
			err:       driverErrorf(1142, "42000", "SELECT command denied to user 'isutools_explain'@'localhost' for table 'posts'"),
			wantClass: PlanErrPermission, wantErrno: 1142, wantState: "42000",
		},
		{
			name:      "no database selected",
			err:       driverErrorf(errnoNoDatabase, "3D000", "No database selected"),
			wantClass: PlanErrObjectMissing, wantErrno: errnoNoDatabase, wantState: "3D000",
		},
		{
			name:      "table gone",
			err:       driverErrorf(1146, "42S02", "Table 'isuconp.posts' doesn't exist"),
			wantClass: PlanErrObjectMissing, wantErrno: 1146, wantState: "42S02",
		},
		{
			name:      "statement interrupted",
			err:       driverErrorf(1317, "70100", "Query execution was interrupted"),
			wantClass: PlanErrTimeout, wantErrno: 1317, wantState: "70100",
		},
		{
			name:      "server went away",
			err:       driverErrorf(2006, "HY000", "MySQL server has gone away"),
			wantClass: PlanErrConnection, wantErrno: 2006, wantState: "HY000",
		},
		{
			name:      "an errno with no entry keeps the number",
			err:       driverErrorf(1264, "22003", "Out of range value"),
			wantClass: PlanErrOther, wantErrno: 1264, wantState: "22003",
		},
		{
			name:      "older drivers omit the sqlstate",
			err:       errors.New("Error 1064: syntax error near '" + leakMarker + "'"),
			wantClass: PlanErrSyntax, wantErrno: 1064,
		},
		{
			name:      "deadline",
			err:       fmt.Errorf("inspect query: %w", context.DeadlineExceeded),
			wantClass: PlanErrTimeout,
		},
		{
			name:      "cancellation is a budget cutoff too",
			err:       context.Canceled,
			wantClass: PlanErrTimeout,
		},
		{
			name:      "bad connection",
			err:       fmt.Errorf("inspect query: %w", driver.ErrBadConn),
			wantClass: PlanErrConnection,
		},
		{
			name:      "an error with nothing recognisable in it",
			err:       errors.New("something went wrong with " + leakMarker),
			wantClass: PlanErrOther,
		},
		{
			// A driver returning a marker where a SQLSTATE belongs must not be
			// able to write it into a snapshot: the field is only filled when
			// the value has the exact five-character shape.
			name:      "a sqlstate that is not one",
			err:       errors.New("Error 1064 (" + leakMarker + "): nope"),
			wantClass: PlanErrOther,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			if got == nil {
				t.Fatal("classify returned nil for a non-nil error")
			}
			if got.Class != tc.wantClass {
				t.Fatalf("class = %q, want %q", got.Class, tc.wantClass)
			}
			if got.Errno != tc.wantErrno {
				t.Fatalf("errno = %d, want %d", got.Errno, tc.wantErrno)
			}
			if got.SQLState != tc.wantState {
				t.Fatalf("sqlstate = %q, want %q", got.SQLState, tc.wantState)
			}
			if leaked := fmt.Sprintf("%+v", *got); strings.Contains(leaked, leakMarker) {
				t.Fatalf("the classification carries the statement's literal: %s", leaked)
			}
		})
	}
	if classify(nil) != nil {
		t.Fatal("classify(nil) must stay nil")
	}
}

// TestPlanErrorHasNoFreeText is a regression guard on the type itself. The
// whole leak argument rests on PlanError having no field a driver message
// could be dropped into, so adding one has to fail here rather than in a
// snapshot someone pastes into a chat window.
func TestPlanErrorHasNoFreeText(t *testing.T) {
	allowed := map[string]bool{"Class": true, "SQLState": true}
	planErrorType := reflect.TypeOf(PlanError{})
	for i := 0; i < planErrorType.NumField(); i++ {
		field := planErrorType.Field(i)
		if field.Type.Kind() != reflect.String {
			continue
		}
		if !allowed[field.Name] {
			t.Fatalf("PlanError.%s is a free string field: a driver message must not have a home on this type", field.Name)
		}
	}
	// And the two that are allowed are closed vocabularies, not free text.
	if planErrorType.Field(0).Type != reflect.TypeOf(PlanErrorClass("")) {
		t.Fatal("PlanError.Class must keep its own type, so only the constants can be assigned to it")
	}
}

func TestSQLStatePattern(t *testing.T) {
	valid := []string{"42000", "HY000", "3D000", "42S02"}
	invalid := []string{"", "4200", "420000", "42s02", "ZZ_MA", "42-00"}
	for _, state := range valid {
		if !sqlStatePattern.MatchString(state) {
			t.Fatalf("sqlstate %q was rejected", state)
		}
	}
	for _, state := range invalid {
		// "ZZ_MA" is here because an underscore is not one of [0-9A-Z]: a
		// marker a driver shaped like a SQLSTATE is rejected by the pattern
		// itself, not only by the surrounding "Error <n> (...):" form.
		if sqlStatePattern.MatchString(state) {
			t.Fatalf("sqlstate %q was accepted", state)
		}
	}
}
