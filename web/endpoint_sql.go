package web

import (
	"sort"

	"github.com/ekusiadadus/isutools/httpstats"
)

// endpointSQL combines status codes and protocols before calculating the
// request-weighted average. Old snapshots have no tracked denominator.
type endpointSQL struct {
	Method, Path      string
	Requests, Tracked int64
	SQLCount, SQLMax  int64
	SQLPerRequest     float64
}

func endpointSQLRows(entries httpstats.Snapshot) []endpointSQL {
	type key struct{ method, path string }
	groups := make(map[key]*endpointSQL)
	for _, entry := range entries {
		k := key{entry.Method, entry.Path}
		row := groups[k]
		if row == nil {
			row = &endpointSQL{Method: entry.Method, Path: entry.Path}
			groups[k] = row
		}
		row.Requests += entry.Count
		row.Tracked += entry.SQLTrackedRequests
		row.SQLCount += entry.SQLCount
		if entry.SQLMaxPerRequest > row.SQLMax {
			row.SQLMax = entry.SQLMaxPerRequest
		}
	}
	rows := make([]endpointSQL, 0, len(groups))
	for _, row := range groups {
		if row.Tracked > 0 {
			row.SQLPerRequest = float64(row.SQLCount) / float64(row.Tracked)
		}
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Tracked > 0) != (b.Tracked > 0) {
			return a.Tracked > 0
		}
		if a.SQLPerRequest != b.SQLPerRequest {
			return a.SQLPerRequest > b.SQLPerRequest
		}
		if a.SQLCount != b.SQLCount {
			return a.SQLCount > b.SQLCount
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Path < b.Path
	})
	return rows
}
