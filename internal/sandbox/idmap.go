package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

// ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING, set to any non-empty value,
// forces the root-only fallback regardless of what this host actually
// has: the test switch G5 uses to exercise that path deterministically
// on a host that already has newuidmap/newgidmap and a subuid/subgid
// range, matching noUsernsEnv's own role for G1.
const forceRootOnlyEnv = "ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING"

// idRange is one subuid/subgid entry: length ids starting at start, as
// /etc/subuid and /etc/subgid name them.
type idRange struct {
	start, length uint32
}

// idMapping is what Open found available for the held namespace's own
// user (and group) mapping: either multi (root plus the user's own
// subuid/subgid range, as rootless Podman maps a container) or, lacking
// newuidmap/newgidmap or a subuid/subgid range, root only. Root-only
// means any sandbox whose image declares a non-root USER cannot have
// its in-sandbox uid proxied to a real host id (the gofer's setfsuid
// calls for that uid resolve to the kernel's overflow id, "nobody", with
// no access to anything this node's own real user owns): such images
// fail with EACCES, not a clean refusal, which is why Open logs this
// once, loudly, instead of only on first failure.
type idMapping struct {
	multi    bool
	uid, gid idRange // only meaningful when multi
}

// detectIDMapping decides which of idMapping's two shapes this host
// supports: multi requires both newuidmap and newgidmap on PATH (the
// uidmap package on Debian/Ubuntu) and a /etc/subuid and /etc/subgid
// entry for the invoking user, exactly what rootless Podman itself
// requires before it maps anything beyond root.
func detectIDMapping() (m idMapping, fallbackReason string) {
	if os.Getenv(forceRootOnlyEnv) != "" {
		return idMapping{}, forceRootOnlyEnv + " is set: simulating a host with no newuidmap/newgidmap or subuid/subgid range"
	}
	if _, err := exec.LookPath("newuidmap"); err != nil {
		return idMapping{}, "no newuidmap (install the uidmap package)"
	}
	if _, err := exec.LookPath("newgidmap"); err != nil {
		return idMapping{}, "no newgidmap (install the uidmap package)"
	}
	uidRange, ok := subIDRange("/etc/subuid")
	if !ok {
		return idMapping{}, "no /etc/subuid range for this user"
	}
	gidRange, ok := subIDRange("/etc/subgid")
	if !ok {
		return idMapping{}, "no /etc/subgid range for this user"
	}
	return idMapping{multi: true, uid: uidRange, gid: gidRange}, ""
}

// subIDRange reads path (/etc/subuid or /etc/subgid: "name-or-uid:start:length"
// per line) for the entry naming either the invoking user's name or its
// numeric id, exactly how newuidmap/newgidmap themselves look it up.
func subIDRange(path string) (idRange, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return idRange{}, false
	}
	uid := strconv.Itoa(os.Getuid())
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) != 3 || (fields[0] != name && fields[0] != uid) {
			continue
		}
		start, err1 := strconv.ParseUint(fields[1], 10, 32)
		length, err2 := strconv.ParseUint(fields[2], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		return idRange{start: uint32(start), length: uint32(length)}, true
	}
	return idRange{}, false
}

// applyIDMapping writes pid's uid_map/gid_map through newuidmap/newgidmap:
// namespace id 0 to this process's own real id (always allowed, the same
// single mapping --map-root-user itself would write), plus namespace ids
// 1..m.length to the real subuid/subgid range, one call each since both
// entries must land in the same write (the kernel accepts a user
// namespace's id map exactly once). pid must still have an empty,
// pending id map (true right after Open's unshare, before anything but
// this function touches it).
func applyIDMapping(pid int, m idMapping) error {
	pidStr := strconv.Itoa(pid)
	uidArgs := []string{pidStr, "0", strconv.Itoa(os.Getuid()), "1",
		"1", strconv.Itoa(int(m.uid.start)), strconv.Itoa(int(m.uid.length))}
	if out, err := exec.Command("newuidmap", uidArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("newuidmap: %w: %s", err, out)
	}
	gidArgs := []string{pidStr, "0", strconv.Itoa(os.Getgid()), "1",
		"1", strconv.Itoa(int(m.gid.start)), strconv.Itoa(int(m.gid.length))}
	if out, err := exec.Command("newgidmap", gidArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("newgidmap: %w: %s", err, out)
	}
	return nil
}
