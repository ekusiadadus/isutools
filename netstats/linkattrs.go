package netstats

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"syscall"
)

// Accepted ranges for the two link attributes. They are deliberately separate:
// the attributes share a directory but not their notion of a sane value, and a
// single shared range would pin neither boundary.
const (
	// minSpeedMbit rejects zero and negative speeds other than the kernel's
	// documented unknown marker.
	minSpeedMbit = 1
	// maxSpeedMbit is 1 Tbit/s. Anything above it is a corrupt read rather
	// than a link we could saturate.
	maxSpeedMbit = 1000000
	// speedUnknown is what the kernel reports when ethtool cannot supply a
	// speed. It is a normal answer for veth, bridges and down links, not a
	// fault.
	speedUnknown = -1

	// minMTU is ETH_MIN_MTU: RFC 791's minimum IPv4 reassembly buffer. No
	// device in practice goes below it, so a smaller value is a corrupt read.
	minMTU = 68
	// maxMTU is the largest value Linux actually takes (loopback). Above it
	// the value also exceeds the IP total-length field, so it cannot be real.
	// Note that 9000 (jumbo frames) sits inside the range and is accepted
	// verbatim: this package displays MTU, it does not judge it.
	maxMTU = 65536
)

// linkAttrs holds the sysfs attributes of one interface. Zero means "not
// accepted" for both fields; neither attribute can legitimately be zero, so no
// separate presence flag is needed.
type linkAttrs struct {
	SpeedMbit int64
	MTU       int64
	// Notes carries per-attribute rejection details for HealthSysfsUnreadable.
	Notes []string
}

// readLinkAttrs reads /sys/class/net/<if>/{speed,mtu}.
//
// It returns an error only when sysfs was never injected, which is a wiring
// fault worth reporting once. Every per-attribute failure is a Note instead:
// measurement is fail-open, and an unreadable speed must not cost the reader
// the interface's throughput.
func readLinkAttrs(sysFS fs.FS, ifname string) (linkAttrs, error) {
	if sysFS == nil {
		return linkAttrs{}, errors.New("netstats: sysfs is not injected")
	}
	var out linkAttrs
	speed, note := readLinkAttr(sysFS, ifname, "speed", classifySpeed)
	out.SpeedMbit = speed
	if note != "" {
		out.Notes = append(out.Notes, note)
	}
	mtu, note := readLinkAttr(sysFS, ifname, "mtu", classifyMTU)
	out.MTU = mtu
	if note != "" {
		out.Notes = append(out.Notes, note)
	}
	return out, nil
}

// readLinkAttr reads one attribute and applies its classifier. The returned
// note is empty when there is nothing worth reporting.
func readLinkAttr(sysFS fs.FS, ifname, attr string, classify func(int64) (int64, bool)) (int64, string) {
	data, err := fs.ReadFile(sysFS, path.Join("class", "net", ifname, attr))
	if err != nil {
		// An absent attribute is routine (virtual NIC, namespace, old kernel)
		// and EINVAL is how the kernel answers for a link that is down, so
		// neither is reported. Anything else — EACCES, a broken mount — is.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EINVAL) || errors.Is(err, fs.ErrInvalid) {
			return 0, ""
		}
		return 0, fmt.Sprintf("%s:%s=%s", ifname, attr, truncateErr(err.Error()))
	}
	raw := strings.TrimSpace(string(data))
	value, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil {
		return 0, fmt.Sprintf("%s:%s=%s", ifname, attr, truncateRaw(raw))
	}
	accepted, quiet := classify(value)
	if accepted == 0 && !quiet {
		return 0, fmt.Sprintf("%s:%s=%s", ifname, attr, truncateRaw(raw))
	}
	return accepted, ""
}

// classifySpeed returns the accepted speed and whether a rejection should stay
// quiet. -1 is quiet because it is the kernel's documented answer for "no
// ethtool speed available"; a host full of veth and docker0 devices would
// otherwise report a health note on every single run, and a note that always
// fires carries no information.
func classifySpeed(value int64) (int64, bool) {
	if value == speedUnknown {
		return 0, true
	}
	if value >= minSpeedMbit && value <= maxSpeedMbit {
		return value, true
	}
	return 0, false
}

// classifyMTU returns the accepted MTU. There is no quiet rejection here: MTU
// has no defined "unknown" value, so anything outside the range — including
// -1 — is a corrupt read and is reported.
func classifyMTU(value int64) (int64, bool) {
	if value >= minMTU && value <= maxMTU {
		return value, true
	}
	return 0, false
}
