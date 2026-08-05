package hoststats

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func TestHostStatsScopeMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		env       testEnv
		selfGroup string
		wantScope string
		wantPath  string
		wantSkip  string
	}{
		{
			name:      "row 1: a valid configured path wins",
			env:       testEnv{EnvCGroupPath: mysqlCGroupRel},
			wantScope: ScopeConfigured,
			wantPath:  mysqlCGroupRel,
		},
		{
			name:     "row 2: a broken configured path skips cgroups entirely",
			env:      testEnv{EnvCGroupPath: "../../etc"},
			wantSkip: cgroupSkipRejectPrefix + rejectDotDot,
		},
		{
			name:     "row 2: a configured path with no limit files is a rejection",
			env:      testEnv{EnvCGroupPath: "nolimits"},
			wantSkip: cgroupSkipRejectPrefix + rejectUnreadable,
		},
		{
			name:      "row 3: host scope only when declared",
			env:       testEnv{EnvCGroupScope: ScopeHost},
			wantScope: ScopeHost,
			wantPath:  agentCGroupRel,
		},
		{
			name:      "row 4: a visible root stays visible-root, never host",
			env:       testEnv{},
			selfGroup: "0::/\n",
			wantScope: ScopeVisibleRoot,
			wantPath:  "",
		},
		{
			name:      "row 5: the agent's own cgroup",
			env:       testEnv{},
			wantScope: ScopeAgentCGroup,
			wantPath:  agentCGroupRel,
		},
		{
			name:      "row 6: cgroup v1 only",
			env:       testEnv{},
			selfGroup: "11:memory:/user.slice\n1:name=systemd:/user.slice\n",
			wantSkip:  cgroupSkipV1,
		},
		{
			name:      "row 6: unreadable self cgroup is treated as no v2",
			env:       testEnv{},
			selfGroup: "\x00missing",
			wantSkip:  cgroupSkipV1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt := testOptions(tt.env, newClock(fixtureTime))
			procFS := newProcFS()
			switch tt.selfGroup {
			case "":
			case "\x00missing":
				delete(procFS, pathSelfCGroup)
			default:
				procFS[pathSelfCGroup] = mapFile(tt.selfGroup)
			}
			opt.ProcFS = procFS

			got := resolveCGroup(opt)
			if got.skip != tt.wantSkip {
				t.Fatalf("skip = %q, want %q", got.skip, tt.wantSkip)
			}
			if tt.wantSkip != "" {
				if got.fs != nil {
					t.Fatal("a skipped target must not carry a filesystem to read")
				}
				return
			}
			if got.scope != tt.wantScope {
				t.Fatalf("scope = %q, want %q", got.scope, tt.wantScope)
			}
			if got.path != tt.wantPath {
				t.Fatalf("path = %q, want %q", got.path, tt.wantPath)
			}
			if got.fs == nil {
				t.Fatal("a resolved target must carry the cgroup filesystem")
			}
		})
	}
}

func TestHostStatsCGroupPathEscape_Rejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rel      string
		wantCode string
	}{
		{name: "escape-absolute", rel: "/etc", wantCode: rejectAbsolute},
		{name: "escape-dotdot", rel: "../../etc", wantCode: rejectDotDot},
		{name: "escape-dotdot-inner", rel: "system.slice/../../etc", wantCode: rejectDotDot},
		{name: "escape-symlink", rel: "escape-symlink", wantCode: rejectEscapesMount},
		{name: "not-found", rel: "no/such/cgroup", wantCode: rejectNotFound},
		{name: "empty-element", rel: "system.slice//x", wantCode: rejectInvalid},
		{name: "ok-relative", rel: mysqlCGroupRel, wantCode: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved, code := validateCGroupPath(cgroupRoot, tt.rel, fakeEvalSymlinks(defaultSymlinks()))
			if code != tt.wantCode {
				t.Fatalf("code = %q, want %q", code, tt.wantCode)
			}
			if tt.wantCode != "" {
				return
			}
			if resolved != tt.rel {
				t.Fatalf("resolved = %q, want %q", resolved, tt.rel)
			}
		})
	}
}

