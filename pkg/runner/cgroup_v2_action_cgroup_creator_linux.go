//go:build linux
// +build linux

package runner

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/google/uuid"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	cgroupV2MinimumPollInterval = time.Millisecond
	cgroupV2MaximumPollInterval = 100 * time.Millisecond
)

// MoveCurrentProcessIntoCgroup moves the current process into a cgroup
// v2 cgroup, creating the cgroup if it does not exist.
//
// This can be used to move bb_runner out of the root of a delegated
// cgroup hierarchy, so that it does not prevent controllers from being
// enabled for the cgroups of build actions.
func MoveCurrentProcessIntoCgroup(cgroupPath string) error {
	if err := createCgroupV2Directory(cgroupPath); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte(strconv.FormatInt(int64(os.Getpid()), 10)), 0); err != nil {
		return util.StatusWrapfWithCode(err, codes.Internal, "Failed to move process into cgroup %#v", cgroupPath)
	}
	return nil
}

// createCgroupV2Directory creates a directory in a cgroup v2 hierarchy
// if it does not already exist, and checks that it is a cgroup that
// supports the features that bb_runner requires.
func createCgroupV2Directory(cgroupPath string) error {
	if !filepath.IsAbs(cgroupPath) {
		return status.Errorf(codes.InvalidArgument, "Cgroup path %#v is not absolute", cgroupPath)
	}
	if err := os.Mkdir(cgroupPath, 0o755); err != nil && !os.IsExist(err) {
		return util.StatusWrapfWithCode(err, codes.Internal, "Failed to create cgroup %#v", cgroupPath)
	}
	var statfs unix.Statfs_t
	if err := unix.Statfs(cgroupPath, &statfs); err != nil {
		return util.StatusWrapfWithCode(err, codes.Internal, "Failed to obtain file system properties of cgroup %#v", cgroupPath)
	}
	if statfs.Type != unix.CGROUP2_SUPER_MAGIC {
		return status.Errorf(codes.InvalidArgument, "Path %#v is not part of a cgroup v2 hierarchy", cgroupPath)
	}
	if _, err := os.Stat(filepath.Join(cgroupPath, "cgroup.kill")); err != nil {
		return util.StatusWrapfWithCode(err, codes.FailedPrecondition, "Cgroup %#v does not support cgroup.kill, which requires Linux 5.14 or later", cgroupPath)
	}
	return nil
}

type cgroupV2ActionCgroupCreator struct {
	parentCgroupPath string
	killTimeout      time.Duration
	clock            clock.Clock
}

// NewCgroupV2ActionCgroupCreator creates an ActionCgroupCreator that
// creates cgroups underneath a given cgroup v2 directory. Build actions
// are started inside their cgroup directly, using clone3()'s
// CLONE_INTO_CGROUP flag. Once a build action completes, all processes
// in its cgroup are killed by writing to cgroup.kill.
func NewCgroupV2ActionCgroupCreator(parentCgroupPath string, killTimeout time.Duration, clock clock.Clock) (ActionCgroupCreator, error) {
	if err := createCgroupV2Directory(parentCgroupPath); err != nil {
		return nil, err
	}
	return &cgroupV2ActionCgroupCreator{
		parentCgroupPath: parentCgroupPath,
		killTimeout:      killTimeout,
		clock:            clock,
	}, nil
}

func (cc *cgroupV2ActionCgroupCreator) CreateActionCgroup() (ActionCgroup, error) {
	// Use random names, so that cgroups left behind by previous
	// invocations of bb_runner don't cause collisions.
	name, err := uuid.NewRandom()
	if err != nil {
		return nil, util.StatusWrapWithCode(err, codes.Internal, "Failed to generate cgroup name")
	}
	cgroupPath := filepath.Join(cc.parentCgroupPath, name.String())
	if err := os.Mkdir(cgroupPath, 0o755); err != nil {
		return nil, util.StatusWrapfWithCode(err, codes.Internal, "Failed to create cgroup %#v", cgroupPath)
	}
	fd, err := unix.Open(cgroupPath, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Rmdir(cgroupPath)
		return nil, util.StatusWrapfWithCode(err, codes.Internal, "Failed to open cgroup %#v", cgroupPath)
	}
	return &cgroupV2ActionCgroup{
		creator:    cc,
		cgroupPath: cgroupPath,
		fd:         fd,
	}, nil
}

