//go:build darwin || freebsd || windows
// +build darwin freebsd windows

package runner

import (
	"time"

	"github.com/buildbarn/bb-storage/pkg/clock"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MoveCurrentProcessIntoCgroup moves the current process into a cgroup
// v2 cgroup. On this operating system this functionality is not
// available.
func MoveCurrentProcessIntoCgroup(cgroupPath string) error {
	return status.Error(codes.Unimplemented, "Cgroups are not supported on this platform")
}

// NewCgroupV2ActionCgroupCreator creates an ActionCgroupCreator that
// creates cgroups underneath a given cgroup v2 directory. On this
// operating system this functionality is not available.
func NewCgroupV2ActionCgroupCreator(parentCgroupPath string, killTimeout time.Duration, clock clock.Clock) (ActionCgroupCreator, error) {
	return nil, status.Error(codes.Unimplemented, "Cgroups are not supported on this platform")
}
