package beam

import (
	"context"
	"fmt"
	"net/http"

	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/tshwrap"
)

// Trial is a beam started for `prism beam test`: outside any daemon, and
// only to prove the whole path works — a beam can be started, its forward
// opened, and inference sent through it, as the daemon would.
//
// Requests go to Trial.URL(…) through Trial.Client(): the same egress
// Switch the daemon uses, with the trial's beam active, so they leave
// exactly the way an agent's would.
type Trial struct {
	ops    Ops
	Beam   *tshwrap.Beam
	conn   Connection
	sw     *egress.Switch
	target *egress.Target
}

// The tunnel ports a trial's requests are addressed to. Nothing listens
// on them: the Switch recognises them and sends the requests through the
// beam instead.
const (
	trialAnthropicPort = 1
	trialOpenAIPort    = 2
)

// NewTrial returns a Trial that talks to the cluster through ops.
func NewTrial(ops Ops) *Trial { return &Trial{ops: ops, sw: egress.NewSwitch(nil)} }

// Start creates the beam.
func (t *Trial) Start(ctx context.Context) (*tshwrap.Beam, error) {
	b, err := t.ops.Add(ctx)
	if err != nil {
		return nil, err
	}
	t.Beam = b
	return b, nil
}

// Connect opens the beam's forwards and routes the trial's requests
// through them.
func (t *Trial) Connect(ctx context.Context) error {
	if t.Beam == nil {
		return fmt.Errorf("no beam started")
	}
	anth, oai, err := t.ops.AppHosts(ctx)
	if err != nil {
		return fmt.Errorf("find the LLM apps: %w", err)
	}
	ports, err := freePorts(2)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := t.ops.Connect(cctx, Forward{UUID: t.Beam.UUID, AnthropicHost: anth, OpenAIHost: oai,
		AnthropicPort: ports[0], OpenAIPort: ports[1]})
	if err != nil {
		return fmt.Errorf("open forwards: %w", err)
	}
	t.conn = conn
	target, err := egress.NewTarget(t.Beam.ID,
		egress.Route{TunnelPort: trialAnthropicPort, AppHost: anth, ForwardAddr: fmt.Sprintf("127.0.0.1:%d", ports[0])},
		egress.Route{TunnelPort: trialOpenAIPort, AppHost: oai, ForwardAddr: fmt.Sprintf("127.0.0.1:%d", ports[1])},
	)
	if err != nil {
		return err
	}
	t.target = target
	t.sw.Activate(target)
	return nil
}

// Client sends requests through the beam.
func (t *Trial) Client() *http.Client { return &http.Client{Transport: t.sw} }

// URL is where to send a request for app ("anthropic" or "openai") at
// path, e.g. URL("anthropic", "/v1/messages").
func (t *Trial) URL(app, path string) string {
	port := trialAnthropicPort
	if app == "openai" {
		port = trialOpenAIPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

// Close closes the forwards and, unless keep is set, removes the beam so
// its summary can start.
func (t *Trial) Close(ctx context.Context, keep bool) error {
	if t.target != nil {
		t.sw.Activate(nil)
		t.target.Close()
	}
	if t.conn != nil {
		t.conn.Close()
	}
	if t.Beam == nil || keep {
		return nil
	}
	return t.ops.Remove(ctx, t.Beam.UUID)
}
