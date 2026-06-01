package tunnel

import (
	"context"
	"fmt"
	"os/exec"
)

// tshRuntime is the default Runtime: spawns `tsh proxy app <name> --port <localport>`
// with no pre-start work. Identity is the user's interactive tsh login,
// which the supervisor's identity watcher tracks so the subprocess can
// be kicked when the user re-logs-in after expiry.
type tshRuntime struct{}

// newTshRuntime returns the default Runtime used when Config.Runtime is nil.
func newTshRuntime() Runtime { return &tshRuntime{} }

func (*tshRuntime) Prepare(context.Context, AppInfo) error { return nil }

func (*tshRuntime) Command(ctx context.Context, info AppInfo) *exec.Cmd {
	return exec.CommandContext(ctx, "tsh", "proxy", "app", info.AppName, "--port", fmt.Sprint(info.LocalPort))
}

func (*tshRuntime) Name() string { return "tsh" }

func (*tshRuntime) WatchTshIdentity() bool { return true }
