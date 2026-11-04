//go:build linux
// +build linux

package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runner_pb "github.com/buildbarn/bb-remote-execution/pkg/proto/runner"
	"github.com/buildbarn/bb-remote-execution/pkg/runner"
	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

// getTestParentCgroupPath creates a cgroup underneath the cgroup of the
// current process that may be used by the test. The test is skipped if
// the current process is not permitted to create cgroups.
func getTestParentCgroupPath(t *testing.T) string {
	cgroups, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("Cannot determine cgroup of current process: %s", err)
	}
	ownCgroup, ok := strings.CutPrefix(strings.TrimSpace(string(cgroups)), "0::")
	if !ok {
		t.Skip("Current process is not part of a cgroup v2 hierarchy")
	}
	parentCgroupPath := filepath.Join("/sys/fs/cgroup", ownCgroup, "bb_runner_test_"+strconv.FormatInt(int64(os.Getpid()), 10))
	if err := os.Mkdir(parentCgroupPath, 0o755); err != nil {
		t.Skipf("Cannot create cgroup: %s", err)
	}
	t.Cleanup(func() {
		require.NoError(t, syscall.Rmdir(parentCgroupPath))
	})
	return parentCgroupPath
}

// requireProcessTerminated requires that a process is no longer
// running. The process may still exist as a zombie, as it may have
// been reparented to a process that does not reap its children.
func requireProcessTerminated(t *testing.T, pid string) {
	stat, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	require.Equal(t, "Z", fields[0], "Process %s is still running", pid)
}

