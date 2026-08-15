package accessinspect

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

type OutputFormat string

const (
	OutputTable    OutputFormat = "table"
	OutputJSON     OutputFormat = "json"
	OutputMarkdown OutputFormat = "markdown"
	OutputTSV      OutputFormat = "tsv"
	OutputCSV      OutputFormat = "csv"
)

type RenderOptions struct {
	Format  OutputFormat
	Sort    string
	Reverse bool
	Limit   int
	Columns []string
}

var defaultColumns = []string{
	"method", "path", "count", "status_1xx", "status_2xx", "status_3xx", "status_4xx", "status_5xx",
	"request_min_ns", "request_avg_ns", "request_max_ns", "request_stddev_ns", "body_sum", "upstream_sum_ns", "residual_sum_ns", "no_upstream",
}

// Render writes one deterministic representation. JSON retains the full
// report envelope; row-oriented formats contain only selected columns.
func Render(output io.Writer, report Report, options RenderOptions) error {
	if options.Format == "" {
		options.Format = OutputTable
	}
	rows := append([]Row(nil), report.Rows...)
	if err := sortRows(rows, options.Sort, options.Reverse); err != nil {
		return err
	}
	if options.Limit < 0 {
		return errors.New("accessinspect render: invalid-limit")
	}
	if options.Limit > 0 && options.Limit < len(rows) {
		rows = rows[:options.Limit]
	}
	report.Rows = rows
	if options.Format == OutputJSON {
		encoder := json.NewEncoder(output)
		encoder.SetEscapeHTML(true)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	columns := options.Columns
	if len(columns) == 0 {
		columns = defaultColumns
	}
	for _, column := range columns {
		if _, ok := columnValue(Row{}, column); !ok {
			return errors.New("accessinspect render: unsupported-column")
		}
	}
	switch options.Format {
	case OutputTable:
		writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, strings.Join(columns, "\t")); err != nil {
			return err
		}
		for _, row := range rows {
			if _, err := fmt.Fprintln(writer, strings.Join(rowValues(row, columns, false), "\t")); err != nil {
				return err
			}
		}
		return writer.Flush()
	case OutputMarkdown:
		if _, err := fmt.Fprintf(output, "| %s |\n", strings.Join(columns, " | ")); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "| %s |\n", strings.TrimSuffix(strings.Repeat("--- | ", len(columns)), " | ")); err != nil {
			return err
		}
		for _, row := range rows {
			values := rowValues(row, columns, false)
			for index := range values {
				values[index] = strings.ReplaceAll(strings.ReplaceAll(values[index], "\\", "\\\\"), "|", "\\|")
			}
			if _, err := fmt.Fprintf(output, "| %s |\n", strings.Join(values, " | ")); err != nil {
				return err
			}
		}
		return nil
	case OutputCSV, OutputTSV:
		writer := csv.NewWriter(output)
		if options.Format == OutputTSV {
			writer.Comma = '\t'
		}
		if err := writer.Write(columns); err != nil {
			return err
		}
		for _, row := range rows {
			if err := writer.Write(rowValues(row, columns, true)); err != nil {
				return err
			}
		}
		writer.Flush()
		return writer.Error()
	default:
		return errors.New("accessinspect render: unsupported-format")
	}
}

func rowValues(row Row, columns []string, spreadsheetSafe bool) []string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		value, _ := columnValue(row, column)
		if spreadsheetSafe {
			value = neutralizeFormula(value)
		}
		values = append(values, value)
	}
	return values
}

func columnValue(row Row, column string) (string, bool) {
	switch column {
	case "method":
		return row.Method, true
	case "path":
		return row.Path, true
	case "count":
		return strconv.FormatInt(row.Count, 10), true
	case "status_1xx", "status_2xx", "status_3xx", "status_4xx", "status_5xx":
		index := int(column[len("status_")] - '1')
		return strconv.FormatInt(row.StatusClasses[index], 10), true
	case "request_count":
		return strconv.FormatInt(row.Request.Count, 10), true
	case "request_min_ns":
		return strconv.FormatInt(int64(row.Request.Min), 10), true
	case "request_max_ns":
		return strconv.FormatInt(int64(row.Request.Max), 10), true
	case "request_sum_ns":
		return strconv.FormatInt(int64(row.Request.Sum), 10), true
	case "request_avg_ns":
		return strconv.FormatInt(int64(row.Request.Avg), 10), true
	case "request_stddev_ns":
		return strconv.FormatInt(int64(row.Request.Stddev), 10), true
	case "body_count":
		return strconv.FormatInt(row.Body.Count, 10), true
	case "body_min":
		return strconv.FormatInt(row.Body.Min, 10), true
	case "body_max":
		return strconv.FormatInt(row.Body.Max, 10), true
	case "body_sum":
		return strconv.FormatInt(row.Body.Sum, 10), true
	case "body_avg":
		return strconv.FormatInt(row.Body.Avg, 10), true
	case "upstream_count":
		return strconv.FormatInt(row.Upstream.Count, 10), true
	case "upstream_sum_ns":
		return strconv.FormatInt(int64(row.Upstream.Sum), 10), true
	case "residual_count":
		return strconv.FormatInt(row.Residual.Count, 10), true
	case "residual_sum_ns":
		return strconv.FormatInt(int64(row.Residual.Sum), 10), true
	case "no_upstream":
		return strconv.FormatInt(row.NoUpstream, 10), true
	}
	if strings.HasPrefix(column, "p") && strings.HasSuffix(column, "_ns") {
		percent, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(column, "p"), "_ns"), 64)
		if err == nil {
			for _, percentile := range row.Request.Percentiles {
				if percentile.Percent == percent {
					return strconv.FormatInt(int64(percentile.Value), 10), true
				}
			}
			return "", true
		}
	}
	return "", false
}

func sortRows(rows []Row, field string, reverse bool) error {
	if field == "" {
		field = "path"
	}
	valid := map[string]bool{"method": true, "path": true, "count": true, "request_min_ns": true, "request_max_ns": true, "request_sum_ns": true, "request_avg_ns": true, "request_stddev_ns": true, "body_sum": true, "status_5xx": true}
	if !valid[field] && (!strings.HasPrefix(field, "p") || !strings.HasSuffix(field, "_ns")) {
		return errors.New("accessinspect render: unsupported-sort")
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := columnValue(rows[i], field)
		b, _ := columnValue(rows[j], field)
		less := a < b
		if field != "method" && field != "path" {
			ai, _ := strconv.ParseInt(a, 10, 64)
			bi, _ := strconv.ParseInt(b, 10, 64)
			less = ai < bi
		}
		if a == b {
			less = rows[i].Method+"\x00"+rows[i].Path < rows[j].Method+"\x00"+rows[j].Path
		}
		if reverse {
			return !less && a != b
		}
		return less
	})
	return nil
}

func neutralizeFormula(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	default:
		return value
	}
}
