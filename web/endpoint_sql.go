package web

import (
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/requestsql"
)

// endpointSQL combines status codes and protocols before calculating the
// request-weighted average. Old snapshots have no tracked denominator.
type endpointSQL struct {
	Method, Path                            string
	Requests, Tracked                       int64
	SQLCount, SQLMax                        int64
	SQLPerRequest                           float64
	ShapeTracked, ShapeCount, ShapeOverflow int64
}

// endpointShapeRows combines HTTP status/protocol variants after route
// normalization. Missing shape tracking in old snapshots remains unmeasured.
type endpointShapeRow struct {
	Method, Path, Shape                             string
	Count, Errors, MaxPerRequest, Tracked, SQLCount int64
	Total                                           time.Duration
	PerRequest                                      float64
}

func endpointShapeRows(entries httpstats.Snapshot) []endpointShapeRow {
	type routeKey struct{ method, path string }
	type shapeKey struct{ method, path, shape string }
	routes := make(map[routeKey]struct{ tracked, sqlCount int64 })
	shapes := make(map[shapeKey]*endpointShapeRow)
	for _, entry := range entries {
		route := routeKey{entry.Method, entry.Path}
		coverage := routes[route]
		coverage.tracked += entry.SQLShapeTrackedRequests
		coverage.sqlCount += entry.SQLCount
		routes[route] = coverage
		for _, shape := range entry.SQLShapes {
			key := shapeKey{entry.Method, entry.Path, shape.Key}
			row := shapes[key]
			if row == nil {
				row = &endpointShapeRow{Method: entry.Method, Path: entry.Path, Shape: shape.Key}
				shapes[key] = row
			}
			row.Count += shape.Count
			row.Total += shape.Total
			row.Errors += shape.Errors
			if shape.MaxPerRequest > row.MaxPerRequest {
				row.MaxPerRequest = shape.MaxPerRequest
			}
		}
	}
	rows := make([]endpointShapeRow, 0, len(shapes))
	for _, row := range shapes {
		coverage := routes[routeKey{row.Method, row.Path}]
		row.Tracked, row.SQLCount = coverage.tracked, coverage.sqlCount
		if row.Tracked > 0 {
			row.PerRequest = float64(row.Count) / float64(row.Tracked)
		}
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Shape < b.Shape
	})
	return rows
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
		row.ShapeTracked += entry.SQLShapeTrackedRequests
		row.SQLCount += entry.SQLCount
		for _, shape := range entry.SQLShapes {
			row.ShapeCount += shape.Count
			if shape.Key == requestsql.OverflowShape {
				row.ShapeOverflow += shape.Count
			}
		}
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
