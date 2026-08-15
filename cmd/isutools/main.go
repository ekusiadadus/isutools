package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/internal/accessinspect"
	"golang.org/x/sys/unix"
)

var version = "devel"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		writeUsage(stdout)
		return 0
	}
	if len(args) == 1 && args[0] == "version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if len(args) >= 2 && args[0] == "inspect" && args[1] == "accesslog" {
		return runAccesslog(ctx, args[2:], stdin, stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "analyze" && args[1] == "mysql-slowlog" {
		return runMySQLSlowlog(ctx, args[2:], stdin, stdout, stderr)
	}
	{
		writeUsage(stderr)
		return 2
	}
}

func writeUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, "usage: isutools inspect accesslog [--file access.log] [--format auto] [--where expression] [--output table|json|markdown|tsv|csv]")
	_, _ = fmt.Fprintln(output, "       isutools analyze mysql-slowlog [--file mysql-slow.log] [--pt-query-digest] [--data-dir DIR --run-id ID --snapshot-base BASE --snapshot-sha256 SHA]")
}

func runAccesslog(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("inspect accesslog", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var (
		filePath       = flags.String("file", "", "input file; stdin when omitted")
		formatName     = flags.String("format", "auto", "explicit decoder")
		where          = flags.String("where", "", "bounded filter expression")
		pathRulesSpec  = flags.String("path-rules", "", "ordered regexp=normalized-path rules")
		unmatchedName  = flags.String("unmatched", "keep", "keep or collapse")
		percentileSpec = flags.String("percentiles", "50,90,95,99", "comma-separated percentiles")
		outputName     = flags.String("output", "table", "table, json, markdown, tsv, or csv")
		sortName       = flags.String("sort", "path", "sort column")
		reverse        = flags.Bool("reverse", false, "reverse sort order")
		limit          = flags.Int("limit", 0, "maximum output rows; zero means all")
		columnSpec     = flags.String("columns", "", "comma-separated columns")
		maxInput       = flags.Int64("max-input-bytes", accessinspect.DefaultMaxInputBytes, "input byte limit")
		maxLine        = flags.Int("max-line-bytes", accessinspect.DefaultMaxLineBytes, "line byte limit")
		maxRecords     = flags.Int64("max-records", accessinspect.DefaultMaxRecords, "parsed record limit")
		maxKeys        = flags.Int("max-keys", accessinspect.DefaultMaxKeys, "aggregation key limit")
		coverageSet    = flags.Bool("coverage", false, "require explicit run boundary coverage metadata")
		startDevice    = flags.Uint64("start-device", 0, "capture-start log device")
		startInode     = flags.Uint64("start-inode", 0, "capture-start log inode")
		startOffset    = flags.Uint64("start-offset", 0, "capture-start byte offset")
		startClock     = flags.String("start-clock", "", "capture-start proxy clock (RFC3339Nano)")
		endDevice      = flags.Uint64("end-device", 0, "capture-end log device")
		endInode       = flags.Uint64("end-inode", 0, "capture-end log inode")
		endOffset      = flags.Uint64("end-offset", 0, "capture-end byte offset")
		endClock       = flags.String("end-clock", "", "capture-end proxy clock (RFC3339Nano)")
		dataDir        = flags.String("data-dir", "", "artifact and snapshot data directory")
		namespace      = flags.String("namespace", "", "artifact namespace; defaults to run id")
		expected       = flags.String("expected-current", "none", "expected current artifact id")
		runID          = flags.String("run-id", "", "source run id")
		snapshotBase   = flags.String("snapshot-base", "", "source snapshot basename")
		snapshotSHA    = flags.String("snapshot-sha256", "", "source snapshot sha256")
		snapshotSchema = flags.Int("snapshot-schema", 0, "source snapshot schema version")
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stdout, "usage: isutools inspect accesslog [--file access.log] [--format auto] [--where expression] [--output table|json|markdown|tsv|csv]")
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "isutools: invalid-flags")
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "isutools: invalid-flags")
		writeUsage(stderr)
		return 2
	}
	format, err := accesslog.ParseFormat(*formatName)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "isutools: unsupported-format")
		return 2
	}
	filter, err := accessinspect.CompileFilter(*where)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	unmatched := accesslog.UnmatchedPolicy(*unmatchedName)
	pathRules, err := accesslog.ParsePathRules(*pathRulesSpec, unmatched)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	percentiles, err := parsePercentiles(*percentileSpec)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "isutools: invalid-percentiles")
		return 2
	}
	columns, err := parseList(*columnSpec, 64)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "isutools: invalid-columns")
		return 2
	}
	coverage, err := accessLogCoverage(*coverageSet, *startDevice, *startInode, *startOffset, *startClock, *endDevice, *endInode, *endOffset, *endClock)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "accessinspect: invalid-coverage")
		return 2
	}
	input := stdin
	var closer io.Closer
	if *filePath != "" && *filePath != "-" {
		file, openErr := openRegularNoFollow(*filePath)
		if openErr != nil {
			_, _ = fmt.Fprintln(stderr, "isutools: input-not-regular")
			return 1
		}
		input, closer = file, file
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}
	hasher := sha256.New()
	counter := &byteCounter{}
	report, err := accessinspect.Inspect(io.TeeReader(input, io.MultiWriter(hasher, counter)), accessinspect.Options{
		Context: ctx, Format: format, Filter: filter, PathRules: pathRules, Percentiles: percentiles,
		MaxInputBytes: *maxInput, MaxLineBytes: *maxLine, MaxRecords: *maxRecords, MaxKeys: *maxKeys,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	var rendered bytes.Buffer
	err = accessinspect.Render(&rendered, report, accessinspect.RenderOptions{
		Format: accessinspect.OutputFormat(*outputName), Sort: *sortName, Reverse: *reverse, Limit: *limit, Columns: columns,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if *dataDir != "" {
		artifactID, publishErr := publishAccesslogArtifact(*dataDir, *namespace, *expected, *runID, *snapshotBase, *snapshotSHA, *snapshotSchema,
			hasher.Sum(nil), counter.n, rendered.Bytes(), report, coverage, *outputName, *maxInput, *maxRecords, *maxKeys)
		if publishErr != nil {
			_, _ = fmt.Fprintln(stderr, publishErr)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "accessinspect: artifact=%s\n", artifactID)
	} else if *namespace != "" || *runID != "" || *snapshotBase != "" || *snapshotSHA != "" || *snapshotSchema != 0 {
		_, _ = fmt.Fprintln(stderr, "accessinspect: data-dir-required-for-binding")
		return 2
	}
	if _, err := stdout.Write(rendered.Bytes()); err != nil {
		_, _ = fmt.Fprintln(stderr, "accessinspect: output-write-failed")
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "accessinspect: lines=%d parsed=%d selected=%d malformed=%d partial=%d stripped_query=%d\n",
		report.Health.Lines, report.Health.Parsed, report.Health.Filtered, report.Health.Malformed, report.Health.Partial, report.Health.QueryStripped)
	return 0
}

type byteCounter struct{ n uint64 }

func (counter *byteCounter) Write(body []byte) (int, error) {
	counter.n += uint64(len(body))
	return len(body), nil
}

func parsePercentiles(spec string) ([]float64, error) {
	values, err := parseList(spec, 16)
	if err != nil || len(values) == 0 {
		return nil, errors.New("invalid")
	}
	result := make([]float64, 0, len(values))
	for _, value := range values {
		percentile, parseErr := strconv.ParseFloat(value, 64)
		if parseErr != nil || percentile <= 0 || percentile > 100 {
			return nil, errors.New("invalid")
		}
		result = append(result, percentile)
	}
	return result, nil
}

func parseList(spec string, max int) ([]string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	if len(parts) > max {
		return nil, errors.New("too many values")
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || len(part) > 64 || strings.ContainsAny(part, "\r\n\x00") {
			return nil, errors.New("invalid value")
		}
		result = append(result, part)
	}
	return result, nil
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "accesslog-input")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open failed")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("input must be a single-link regular file")
	}
	return file, nil
}
