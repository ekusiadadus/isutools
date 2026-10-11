package web

import (
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
)

type endpointWork struct {
	Method, Path string
	Requests     int64
	Work         httpstats.RequestWork
}

func endpointWorkRows(entries httpstats.Snapshot) []endpointWork {
	type key struct{ method, path string }
	groups := make(map[key]*endpointWork)
	for _, e := range entries {
		k := key{e.Method, e.Path}
		row := groups[k]
		if row == nil {
			row = &endpointWork{Method: e.Method, Path: e.Path}
			groups[k] = row
		}
		row.Requests += e.Count
		if e.Work == nil {
			continue
		}
		s, d := e.Work, &row.Work
		d.TrackedRequests += s.TrackedRequests
		d.CompleteRequests += s.CompleteRequests
		d.TruncatedRequests += s.TruncatedRequests
		d.Total += s.Total
		d.CompleteTotal += s.CompleteTotal
		d.SQLTotal += s.SQLTotal
		d.ExternalTotal += s.ExternalTotal
		d.SQLOnly += s.SQLOnly
		d.ExternalOnly += s.ExternalOnly
		d.Overlap += s.Overlap
		d.Other += s.Other
		d.ExternalCount += s.ExternalCount
		d.ExternalErrors += s.ExternalErrors
		for _, call := range s.Calls {
			idx := -1
			for i := range d.Calls {
				if d.Calls[i].Label == call.Label {
					idx = i
					break
				}
			}
			if idx < 0 {
				d.Calls = append(d.Calls, httpstats.RequestCall{Label: call.Label})
				idx = len(d.Calls) - 1
			}
			c := &d.Calls[idx]
			c.Count += call.Count
			c.Errors += call.Errors
			c.Reused += call.Reused
			c.Total += call.Total
			c.DNS += call.DNS
			c.Connect += call.Connect
			c.TLS += call.TLS
			c.Wait += call.Wait
			c.Headers += call.Headers
			c.Body += call.Body
			c.Acquire += call.Acquire
		}
	}
	rows := make([]endpointWork, 0, len(groups))
	for _, r := range groups {
		sort.Slice(r.Work.Calls, func(i, j int) bool { return r.Work.Calls[i].Label < r.Work.Calls[j].Label })
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Work.ExternalTotal != rows[j].Work.ExternalTotal {
			return rows[i].Work.ExternalTotal > rows[j].Work.ExternalTotal
		}
		if rows[i].Method != rows[j].Method {
			return rows[i].Method < rows[j].Method
		}
		return rows[i].Path < rows[j].Path
	})
	return rows
}

func workAverage(d time.Duration, n int64) time.Duration {
	if n <= 0 {
		return 0
	}
	return d / time.Duration(n)
}
func workRatio(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}
