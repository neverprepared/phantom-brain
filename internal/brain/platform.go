package brain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// Platform captures the values that identify the host (and possibly
// the container) a brain was born into. These are stamped into the
// manifest at birth and used by the recovery sweep to distinguish
// "crashed brain from a previous boot" (host matches, boot_id differs)
// from "live brain we should not touch" (heartbeat fresh, flock held).
//
// Implemented as an interface so platform-detection tests can inject
// deterministic values instead of probing the real host.
type Platform interface {
	// HostUUID returns a stable per-host identifier. Linux: /etc/machine-id.
	// Darwin: IOPlatformUUID via ioreg. Stable across reboots, not across
	// hardware replacements. Errors if it cannot be determined.
	HostUUID() (string, error)

	// BootID returns an identifier that changes on every boot. Linux:
	// /proc/sys/kernel/random/boot_id. Darwin: hash of kern.boottime
	// sysctl output (no native boot_id, but boottime is monotonic per
	// boot). Used by recovery sweep to detect previous-boot corpses.
	BootID() (string, error)

	// Hostname returns os.Hostname() — exposed via the interface so
	// tests can mock it without monkeypatching os.
	Hostname() (string, error)

	// InContainer reports whether the process appears to be running
	// inside a container. True if /.dockerenv exists OR /proc/1/cgroup
	// names a container runtime. Used to decide whether to mint a
	// container nonce (so two containers on the same host with the same
	// host_uuid have distinct contributor_ids).
	InContainer() bool

	// ProcessAlive reports whether pid is still a live process on this
	// host. Used by the recovery sweep before declaring a brain dead.
	// True for SIGSTOP'd processes (kill(pid, 0) succeeds).
	ProcessAlive(pid int) bool
}

// realPlatform is the production implementation; results are cached
// because HostUUID and BootID never change during the process lifetime
// and ioreg/sysctl exec is not free.
type realPlatform struct {
	once     sync.Once
	hostUUID string
	hostErr  error
	bootID   string
	bootErr  error
}

// NewPlatform returns the production Platform. Cached lazily on first
// call; safe to share across goroutines.
func NewPlatform() Platform { return &realPlatform{} }

func (p *realPlatform) ensure() {
	p.once.Do(func() {
		p.hostUUID, p.hostErr = detectHostUUID()
		p.bootID, p.bootErr = detectBootID()
	})
}

func (p *realPlatform) HostUUID() (string, error) {
	p.ensure()
	return p.hostUUID, p.hostErr
}

func (p *realPlatform) BootID() (string, error) {
	p.ensure()
	return p.bootID, p.bootErr
}

func (p *realPlatform) Hostname() (string, error) {
	return os.Hostname()
}

func (p *realPlatform) InContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// /proc/1/cgroup exists on Linux only. On Darwin the file is absent
	// and the read errors out — that's fine, we're not in a container.
	raw, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(raw)
	for _, needle := range []string{"docker", "containerd", "kubepods", "lxc"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func (p *realPlatform) ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is the standard liveness probe — does not deliver a
	// signal but checks that the process exists and we have permission
	// to signal it. Works for SIGSTOP'd processes (they exist).
	return proc.Signal(syscall.Signal(0)) == nil
}

// detectHostUUID dispatches to OS-specific helpers. Falls back to the
// hostname if the host-uuid sources are unavailable — better than
// failing birth outright, but recovery may misattribute brains across
// hostname collisions.
func detectHostUUID() (string, error) {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/etc/machine-id")
		if err == nil {
			id := strings.TrimSpace(string(raw))
			if id != "" {
				return id, nil
			}
		}
		// Some distros only populate /var/lib/dbus/machine-id.
		raw, err = os.ReadFile("/var/lib/dbus/machine-id")
		if err == nil {
			id := strings.TrimSpace(string(raw))
			if id != "" {
				return id, nil
			}
		}
		return fallbackHostID()
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			if id := extractIOPlatformUUID(string(out)); id != "" {
				return strings.ToLower(id), nil
			}
		}
		return fallbackHostID()
	default:
		return fallbackHostID()
	}
}

// extractIOPlatformUUID parses `ioreg -rd1 -c IOPlatformExpertDevice`
// output and returns the IOPlatformUUID value (the unquoted hex GUID).
// Returns "" if the field is missing.
func extractIOPlatformUUID(ioregOutput string) string {
	const key = "\"IOPlatformUUID\""
	idx := strings.Index(ioregOutput, key)
	if idx < 0 {
		return ""
	}
	rest := ioregOutput[idx+len(key):]
	// Format: `"IOPlatformUUID" = "DEADBEEF-..."`
	eq := strings.Index(rest, "=")
	if eq < 0 {
		return ""
	}
	rest = rest[eq+1:]
	q1 := strings.Index(rest, "\"")
	if q1 < 0 {
		return ""
	}
	q2 := strings.Index(rest[q1+1:], "\"")
	if q2 < 0 {
		return ""
	}
	return rest[q1+1 : q1+1+q2]
}

// detectBootID returns a value that changes on every reboot. Linux has
// a kernel-provided random ID; Darwin doesn't, so we hash the
// kern.boottime sysctl output (the boot timestamp is sufficient for
// our purposes — distinguishing this boot's brains from prior boots').
func detectBootID() (string, error) {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return "", fmt.Errorf("brain: read boot_id: %w", err)
		}
		id := strings.TrimSpace(string(raw))
		if id == "" {
			return "", errors.New("brain: /proc/sys/kernel/random/boot_id was empty")
		}
		return id, nil
	case "darwin":
		// sysctl -n kern.boottime emits `{ sec = ..., usec = ... } ...`.
		// Hash the raw output so we don't have to parse it.
		out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
		if err != nil {
			return "", fmt.Errorf("brain: read kern.boottime: %w", err)
		}
		sum := sha256.Sum256(out)
		return hex.EncodeToString(sum[:16]), nil // 128-bit prefix, looks like a uuid
	default:
		return "", fmt.Errorf("brain: boot_id not implemented for GOOS=%s", runtime.GOOS)
	}
}

// fallbackHostID returns a deterministic identifier derived from
// os.Hostname when /etc/machine-id and ioreg are both unavailable. The
// hash prefix matches the visual shape of a real host_uuid so logs
// don't break.
func fallbackHostID() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("brain: cannot determine host id (no machine-id, no hostname): %w", err)
	}
	sum := sha256.Sum256([]byte("fallback:" + h))
	return hex.EncodeToString(sum[:16]), nil
}

// ContributorID is the per-(profile, vault) identity stamped into a
// brain's manifest. Same brain on the same host in two different vaults
// is two distinct contributors — that's why profile and vault are part
// of the key, not just the host. Container nonce is appended when the
// brain is born inside a container so two containers on the same host
// don't collide.
//
// Format matches v4.4 §6:
//
//	{profile}/{vault}@{host_uuid}[+{container_nonce}]
func ContributorID(profile, vault, hostUUID, containerNonce string) string {
	id := fmt.Sprintf("%s/%s@%s", profile, vault, hostUUID)
	if containerNonce != "" {
		id += "+" + containerNonce
	}
	return id
}