func TestCgroupV2ActionCgroupCreator(t *testing.T) {
	parentCgroupPath := getTestParentCgroupPath(t)
	actionCgroupCreator, err := runner.NewCgroupV2ActionCgroupCreator(parentCgroupPath, 10*time.Second, clock.SystemClock)
	require.NoError(t, err)

	buildDirectoryPath := t.TempDir()
	buildDirectory, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(buildDirectoryPath))
	require.NoError(t, err)
	defer buildDirectory.Close()
	buildDirectoryPathBuilder, scopeWalker := path.EmptyBuilder.Join(path.VoidScopeWalker)
	require.NoError(t, path.Resolve(path.LocalFormat.NewParser(buildDirectoryPath), scopeWalker))

	runner := runner.NewLocalRunner(buildDirectory, buildDirectoryPathBuilder, runner.NewPlainCommandCreator(&syscall.SysProcAttr{}), false, actionCgroupCreator)

	t.Run("BackgroundProcess", func(t *testing.T) {
		testPath := filepath.Join(buildDirectoryPath, "BackgroundProcess")
		require.NoError(t, os.Mkdir(testPath, 0o777))
		require.NoError(t, os.Mkdir(filepath.Join(testPath, "root"), 0o777))

		// Processes that are left behind by the action should
		// be killed once the action completes. The action
		// should run in a cgroup of its own, which is removed
		// afterwards.
		response, err := runner.Run(context.Background(), &runner_pb.RunRequest{
			Arguments:          []string{"/bin/sh", "-c", "sleep 1000 & echo $! > pid; cat /proc/self/cgroup > cgroup"},
			StdoutPath:         "BackgroundProcess/stdout",
			StderrPath:         "BackgroundProcess/stderr",
			InputRootDirectory: "BackgroundProcess/root",
			WorkingDirectory:   "..",
		})
		require.NoError(t, err)
		require.Equal(t, int64(0), response.ExitCode)

		pid, err := os.ReadFile(filepath.Join(testPath, "pid"))
		require.NoError(t, err)
		requireProcessTerminated(t, strings.TrimSpace(string(pid)))

		cgroup, err := os.ReadFile(filepath.Join(testPath, "cgroup"))
		require.NoError(t, err)
		ownCgroup := strings.TrimPrefix(parentCgroupPath, "/sys/fs/cgroup")
		require.Regexp(t, "^0::"+ownCgroup+"/[0-9a-f-]{36}\n$", string(cgroup))

		entries, err := os.ReadDir(parentCgroupPath)
		require.NoError(t, err)
		for _, entry := range entries {
			require.False(t, entry.IsDir(), "Cgroup %#v was not removed", entry.Name())
		}
	})

	t.Run("DaemonInheritingOutputs", func(t *testing.T) {
		testPath := filepath.Join(buildDirectoryPath, "DaemonInheritingOutputs")
		require.NoError(t, os.Mkdir(testPath, 0o777))
		require.NoError(t, os.Mkdir(filepath.Join(testPath, "root"), 0o777))

		// A daemon that inherits the action's stdout and stderr
		// must not prevent Run() from returning. This requires
		// that the log files are passed to the action directly,
		// as opposed to through a pipe that os/exec drains.
		start := time.Now()
		response, err := runner.Run(context.Background(), &runner_pb.RunRequest{
			Arguments:          []string{"/bin/sh", "-c", "sleep 1000 & echo $! > pid; echo hi; echo bye >&2"},
			StdoutPath:         "DaemonInheritingOutputs/stdout",
			StderrPath:         "DaemonInheritingOutputs/stderr",
			InputRootDirectory: "DaemonInheritingOutputs/root",
			WorkingDirectory:   "..",
		})
		require.NoError(t, err)
		require.Equal(t, int64(0), response.ExitCode)
		require.Less(t, time.Since(start), 5*time.Second)

		stdout, err := os.ReadFile(filepath.Join(testPath, "stdout"))
		require.NoError(t, err)
		require.Equal(t, "hi\n", string(stdout))
		stderr, err := os.ReadFile(filepath.Join(testPath, "stderr"))
		require.NoError(t, err)
		require.Equal(t, "bye\n", string(stderr))

		pid, err := os.ReadFile(filepath.Join(testPath, "pid"))
		require.NoError(t, err)
		requireProcessTerminated(t, strings.TrimSpace(string(pid)))
	})

	t.Run("Cancellation", func(t *testing.T) {
		testPath := filepath.Join(buildDirectoryPath, "Cancellation")
		require.NoError(t, os.Mkdir(testPath, 0o777))
		require.NoError(t, os.Mkdir(filepath.Join(testPath, "root"), 0o777))

		// When an action times out, only its main process is
		// killed by exec.CommandContext(). Its children should
		// be killed through the cgroup.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		response, err := runner.Run(ctx, &runner_pb.RunRequest{
			Arguments:          []string{"/bin/sh", "-c", "sleep 1000 & echo $! > pid; wait"},
			StdoutPath:         "Cancellation/stdout",
			StderrPath:         "Cancellation/stderr",
			InputRootDirectory: "Cancellation/root",
			WorkingDirectory:   "..",
		})
		require.NoError(t, err)
		require.Equal(t, int64(-1), response.ExitCode)

		pid, err := os.ReadFile(filepath.Join(testPath, "pid"))
		require.NoError(t, err)
		requireProcessTerminated(t, strings.TrimSpace(string(pid)))
	})

	t.Run("NestedCgroup", func(t *testing.T) {
		testPath := filepath.Join(buildDirectoryPath, "NestedCgroup")
		require.NoError(t, os.Mkdir(testPath, 0o777))
		require.NoError(t, os.Mkdir(filepath.Join(testPath, "root"), 0o777))

		// Actions that are permitted to create cgroups of
		// their own should not cause cleanup to fail.
		response, err := runner.Run(context.Background(), &runner_pb.RunRequest{
			Arguments:          []string{"/bin/sh", "-c", "d=/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/nested && mkdir $d && sh -c 'echo $$ > '$d'/cgroup.procs; exec sleep 1000' & sleep 0.1"},
			StdoutPath:         "NestedCgroup/stdout",
			StderrPath:         "NestedCgroup/stderr",
			InputRootDirectory: "NestedCgroup/root",
		})
		require.NoError(t, err)
		require.Equal(t, int64(0), response.ExitCode)

		entries, err := os.ReadDir(parentCgroupPath)
		require.NoError(t, err)
		for _, entry := range entries {
			require.False(t, entry.IsDir(), "Cgroup %#v was not removed", entry.Name())
		}
	})
}
