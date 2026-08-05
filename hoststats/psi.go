package hoststats

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// psiResources are the three pressure files the kernel exposes.
var psiResources = []string{"cpu", "memory", "io"}

// readPSI reads /proc/pressure/{cpu,memory,io}.
//
// PSI is absent on kernels before 4.20 and on kernels built without
// CONFIG_PSI, and it is the single most useful signal here when present, so
// "absent" has to be a clean skip rather than an error: it returns an error
// only when not one resource could be read, which is the caller's cue to
// record "not-captured:psi".
func readPSI(procFS fs.FS) (map[string]PSIRaw, error) {
	out := make(map[string]PSIRaw, len(psiResources))
	for _, name := range psiResources {
		data, err := readFile(procFS, path.Join(pathPressureDir, name))
		if err != nil {
			continue
		}
		raw, err := parsePSI(data)
		if err != nil {
			continue
		}
		out[name] = raw
	}
	if len(out) == 0 {
		return nil, errors.New("pressure: no resource readable")
	}
	return out, nil
}

// parsePSI reads one pressure file:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=1234
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// avg300 is deliberately dropped: a five minute window says nothing about a
// benchmark run that lasts one.
func parsePSI(data []byte) (PSIRaw, error) {
	var psi PSIRaw
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		avg10, avg60, total, ok := parsePSIFields(fields[1:])
		if !ok {
			continue
		}
		if kind == "some" {
			psi.SomeAvg10, psi.SomeAvg60, psi.SomeTotalUS = avg10, avg60, total
		} else {
			psi.FullAvg10, psi.FullAvg60, psi.FullTotalUS = avg10, avg60, total
			psi.HasFull = true
		}
		seen = true
	}
	if !seen {
		return PSIRaw{}, errors.New("pressure: no some or full line")
	}
	return psi, nil
}

// parsePSIFields reads the key=value pairs of one pressure line.
func parsePSIFields(fields []string) (avg10, avg60 float64, total uint64, ok bool) {
	for _, field := range fields {
		eq := strings.IndexByte(field, '=')
		if eq <= 0 {
			continue
		}
		key, value := field[:eq], field[eq+1:]
		switch key {
		case "avg10":
			avg10 = parseFloatOrZero(value)
		case "avg60":
			avg60 = parseFloatOrZero(value)
		case "total":
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return 0, 0, 0, false
			}
			total = parsed
			ok = true
		}
	}
	return avg10, avg60, total, ok
}

// parseFloatOrZero treats an unreadable average as zero. The stall total is
// the number decisions get made on; a garbled average must not discard it.
func parseFloatOrZero(value string) float64 {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}