type cgroupV2ActionCgroup struct {
	creator    *cgroupV2ActionCgroupCreator
	cgroupPath string
	fd         int
}

func (c *cgroupV2ActionCgroup) ConfigureCommand(cmd *exec.Cmd) {
	// Make a copy of SysProcAttr, as CommandCreator may hand out
	// the same instance to multiple commands.
	var sysProcAttr syscall.SysProcAttr
	if cmd.SysProcAttr != nil {
		sysProcAttr = *cmd.SysProcAttr
	}
	sysProcAttr.UseCgroupFD = true
	sysProcAttr.CgroupFD = c.fd
	cmd.SysProcAttr = &sysProcAttr
}

func (c *cgroupV2ActionCgroup) Destroy() error {
	unix.Close(c.fd)

	// Kill all processes in the cgroup, including ones in nested
	// cgroups, and wait for them to terminate.
	if err := os.WriteFile(filepath.Join(c.cgroupPath, "cgroup.kill"), []byte("1"), 0); err != nil {
		return util.StatusWrapfWithCode(err, codes.Internal, "Failed to kill processes in cgroup %#v", c.cgroupPath)
	}
	cc := c.creator
	deadline := cc.clock.Now().Add(cc.killTimeout)
	pollInterval := cgroupV2MinimumPollInterval
	for {
		populated, err := c.isPopulated()
		if err != nil {
			return err
		}
		if !populated {
			break
		}
		if !cc.clock.Now().Before(deadline) {
			return status.Errorf(codes.Internal, "Processes in cgroup %#v did not terminate within %s after being killed", c.cgroupPath, cc.killTimeout)
		}
		_, t := cc.clock.NewTimer(pollInterval)
		<-t
		pollInterval = min(2*pollInterval, cgroupV2MaximumPollInterval)
	}

	return removeCgroupV2Recursively(c.cgroupPath)
}

// isPopulated returns whether the cgroup or any of its descendants
// still contain live processes, by inspecting cgroup.events.
func (c *cgroupV2ActionCgroup) isPopulated() (bool, error) {
	events, err := os.ReadFile(filepath.Join(c.cgroupPath, "cgroup.events"))
	if err != nil {
		return false, util.StatusWrapfWithCode(err, codes.Internal, "Failed to read events of cgroup %#v", c.cgroupPath)
	}
	populated, err := parseCgroupV2PopulatedEvent(events)
	if err != nil {
		return false, util.StatusWrapf(err, "Failed to parse events of cgroup %#v", c.cgroupPath)
	}
	return populated, nil
}

// parseCgroupV2PopulatedEvent extracts the "populated" key from the
// contents of a cgroup.events file.
func parseCgroupV2PopulatedEvent(events []byte) (bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(events))
	for scanner.Scan() {
		if value, ok := bytes.CutPrefix(scanner.Bytes(), []byte("populated ")); ok {
			switch string(value) {
			case "0":
				return false, nil
			case "1":
				return true, nil
			default:
				return false, status.Errorf(codes.Internal, "Invalid value for key \"populated\": %#v", string(value))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, util.StatusWrapWithCode(err, codes.Internal, "Failed to scan events")
	}
	return false, status.Error(codes.Internal, "Key \"populated\" not found")
}

// removeCgroupV2Recursively removes a cgroup, including any nested
// cgroups the build action may have created. Interface files (e.g.,
// cgroup.procs) are removed by the kernel automatically. Only
// subdirectories need to be removed explicitly.
func removeCgroupV2Recursively(cgroupPath string) error {
	entries, err := os.ReadDir(cgroupPath)
	if err != nil {
		return util.StatusWrapfWithCode(err, codes.Internal, "Failed to read contents of cgroup %#v", cgroupPath)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeCgroupV2Recursively(filepath.Join(cgroupPath, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := unix.Rmdir(cgroupPath); err != nil {
		return util.StatusWrapfWithCode(err, codes.Internal, "Failed to remove cgroup %#v", cgroupPath)
	}
	return nil
}
