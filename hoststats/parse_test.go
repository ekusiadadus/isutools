package hoststats

import (
	"errors"
	"runtime"
	"testing"
	"testing/fstest"
)

func TestParseMeminfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    MemRaw
		wantErr bool
	}{
		{
			name:  "kernel format converts kB to bytes",
			input: fixtureMeminfo,
			want: MemRaw{
				TotalBytes:     16316948 * 1024,
				AvailableBytes: 8158474 * 1024,
				CachedBytes:    4194304 * 1024,
				DirtyBytes:     12288 * 1024,
				SwapTotalBytes: 2097152 * 1024,
				SwapFreeBytes:  1572864 * 1024,
			},
		},
		{
			name:  "unitless value is already bytes",
			input: "MemTotal: 4096\n",
			want:  MemRaw{TotalBytes: 4096},
		},
		{
			name:  "unit case is ignored",
			input: "MemTotal: 4 KB\n",
			want:  MemRaw{TotalBytes: 4096},
		},
		{
			name:  "SwapCached does not leak into Cached",
			input: "MemTotal: 10 kB\nSwapCached: 99 kB\n",
			want:  MemRaw{TotalBytes: 10 * 1024},
		},
		{
			name:  "non-numeric optional field is dropped",
			input: "MemTotal: 10 kB\nMemAvailable: notanumber kB\n",
			want:  MemRaw{TotalBytes: 10 * 1024},
		},
		{
			name:  "overflowing value is dropped",
			input: "MemTotal: 10 kB\nCached: 18446744073709551615 kB\n",
			want:  MemRaw{TotalBytes: 10 * 1024},
		},
		{
			name:  "blank and malformed lines are ignored",
			input: "\ngarbage\n:8\nMemTotal: 1 kB\n",
			want:  MemRaw{TotalBytes: 1024},
		},
		{
			name:    "missing MemTotal fails the required source",
			input:   "MemFree: 100 kB\n",
			wantErr: true,
		},
		{
			name:    "non-numeric MemTotal fails the required source",
			input:   "MemTotal: nope kB\n",
			wantErr: true,
		},
		{
			name:    "empty file fails the required source",
			input:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseMeminfo([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMeminfo() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMeminfo() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("parseMeminfo() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseVMStat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{name: "kernel format", input: fixtureVMStat, want: 4200},
		{name: "zero is a value", input: "pgmajfault 0\n", want: 0},
		{name: "missing counter", input: "pgfault 1\n", wantErr: true},
		{name: "non-numeric counter", input: "pgmajfault x\n", wantErr: true},
		{name: "truncated line", input: "pgmajfault\n", wantErr: true},
		{name: "empty file", input: "", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseVMStat([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseVMStat() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVMStat() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("parseVMStat() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHostStatsDiskstats_SectorsAndPartitions(t *testing.T) {
	t.Parallel()
	filter := wholeDeviceFilter(newSysFS())
	if filter == nil {
		t.Fatal("wholeDeviceFilter() = nil, want a predicate when /sys/block exists")
	}

	disks, err := parseDiskstats([]byte(fixtureDiskstats), filter)
	if err != nil {
		t.Fatalf("parseDiskstats() error = %v, want nil", err)
	}
	if len(disks) != 2 {
		t.Fatalf("devices = %v, want sda and nvme0n1 only", disks)
	}
	if _, partition := disks["sda1"]; partition {
		t.Fatal("sda1 is a partition of sda and would double count every byte")
	}
	want := DiskRaw{ReadSectors: 2000, WriteSectors: 4000, IOTicksMS: 1200, WeightedMS: 1400}
	if disks["sda"] != want {
		t.Fatalf("sda = %+v, want %+v", disks["sda"], want)
	}
	if disks["nvme0n1"].ReadSectors != 40 {
		t.Fatalf("nvme0n1 = %+v, want the multi-queue device kept", disks["nvme0n1"])
	}
}

func TestParseDiskstats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		filter  func(string) bool
		want    int
		wantErr bool
	}{
		{name: "no filter keeps partitions", input: fixtureDiskstats, want: 3},
		{
			name:   "short lines are skipped",
			input:  "   8       0 sda 1 2 3\n" + "   8       0 sdb 100 0 2000 500 200 0 4000 900 0 1200 1400\n",
			filter: nil,
			want:   1,
		},
		{
			name:   "non-numeric counters skip only that device",
			input:  "   8 0 sda 1 2 x 4 5 6 7 8 9 10 11\n 8 1 sdb 1 2 3 4 5 6 7 8 9 10 11\n",
			filter: nil,
			want:   1,
		},
		{name: "empty input", input: "", wantErr: true},
		{
			name:    "everything filtered out",
			input:   fixtureDiskstats,
			filter:  func(string) bool { return false },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDiskstats([]byte(tt.input), tt.filter)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDiskstats() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDiskstats() error = %v, want nil", err)
			}
			if len(got) != tt.want {
				t.Fatalf("len(devices) = %d, want %d (%v)", len(got), tt.want, got)
			}
		})
	}
}

func TestWholeDeviceFilterFailsOpen(t *testing.T) {
	t.Parallel()
	if wholeDeviceFilter(nil) != nil {
		t.Fatal("a nil sysfs must not filter every device away")
	}
	if wholeDeviceFilter(fstest.MapFS{}) != nil {
		t.Fatal("a sysfs without /sys/block must not filter every device away")
	}
}

func TestParsePSI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    PSIRaw
		wantErr bool
	}{
		{
			name:  "some only, as cpu reports before 5.13",
			input: fixturePSICPU,
			want:  PSIRaw{SomeAvg10: 0.10, SomeAvg60: 0.20, SomeTotalUS: 1000000},
		},
		{
			name:  "some and full",
			input: fixturePSIMemory,
			want: PSIRaw{
				SomeAvg10: 1, SomeAvg60: 2, SomeTotalUS: 2000000,
				FullAvg10: 0.5, FullAvg60: 0.6, FullTotalUS: 1000000, HasFull: true,
			},
		},
		{
			name:  "unreadable average keeps the total",
			input: "some avg10=x avg60=1.00 total=42\n",
			want:  PSIRaw{SomeAvg60: 1, SomeTotalUS: 42},
		},
		{
			name:    "line without total is unusable",
			input:   "some avg10=1.00 avg60=1.00\n",
			wantErr: true,
		},
		{
			name:    "non-numeric total is unusable",
			input:   "some avg10=1.00 total=x\n",
			wantErr: true,
		},
		{name: "empty file", input: "", wantErr: true},
		{name: "unknown line kind", input: "partial avg10=1.00 total=5\n", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePSI([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePSI() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePSI() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("parsePSI() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadPSI(t *testing.T) {
	t.Parallel()
	all, err := readPSI(newProcFS())
	if err != nil {
		t.Fatalf("readPSI() error = %v, want nil", err)
	}
	if len(all) != 3 {
		t.Fatalf("resources = %v, want cpu, memory and io", all)
	}

	procFS := newProcFS()
	delete(procFS, "pressure/memory")
	delete(procFS, "pressure/io")
	partial, err := readPSI(procFS)
	if err != nil || len(partial) != 1 {
		t.Fatalf("readPSI() = %v, %v; want just cpu", partial, err)
	}

	if _, err := readPSI(fstest.MapFS{}); err == nil {
		t.Fatal("readPSI() on a kernel without PSI must report that nothing was read")
	}
}

func TestReadFilesystems(t *testing.T) {
	t.Parallel()
	sizes := defaultStatfsSizes()

	both, err := readFilesystems(fakeStatfs(sizes), "/var/lib/mysql")
	if err != nil {
		t.Fatalf("readFilesystems() error = %v, want nil", err)
	}
	if len(both) != 2 {
		t.Fatalf("paths = %v, want / and the data directory", both)
	}

	rootOnly, err := readFilesystems(fakeStatfs(sizes), "")
	if err != nil || len(rootOnly) != 1 {
		t.Fatalf("readFilesystems(no data dir) = %v, %v; want just /", rootOnly, err)
	}

	deduped, err := readFilesystems(fakeStatfs(sizes), "/")
	if err != nil || len(deduped) != 1 {
		t.Fatalf("readFilesystems(data dir = /) = %v, %v; want one entry", deduped, err)
	}

	if _, err := readFilesystems(fakeStatfs(nil), "/data"); err == nil {
		t.Fatal("readFilesystems() must report when no path could be read")
	}
	if _, err := readFilesystems(nil, ""); err == nil {
		t.Fatal("readFilesystems() without an implementation must report it")
	}
}

func TestMulSaturate(t *testing.T) {
	t.Parallel()
	if got := mulSaturate(0, 512); got != 0 {
		t.Fatalf("mulSaturate(0, 512) = %d, want 0", got)
	}
	if got := mulSaturate(3, 512); got != 1536 {
		t.Fatalf("mulSaturate(3, 512) = %d, want 1536", got)
	}
	const maxUint64 = ^uint64(0)
	if got := mulSaturate(maxUint64, 512); got != maxUint64 {
		t.Fatalf("mulSaturate() must saturate rather than wrap, got %d", got)
	}
}

func TestParseMeminfoLineEdgeCases(t *testing.T) {
	t.Parallel()
	if _, _, ok := parseMeminfoLine("  : 8"); ok {
		t.Fatal("a line with an empty key must be ignored")
	}
	if _, _, ok := parseMeminfoLine("MemTotal:"); ok {
		t.Fatal("a line with no value must be ignored")
	}
}

func TestParsePSIFieldsIgnoresUnpairedFields(t *testing.T) {
	t.Parallel()
	got, err := parsePSI([]byte("some junk =1 avg10=1.00 total=5\n"))
	if err != nil {
		t.Fatalf("parsePSI() error = %v, want nil", err)
	}
	if got.SomeAvg10 != 1 || got.SomeTotalUS != 5 {
		t.Fatalf("parsePSI() = %+v, want the readable pairs kept", got)
	}
}

func TestReadPSISkipsMalformedFiles(t *testing.T) {
	t.Parallel()
	procFS := newProcFS()
	procFS["pressure/cpu"] = mapFile("garbage\n")
	got, err := readPSI(procFS)
	if err != nil {
		t.Fatalf("readPSI() error = %v, want nil", err)
	}
	if _, present := got["cpu"]; present {
		t.Fatalf("resources = %v, want the malformed one dropped", got)
	}
	if len(got) != 2 {
		t.Fatalf("resources = %v, want the readable ones kept", got)
	}
}

func TestOSStatfsMatchesPlatform(t *testing.T) {
	t.Parallel()
	got, err := osStatfs("/")
	if runtime.GOOS != "linux" {
		if !errors.Is(err, ErrUnsupportedOS) {
			t.Fatalf("osStatfs() error = %v, want ErrUnsupportedOS off Linux", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("osStatfs(/) error = %v, want nil on Linux", err)
	}
	if got.TotalBytes == 0 {
		t.Fatalf("osStatfs(/) = %+v, want a size", got)
	}
}