func TestValidateCGroupPathEdgeCases(t *testing.T) {
	t.Parallel()
	if _, code := validateCGroupPath(cgroupRoot, "x", nil); code != rejectEvalFailed {
		t.Fatalf("code = %q, want %q without an EvalSymlinks seam", code, rejectEvalFailed)
	}

	failing := func(string) (string, error) { return "", context.DeadlineExceeded }
	if _, code := validateCGroupPath(cgroupRoot, "x", failing); code != rejectEvalFailed {
		t.Fatalf("code = %q, want %q", code, rejectEvalFailed)
	}

	// A configured path that resolves to the mount root itself is legal: it is
	// inside the mount, which is the only thing the check is about.
	resolved, code := validateCGroupPath(cgroupRoot, "self", fakeEvalSymlinks(map[string]string{
		cgroupRoot + "/self": cgroupRoot,
	}))
	if code != "" || resolved != "" {
		t.Fatalf("validateCGroupPath() = %q, %q; want the mount root accepted", resolved, code)
	}
}

func TestResolveCGroupWithoutMount(t *testing.T) {
	t.Parallel()
	opt := testOptions(testEnv{}, newClock(fixtureTime))
	opt.CGroupFS = nil
	opt.CGroupRoot = ""
	procFS := newProcFS()
	delete(procFS, pathSelfMountinfo)
	opt.ProcFS = procFS

	if got := resolveCGroup(opt); got.skip != cgroupSkipNoMount {
		t.Fatalf("skip = %q, want %q", got.skip, cgroupSkipNoMount)
	}

	configured := testOptions(testEnv{EnvCGroupPath: mysqlCGroupRel}, newClock(fixtureTime))
	configured.CGroupFS = nil
	configured.CGroupRoot = ""
	configured.ProcFS = procFS
	want := cgroupSkipRejectPrefix + rejectNoMount
	if got := resolveCGroup(configured); got.skip != want {
		t.Fatalf("skip = %q, want %q", got.skip, want)
	}
}

func TestFindCGroup2Mount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "standard layout", input: fixtureMountinfo, want: cgroupRoot},
		{
			name:  "optional fields absent",
			input: "31 28 0:26 / /cgroup2 rw - cgroup2 cgroup2 rw\n",
			want:  "/cgroup2",
		},
		{
			name:  "octal escapes are decoded",
			input: `31 28 0:26 / /mnt/cgroup\0402 rw shared:9 - cgroup2 cgroup2 rw` + "\n",
			want:  "/mnt/cgroup 2",
		},
		{
			name:  "cgroup v1 mounts are not cgroup2",
			input: "31 28 0:26 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n",
			want:  "",
		},
		{name: "truncated line", input: "31 28 0:26 / -\n", want: ""},
		{name: "empty file", input: "", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			procFS := fstest.MapFS{pathSelfMountinfo: mapFile(tt.input)}
			if got := findCGroup2Mount(procFS); got != tt.want {
				t.Fatalf("findCGroup2Mount() = %q, want %q", got, tt.want)
			}
		})
	}

	if got := findCGroup2Mount(fstest.MapFS{}); got != "" {
		t.Fatalf("findCGroup2Mount() = %q, want empty when mountinfo is unreadable", got)
	}
}

func TestReadSelfCGroupV2(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{name: "nested path", input: fixtureSelfCGroup, want: "/system.slice/isutools.service", wantOK: true},
		{name: "root", input: "0::/\n", want: "/", wantOK: true},
		{name: "empty value reads as root", input: "0::\n", want: "/", wantOK: true},
		{name: "v1 only", input: "11:memory:/x\n", wantOK: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := readSelfCGroupV2(fstest.MapFS{pathSelfCGroup: mapFile(tt.input)})
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("readSelfCGroupV2() = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseCPUMax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		wantCores *float64
		wantOK    bool
	}{
		{name: "unlimited", input: "max 100000\n", wantOK: true},
		{name: "two cores", input: "200000 100000\n", wantCores: ratePtr(2), wantOK: true},
		{name: "quota without period", input: "50000\n", wantCores: ratePtr(0.5), wantOK: true},
		{name: "zero period", input: "50000 0\n"},
		{name: "non-numeric quota", input: "x 100000\n"},
		{name: "non-numeric period", input: "100000 x\n"},
		{name: "empty", input: "\n"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseCPUMax([]byte(tt.input))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !sameFloat(got, tt.wantCores) {
				t.Fatalf("cores = %v, want %v", got, tt.wantCores)
			}
		})
	}
}

