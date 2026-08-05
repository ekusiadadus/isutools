package hoststats

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

// cgroupTarget is the resolved answer to "which cgroup may we read, if any".
//
// Reading /sys/fs/cgroup blindly measures the cgroup root, which under systemd
// or in a container is nobody's limit. Worse, the agent and the service under
// measurement usually live in different cgroups, so the naive reading reports
// the agent's memory ceiling as if it were the database's.
type cgroupTarget struct {
	fs    fs.FS
	scope string
	path  string // cgroup2-mount-relative; "" is the mount root
	// skip is empty when the target is readable. Otherwise it explains why
	// there is nothing to read, which is what the health note reports.
	skip string
}

// Skip reasons stored in cgroupTarget.skip and Sample.CGroupSkip.
const (
	cgroupSkipV1           = "v1"
	cgroupSkipNoMount      = "no-mount"
	cgroupSkipRejectPrefix = "path-rejected:"
)

// Rejection codes for ISUTOOLS_CGROUP_PATH.
const (
	rejectAbsolute     = "absolute"
	rejectDotDot       = "dotdot"
	rejectInvalid      = "invalid"
	rejectNotFound     = "not-found"
	rejectEvalFailed   = "eval-failed"
	rejectEscapesMount = "escapes-mount"
	rejectUnreadable   = "unreadable"
	rejectNoMount      = "no-mount"
)

// defaultCPUPeriod is the cpu.max period the kernel uses when a cgroup file
// prints a quota without one.
const defaultCPUPeriod = 100000

// resolveCGroup decides once, at construction, which cgroup this collector
// reads. The decision depends on mounts and configuration rather than on the
// moment of sampling, and deciding it per boundary would allow two boundaries
// of the same run to describe two different cgroups.
//
// It never promotes itself to host scope. Inside a cgroup namespace the
// current cgroup appears as "/" and mountinfo is virtualised to match, so no
// in-process check can distinguish "I am the host" from "I am a container that
// looks like one"; host scope therefore comes only from an operator saying so.
func resolveCGroup(opt Options) cgroupTarget {
	root := strings.TrimSuffix(opt.CGroupRoot, "/")
	if root == "" {
		root = findCGroup2Mount(opt.ProcFS)
	}
	cgroupFS := opt.CGroupFS
	if cgroupFS == nil && root != "" {
		cgroupFS = os.DirFS(root)
	}

	// Row 1 and 2: an explicit path wins, and a broken explicit path skips
	// cgroups entirely. Falling back would answer a question about the
	// database with a number about the agent.
	if rel, configured := configuredCGroupPath(opt.Getenv); configured {
		return resolveConfiguredCGroup(opt, cgroupFS, root, rel)
	}

	own, hasV2 := readSelfCGroupV2(opt.ProcFS)
	if !hasV2 {
		// Row 6: cgroup v1 only. v1 spreads limits over per-controller
		// hierarchies with no single path to read, so it is out of scope.
		return cgroupTarget{skip: cgroupSkipV1}
	}
	if cgroupFS == nil {
		return cgroupTarget{skip: cgroupSkipNoMount}
	}
	relOwn := strings.TrimPrefix(own, "/")
	switch {
	case strings.EqualFold(strings.TrimSpace(opt.Getenv(EnvCGroupScope)), ScopeHost):
		return cgroupTarget{fs: cgroupFS, scope: ScopeHost, path: relOwn} // row 3
	case own == "/":
		return cgroupTarget{fs: cgroupFS, scope: ScopeVisibleRoot} // row 4
	default:
		return cgroupTarget{fs: cgroupFS, scope: ScopeAgentCGroup, path: relOwn} // row 5
	}
}

// resolveConfiguredCGroup validates ISUTOOLS_CGROUP_PATH and reads nothing
// else if it does not hold up. Fail-closed is deliberate: a rejected path that
// silently fell back to the agent's own cgroup would be indistinguishable, in
// the rendered report, from a configuration that worked.
func resolveConfiguredCGroup(opt Options, cgroupFS fs.FS, root, rel string) cgroupTarget {
	if cgroupFS == nil || root == "" {
		return cgroupTarget{skip: cgroupSkipRejectPrefix + rejectNoMount}
	}
	resolved, code := validateCGroupPath(root, rel, opt.EvalSymlinks)
	if code != "" {
		return cgroupTarget{skip: cgroupSkipRejectPrefix + code}
	}
	if !limitsReadable(cgroupFS, resolved) {
		return cgroupTarget{skip: cgroupSkipRejectPrefix + rejectUnreadable}
	}
	return cgroupTarget{fs: cgroupFS, scope: ScopeConfigured, path: resolved}
}

// configuredCGroupPath reads the configured path. Empty and "." mean
// "unconfigured" so that an exported-but-empty variable does not disable
// cgroup reporting.
func configuredCGroupPath(getenv func(string) string) (string, bool) {
	raw := strings.TrimSpace(getenv(EnvCGroupPath))
	if raw == "" || raw == "." {
		return "", false
	}
	return raw, true
}

