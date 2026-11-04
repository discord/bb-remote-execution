package runner

import (
	"os/exec"
)

// ActionCgroupCreator is used by LocalRunner to create a cgroup for
// every build action that it runs. This makes it possible to reliably
// terminate all processes spawned by a build action once it completes,
// without affecting unrelated processes running as the same user.
type ActionCgroupCreator interface {
	CreateActionCgroup() (ActionCgroup, error)
}

// ActionCgroup is a cgroup that was created by ActionCgroupCreator to
// run a single build action.
type ActionCgroup interface {
	// ConfigureCommand adjusts a command, so that the process it
	// spawns is placed inside the cgroup. This method must be
	// called before the command is started.
	ConfigureCommand(cmd *exec.Cmd)

	// Destroy terminates all processes that are still running inside
	// the cgroup, and removes the cgroup. This method must be called
	// exactly once, after the command has completed or failed to
	// start.
	Destroy() error
}