func TestParseMemoryMax(t *testing.T) {
	t.Parallel()
	if got, ok := parseMemoryMax([]byte("max\n")); !ok || got != nil {
		t.Fatalf("parseMemoryMax(max) = %v, %v; want nil, true", got, ok)
	}
	got, ok := parseMemoryMax([]byte("2147483648\n"))
	if !ok || got == nil || *got != 2147483648 {
		t.Fatalf("parseMemoryMax() = %v, %v; want the limit", got, ok)
	}
	if _, ok := parseMemoryMax([]byte("lots\n")); ok {
		t.Fatal("parseMemoryMax() must reject a non-numeric limit")
	}
}

func TestReadCGroupRaw(t *testing.T) {
	t.Parallel()
	target := cgroupTarget{fs: newCGroupFS(), scope: ScopeConfigured, path: mysqlCGroupRel}
	raw, err := readCGroupRaw(target)
	if err != nil {
		t.Fatalf("readCGroupRaw() error = %v, want nil", err)
	}
	if raw.CPUMaxCores == nil || *raw.CPUMaxCores != 4 {
		t.Fatalf("cpu.max = %v, want 4 cores", raw.CPUMaxCores)
	}
	if raw.MemoryMaxBytes == nil || *raw.MemoryMaxBytes != 8589934592 {
		t.Fatalf("memory.max = %v, want the limit", raw.MemoryMaxBytes)
	}
	if raw.MemoryCurrentBytes != 4294967296 {
		t.Fatalf("memory.current = %d, want 4294967296", raw.MemoryCurrentBytes)
	}

	root := cgroupTarget{fs: newCGroupFS(), scope: ScopeVisibleRoot}
	rootRaw, err := readCGroupRaw(root)
	if err != nil {
		t.Fatalf("readCGroupRaw(root) error = %v, want nil", err)
	}
	if rootRaw.CPUMaxCores != nil || rootRaw.MemoryMaxBytes != nil {
		t.Fatalf("root cgroup = %+v, want unlimited rendered as nil", rootRaw)
	}

	if _, err := readCGroupRaw(cgroupTarget{skip: cgroupSkipV1}); err == nil {
		t.Fatal("a skipped target must not produce a reading")
	}
	if _, err := readCGroupRaw(cgroupTarget{fs: fstest.MapFS{}}); err == nil {
		t.Fatal("a cgroup with no readable file must be reported as unreadable")
	}
}

func TestHostStatsCGroupV1_Skipped(t *testing.T) {
	t.Parallel()
	opt := testOptions(testEnv{}, newClock(fixtureTime))
	procFS := newProcFS()
	procFS[pathSelfCGroup] = mapFile("11:memory:/user.slice\n")
	opt.ProcFS = procFS

	c, err := New(opt)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline() = %+v, %v; want a committed sample", res, err)
	}
	sample, ok := res.Handle.Sample().(*Sample)
	if !ok {
		t.Fatalf("handle carries %T, want *Sample", res.Handle.Sample())
	}
	if sample.CGroup != nil {
		t.Fatal("cgroup v1 must leave the cgroup reading empty")
	}
	if sample.CGroupSkip != cgroupSkipV1 {
		t.Fatalf("CGroupSkip = %q, want %q", sample.CGroupSkip, cgroupSkipV1)
	}
	if !containsString(sample.Codes, CodeNotCapturedPrefix+SourceCGroup) {
		t.Fatalf("codes = %v, want the cgroup skip recorded", sample.Codes)
	}
	// Everything else still has to be there: one missing source may not cost
	// the caller the whole host section.
	if sample.Mem.TotalBytes == 0 || len(sample.Disks) == 0 || len(sample.PSI) == 0 {
		t.Fatalf("sample = %+v, want the other sources intact", sample)
	}

	section := collectSection(t, c, sample, sample)
	notes := section.HealthNotes()
	if !hasHealthKey(notes, HealthCGroupV1) {
		t.Fatalf("health notes = %+v, want %s", notes, HealthCGroupV1)
	}
}

