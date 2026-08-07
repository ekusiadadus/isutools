// Command isutools-trajectory turns adapter-produced NDJSON into a portable
// interactive trajectory report.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekusiadadus/isutools/trajectoryviz"
)

func main() {
	input := flag.String("input", "-", "NDJSON input path, or - for stdin")
	output := flag.String("output", "trajectory.html", "self-contained HTML output path, or - for stdout")
	flag.Parse()
	if err := run(*input, *output); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(input, output string) error {
	in := os.Stdin
	if input != "-" {
		file, err := os.Open(input)
		if err != nil {
			return fmt.Errorf("open input: %w", err)
		}
		defer func() { _ = file.Close() }()
		in = file
	}
	dataset, err := trajectoryviz.ParseNDJSON(in)
	if err != nil {
		return err
	}

	if output == "-" {
		return trajectoryviz.RenderHTML(os.Stdout, dataset)
	}

	dir := filepath.Dir(output)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(output)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := trajectoryviz.RenderHTML(tmp, dataset); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(tmpName, output); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	return nil
}
