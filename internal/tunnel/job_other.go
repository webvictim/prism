//go:build !windows

package tunnel

import "os/exec"

// processJob on non-Windows platforms is a no-op. The daemon's
// signal.NotifyContext handler already catches SIGTERM/SIGINT and
// kills the `tsh proxy app` subprocess explicitly before exiting.
// If the daemon is SIGKILL'd, the orphaned tsh keeps running — but
// `tsh proxy app` is a foreground tool that exits if its parent's
// terminal closes; in our setup the daemon was started detached with
// setsid, so the subprocess's parent is the daemon process group,
// which is reaped by init when the daemon dies. So the tsh process
// gets reparented to init (PID 1) and stays alive holding the port.
// For tsh that's a held port. For tbot it is worse: the orphan keeps
// renewing, and two renewers get the bot's join token locked. The
// daemon handles that at startup instead (cmd/prism/tbot_orphans.go),
// which also covers orphans from daemons older than this one, so there
// is still no cgroup/setpgid+process-tree-kill on Unix the way there is
// with Job Objects on Windows.
type processJob struct{}

func newProcessJob() (*processJob, error) { return &processJob{}, nil }

func (j *processJob) assign(*exec.Cmd) error { return nil }

func (j *processJob) close() error { return nil }