func TestHostStatsCGroupPathRejected_Health(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{EnvCGroupPath: "../../etc"}, newClock(fixtureTime))
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline() = %+v, %v; want a committed sample", res, err)
	}
	sample := res.Handle.Sample().(*Sample)
	if sample.CGroup != nil {
		t.Fatal("a rejected path must not fall back to another cgroup")
	}
	section := collectSection(t, c, sample, sample)
	notes := section.HealthNotes()
	if !hasHealthKey(notes, HealthCGroupPathRejected) {
		t.Fatalf("health notes = %+v, want %s", notes, HealthCGroupPathRejected)
	}
	if !strings.Contains(notes[0].Message+notes[len(notes)-1].Message, rejectDotDot) {
		t.Fatalf("health notes = %+v, want the rejection code reported", notes)
	}
}

// collectSection runs Collect over two samples and returns the section.
func collectSection(t *testing.T, c *Collector, base, final *Sample) *Section {
	t.Helper()
	value, err := c.Collect(
		handleOf(base, runctl.PhaseStartBaseline, base.At),
		handleOf(final, runctl.PhaseFinishFinal, final.At),
	)
	if err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	section, ok := value.(*Section)
	if !ok {
		t.Fatalf("Collect() = %T, want *Section", value)
	}
	return section
}

func hasHealthKey(notes []HealthNote, key string) bool {
	for _, note := range notes {
		if note.Key == key {
			return true
		}
	}
	return false
}

func TestReadCGroupRawDegradesPerFile(t *testing.T) {
	t.Parallel()
	cgroupFS := fstest.MapFS{
		fileCPUMax:        mapFile("200000 100000\n"),
		fileMemoryMax:     mapFile("garbage\n"),
		fileMemoryCurrent: mapFile("not a number\n"),
	}
	raw, err := readCGroupRaw(cgroupTarget{fs: cgroupFS, scope: ScopeVisibleRoot})
	if err != nil {
		t.Fatalf("readCGroupRaw() error = %v, want the readable file kept", err)
	}
	if raw.CPUMaxCores == nil || *raw.CPUMaxCores != 2 {
		t.Fatalf("cpu.max = %v, want 2 cores", raw.CPUMaxCores)
	}
	if raw.MemoryMaxBytes != nil || raw.MemoryCurrentBytes != 0 {
		t.Fatalf("cgroup = %+v, want the unparsable files left empty", raw)
	}

	if _, err := readCGroupRaw(cgroupTarget{}); err == nil {
		t.Fatal("a target without a filesystem must not produce a reading")
	}
}

func TestParseUintValueRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parseUintValue([]byte("12x\n")); err == nil {
		t.Fatal("parseUintValue() error = nil, want a parse failure")
	}
}

func TestValidateCGroupPathRejectsUnusableResolution(t *testing.T) {
	t.Parallel()
	eval := fakeEvalSymlinks(map[string]string{cgroupRoot + "/x": cgroupRoot + "/./x"})
	if _, code := validateCGroupPath(cgroupRoot, "x", eval); code != rejectInvalid {
		t.Fatalf("code = %q, want %q for a resolution that is not a clean relative path", code, rejectInvalid)
	}
}

func TestResolveCGroupDefaultsToTheResolvedMount(t *testing.T) {
	t.Parallel()
	opt := testOptions(testEnv{}, newClock(fixtureTime))
	opt.CGroupFS = nil
	opt.CGroupRoot = ""

	got := resolveCGroup(opt)
	if got.skip != "" || got.fs == nil {
		t.Fatalf("target = %+v, want the mount from mountinfo opened", got)
	}
	if got.scope != ScopeAgentCGroup || got.path != agentCGroupRel {
		t.Fatalf("target = %+v, want the agent's own cgroup", got)
	}
}
