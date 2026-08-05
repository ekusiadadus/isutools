package hoststats

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// /proc/diskstats field offsets, counting from the start of the line. The
// kernel's own numbering starts after the device name, so field 3 (sectors
// read) is the sixth column overall.
const (
	diskFieldName         = 2
	diskFieldReadSectors  = 5
	diskFieldWriteSectors = 9
	diskFieldIOTicks      = 12
	diskFieldWeighted     = 13
	diskMinFields         = 14
)

// parseDiskstats reads /proc/diskstats, keeping only whole devices.
//
// isWholeDevice decides what a "device" is. Partitions appear in diskstats
// exactly like their parent and would double-count every byte, so they are
// filtered out by asking sysfs which names exist directly under /sys/block —
// name-shape heuristics break on nvme0n1 (a device) versus nvme0n1p1 (a
// partition) and on device-mapper names.
//
// A malformed or truncated line is skipped rather than failing the read: one
// odd device must not cost the caller every other device.
func parseDiskstats(data []byte, isWholeDevice func(string) bool) (map[string]DiskRaw, error) {
	disks := make(map[string]DiskRaw)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < diskMinFields {
			continue
		}
		name := fields[diskFieldName]
		if name == "" || (isWholeDevice != nil && !isWholeDevice(name)) {
			continue
		}
		raw, ok := parseDiskFields(fields)
		if !ok {
			continue
		}
		disks[name] = raw
	}
	if len(disks) == 0 {
		return nil, errors.New("diskstats: no whole devices found")
	}
	return disks, nil
}

// parseDiskFields pulls the four counters we use out of one line.
func parseDiskFields(fields []string) (DiskRaw, bool) {
	values := make([]uint64, 0, 4)
	for _, index := range []int{diskFieldReadSectors, diskFieldWriteSectors, diskFieldIOTicks, diskFieldWeighted} {
		value, err := strconv.ParseUint(fields[index], 10, 64)
		if err != nil {
			return DiskRaw{}, false
		}
		values = append(values, value)
	}
	return DiskRaw{
		ReadSectors:  values[0],
		WriteSectors: values[1],
		IOTicksMS:    values[2],
		WeightedMS:   values[3],
	}, true
}

// wholeDeviceFilter returns the predicate parseDiskstats uses.
//
// When /sys/block itself cannot be read — a container without sysfs, an
// injected filesystem without it — the filter admits every device. Reporting
// partitions alongside their parent is a smaller loss than reporting no disks
// at all, and the alternative silently empties the section.
func wholeDeviceFilter(sysFS fs.FS) func(string) bool {
	if sysFS == nil {
		return nil
	}
	if _, err := fs.Stat(sysFS, pathSysBlock); err != nil {
		return nil
	}
	return func(name string) bool {
		_, err := fs.Stat(sysFS, path.Join(pathSysBlock, name))
		return err == nil
	}
}
