package server

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// containerNameTTL is how long the PID-to-container mapping is kept before
// re-querying docker. Containers starting and stopping is rare compared to
// the performance tab's polling interval, so a short TTL keeps the mapping
// fresh without spawning docker on every request.
const containerNameTTL = 30 * time.Second

// maxContainerNameDepth caps the parent-PID walk so a malformed process tree
// cannot loop forever.
const maxContainerNameDepth = 64

// dockerContainerRoots returns the root PIDs of the running containers
// mapped to their names. It is a variable so tests can substitute a fake.
var dockerContainerRoots = defaultDockerContainerRoots

// containerNameResolver maps host PIDs to docker container names. Every
// container process on the host is a descendant of the container's root PID
// (the host PID docker reports for its init process), so a PID is matched by
// walking the host parent chain up to a container root PID. The root list is
// re-resolved from docker on a TTL.
type containerNameResolver struct {
	mu      sync.Mutex
	roots   map[int]string
	fetched time.Time
}

// nameFor returns the container name for a host PID, or "" when the PID does
// not belong to a running container (or docker is unavailable). parentCache
// is a per-request cache of host parent PIDs, shared between lookups.
func (r *containerNameResolver) nameFor(pid int, parentCache map[int]int) string {
	if pid <= 0 {
		return ""
	}

	r.mu.Lock()
	if time.Since(r.fetched) > containerNameTTL {
		r.roots = dockerContainerRoots()
		r.fetched = time.Now()
	}
	roots := r.roots
	r.mu.Unlock()

	if len(roots) == 0 {
		return ""
	}

	current := pid
	for depth := 0; current > 0 && depth < maxContainerNameDepth; depth++ {
		if name, ok := roots[current]; ok {
			return name
		}

		ppid, ok := parentCache[current]
		if !ok {
			var err error
			ppid, err = processParentPID(current)
			if err != nil {
				return ""
			}
			parentCache[current] = ppid
		}
		if ppid <= 0 || ppid == current {
			return ""
		}
		current = ppid
	}
	return ""
}

// defaultDockerContainerRoots lists the running containers and maps each
// container's root PID to its name. It returns nil when the docker CLI is
// unavailable or the daemon cannot be reached, which callers treat as "no
// containers".
func defaultDockerContainerRoots() map[int]string {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}

	ids, err := dockerCLIOutput("ps", "-q")
	if err != nil {
		return nil
	}
	containerIDs := strings.Fields(ids)
	if len(containerIDs) == 0 {
		return nil
	}

	format := "{{.State.Pid}} {{.Name}}"
	out, err := dockerCLIOutput(append([]string{"inspect", "--format", format}, containerIDs...)...)
	if err != nil {
		return nil
	}
	return parseDockerContainerRoots(out)
}

// parseDockerContainerRoots parses `docker inspect --format` output of
// "<root PID> <container name>" lines. The name carries a leading "/" that is
// stripped. Lines with a non-positive PID (e.g. stopped containers) are
// ignored.
func parseDockerContainerRoots(output string) map[int]string {
	roots := make(map[int]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidStr, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			continue
		}
		name = strings.TrimPrefix(name, "/")
		if name != "" {
			roots[pid] = name
		}
	}
	return roots
}

// dockerCLIOutput runs the docker CLI with the given args and returns its
// stdout.
func dockerCLIOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