// validateCGroupPath confines a configured path to the cgroup2 mount and
// returns it mount-relative.
//
// The structural checks reject the shapes that could point outside the mount,
// and the symlink resolution catches the one that cannot be seen from the
// string alone. The resolved path is then used as-is: re-resolving it at read
// time would reopen the window between check and use.
func validateCGroupPath(root, rel string, eval func(string) (string, error)) (string, string) {
	switch {
	case strings.HasPrefix(rel, "/"):
		return "", rejectAbsolute
	case hasDotDot(rel):
		return "", rejectDotDot
	case !fs.ValidPath(rel):
		return "", rejectInvalid
	}
	if eval == nil {
		return "", rejectEvalFailed
	}
	resolved, err := eval(root + "/" + rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", rejectNotFound
		}
		return "", rejectEvalFailed
	}
	resolved = strings.TrimSuffix(resolved, "/")
	if resolved != root && !strings.HasPrefix(resolved, root+"/") {
		return "", rejectEscapesMount
	}
	out := strings.TrimPrefix(strings.TrimPrefix(resolved, root), "/")
	if out != "" && !fs.ValidPath(out) {
		return "", rejectInvalid
	}
	return out, ""
}

// hasDotDot reports whether any path element is "..". A textual check is
// enough because it runs before any resolution: the point is to reject the
// shape, not to compute where it would land.
func hasDotDot(rel string) bool {
	for _, element := range strings.Split(rel, "/") {
		if element == ".." {
			return true
		}
	}
	return false
}

// limitsReadable reports whether the target actually exposes cgroup v2 limit
// files. A path that resolves but exposes nothing is treated as a rejected
// configuration rather than as an empty cgroup.
func limitsReadable(cgroupFS fs.FS, rel string) bool {
	for _, name := range []string{fileCPUMax, fileMemoryMax} {
		if _, err := readFile(cgroupFS, path.Join(rel, name)); err == nil {
			return true
		}
	}
	return false
}

// readSelfCGroupV2 returns this process's cgroup v2 path from
// /proc/self/cgroup, which is the "0::<path>" line. Its absence means the host
// runs cgroup v1 only, or that procfs is not readable — both leave us with no
// path to read.
func readSelfCGroupV2(procFS fs.FS) (string, bool) {
	data, err := readFile(procFS, pathSelfCGroup)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		value := strings.TrimPrefix(line, "0::")
		if value == "" {
			value = "/"
		}
		return value, true
	}
	return "", false
}

// findCGroup2Mount finds where cgroup2 is mounted, from /proc/self/mountinfo.
// The location is not fixed: /sys/fs/cgroup on most systems, but containers
// and hybrid setups move it, and reading the wrong tree is worse than reading
// none.
func findCGroup2Mount(procFS fs.FS) string {
	data, err := readFile(procFS, pathSelfMountinfo)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		// Optional fields sit between the mount options and a "-" separator,
		// so the filesystem type can only be found relative to that marker.
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 5 || separator+1 >= len(fields) {
			continue
		}
		if fields[separator+1] != "cgroup2" {
			continue
		}
		return strings.TrimSuffix(unescapeMountPath(fields[4]), "/")
	}
	return ""
}

// unescapeMountPath decodes the octal escapes mountinfo uses for spaces, tabs,
// newlines and backslashes.
func unescapeMountPath(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if decoded, err := strconv.ParseUint(value[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(decoded))
				i += 3
				continue
			}
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

// readCGroupRaw reads one boundary's cgroup values. It returns an error only
// when nothing at all could be read, so a kernel without one of the files
// still contributes the others.
func readCGroupRaw(target cgroupTarget) (*CGroupRaw, error) {
	if target.skip != "" || target.fs == nil {
		return nil, fmt.Errorf("cgroup: skipped (%s)", cgroupSkipReason(target))
	}
	raw := &CGroupRaw{Scope: target.scope, Path: target.path}
	read := 0
	if data, err := readFile(target.fs, path.Join(target.path, fileCPUMax)); err == nil {
		if cores, ok := parseCPUMax(data); ok {
			raw.CPUMaxCores = cores
			read++
		}
	}
	if data, err := readFile(target.fs, path.Join(target.path, fileMemoryMax)); err == nil {
		if limit, ok := parseMemoryMax(data); ok {
			raw.MemoryMaxBytes = limit
			read++
		}
	}
	if data, err := readFile(target.fs, path.Join(target.path, fileMemoryCurrent)); err == nil {
		if current, err := parseUintValue(data); err == nil {
			raw.MemoryCurrentBytes = current
			read++
		}
	}
	if read == 0 {
		return nil, errors.New("cgroup: no file readable")
	}
	return raw, nil
}

// cgroupSkipReason renders a target's skip reason for error messages.
func cgroupSkipReason(target cgroupTarget) string {
	if target.skip != "" {
		return target.skip
	}
	return cgroupSkipNoMount
}

// parseCPUMax converts "200000 100000" into 2 cores. A nil result with ok
// means the quota is "max": unlimited, which is a fact, not a failure.
func parseCPUMax(data []byte) (*float64, bool) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil, false
	}
	if fields[0] == "max" {
		return nil, true
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil, false
	}
	period := float64(defaultCPUPeriod)
	if len(fields) > 1 {
		parsed, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, false
		}
		period = parsed
	}
	if period <= 0 {
		return nil, false
	}
	cores := quota / period
	return &cores, true
}

// parseMemoryMax converts memory.max. A nil result with ok means "max".
func parseMemoryMax(data []byte) (*uint64, bool) {
	value := strings.TrimSpace(string(data))
	if value == "max" {
		return nil, true
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

// parseUintValue reads a cgroup file holding a single integer.
func parseUintValue(data []byte) (uint64, error) {
	value := strings.TrimSpace(string(data))
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", value, err)
	}
	return parsed, nil
}
