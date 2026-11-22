package beam

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/webvictim/prism/internal/tshwrap"
	"github.com/webvictim/prism/internal/tunnel"
)

// Forward is one beam's pair of local forwards to the LLM apps.
type Forward struct {
	UUID string
	// AnthropicHost / OpenAIHost are the apps' public addresses. The
	// forwards are opened to them by name so that the beam resolves them,
	// to its VNet, which is what carries the beam's identity.
	AnthropicHost string
	OpenAIHost    string
	// AnthropicPort / OpenAIPort are the local ends.
	AnthropicPort int
	OpenAIPort    int
}

// Args is the tsh command line for the forward:
//
//	tsh [--proxy=…] ssh -N --no-relogin -L a:anthropic…:443 -L o:openai…:443 beams@<uuid>
//
// --no-relogin keeps an expired login from opening a browser window from
// inside the daemon; the supervisor's tsh identity watcher restarts the
// forward once the user has logged in again. The node is named by its
// UUID, because that is the principal on its host certificate.
func (f Forward) Args() []string {
	args := []string{"ssh", "-N", "--no-relogin"}
	if f.AnthropicPort > 0 && f.AnthropicHost != "" {
		args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:%s:443", f.AnthropicPort, f.AnthropicHost))
	}
	if f.OpenAIPort > 0 && f.OpenAIHost != "" {
		args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:%s:443", f.OpenAIPort, f.OpenAIHost))
	}
	args = append(args, "beams@"+f.UUID)
	return tshwrap.Args(args...)
}

// runtime adapts a Forward to the tunnel supervisor, which then restarts
// it with backoff and kicks it when the user's tsh login comes back.
type runtime struct{ fwd Forward }

// Prepare waits for the beam's SSH node to be registered, which lags
// `tsh beams add` by a few seconds. Starting tsh ssh before then fails,
// and the supervisor would sit out its whole port wait before retrying.
func (r *runtime) Prepare(ctx context.Context, _ tunnel.AppInfo) error {
	deadline := time.Now().Add(nodeWait)
	for {
		ok, err := nodeVisible(ctx, r.fwd.UUID)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("beam %s has no SSH node yet", r.fwd.UUID)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// nodeWait bounds Prepare; the supervisor retries with backoff after it.
const nodeWait = 2 * time.Minute

// nodeVisible is swapped out by tests.
var nodeVisible = tshwrap.BeamNodeVisible

func (r *runtime) Command(ctx context.Context, _ tunnel.AppInfo) *exec.Cmd {
	bin, err := tshwrap.LookPathStrict("tsh")
	if err != nil {
		bin = "tsh"
	}
	return exec.CommandContext(ctx, bin, r.fwd.Args()...)
}

func (r *runtime) Name() string { return "tsh ssh (beam " + r.fwd.UUID + ")" }

func (r *runtime) WatchTshIdentity() bool { return true }
