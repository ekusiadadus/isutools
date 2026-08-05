package hoststats

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
)

// idHashLen is how much of the hash is kept. 16 hex characters is 64 bits of
// identity: far more than enough to tell a handful of benchmark hosts apart,
// and short enough to read in a table.
const idHashLen = 16

// readIdentity records who this agent is and what it can see. Every field
// degrades to an empty string on its own, because identity is diagnostic
// context: it must never be the reason a host section fails to exist.
func (c *Collector) readIdentity() Identity {
	id := Identity{
		MachineIDHash: hashIDFile(c.opt.EtcFS, pathMachineID),
		BootIDHash:    hashIDFile(c.opt.ProcFS, pathBootID),
		PIDNS:         c.namespace("pid"),
		NetNS:         c.namespace("net"),
		MntNS:         c.namespace("mnt"),
		CgroupNS:      c.namespace("cgroup"),
		Role:          strings.TrimSpace(c.opt.Getenv(EnvRole)),
		AgentVersion:  c.agentVersion,
	}
	if name, err := c.opt.Hostname(); err == nil {
		id.Hostname = name
	}
	return id
}

// namespace reads one namespace id from /proc/self/ns. The value is the link
// target verbatim ("pid:[4026531836]"): it is compared, never interpreted, and
// two agents reporting the same id are provably in the same namespace.
func (c *Collector) namespace(name string) string {
	if c.opt.Readlink == nil {
		return ""
	}
	value, err := c.opt.Readlink(nsDir + name)
	if err != nil {
		return ""
	}
	return value
}

// hashIDFile hashes a machine-identifying file. The raw machine-id and boot_id
// are host secrets in the weak sense that they identify a machine to anyone
// who reads a shared snapshot, while a truncated hash is enough for the only
// thing we do with them: telling two hosts, or two boots, apart.
func hashIDFile(fsys fs.FS, name string) string {
	data, err := readFile(fsys, name)
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:idHashLen]
}
