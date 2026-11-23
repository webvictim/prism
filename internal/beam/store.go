// Package beam sends prism's inference out through a Teleport beam, so a
// local agent session is recorded, summarised and classified as a Beams
// session.
//
// The Beams session view is built from LLM proxy recordings carrying the
// beam's ID, and a request gets that ID by leaving the cluster from inside
// the beam: the beam's tbot holds certificates delegated from its owner,
// labelled with the beam, and its VNet applies them to every connection
// that leaves. So prism needs no agent, code or tool in the beam — only
// its inference. Each beam is reached with `tsh ssh -N -L` forwards to the
// two LLM apps, and an egress.Switch points the router at them.
//
// Beams live for at most 11 hours, so the Manager rotates: before one
// expires it starts the next, sends new requests there, and removes the
// old beam once its last request finishes. Removing a beam is what starts
// its summary, which appears about 15 minutes later.
package beam

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/webvictim/prism/internal/config"
)

// Record is one beam prism has sent inference through.
type Record struct {
	UUID  string `json:"uuid"`
	Alias string `json:"alias"`
	// Proxy is the Teleport proxy the beam belongs to, host:port.
	Proxy   string    `json:"proxy,omitempty"`
	Started time.Time `json:"started"`
	Expires time.Time `json:"expires"`
	// Ended is when prism stopped using it, zero while it is current.
	Ended time.Time `json:"ended,omitzero"`
	// Requests is how many requests prism sent through it.
	Requests int64 `json:"requests,omitempty"`
	// Key is the lane it carried: an agent session ("claude:<id>"), a
	// repository ("repo:<path>"), or one of shared, manual and standby.
	Key string `json:"key,omitempty"`
	// Workspace is the repository its requests were tagged with, which is
	// how `prism beam link --pr` picks a PR's beams.
	Workspace string `json:"workspace,omitempty"`
	// Adoptable marks a beam started for a `prism --beam` launch. A later
	// --beam launch from the same repository shares it rather than
	// starting another (Manager.adoptLocked), so a run of one-off requests
	// costs one beam and one wait. Its successor at rotation inherits it.
	Adoptable bool `json:"adoptable,omitempty"`
}

// Link returns the beam's session in the Teleport web UI. A beam's
// recording is keyed by its UUID; the page shows the summary once the
// beam has been removed and summarised.
func (r Record) Link(cluster string) string {
	host := r.Proxy
	if h, _, ok := cutPort(host); ok {
		host = h
	}
	if cluster == "" {
		cluster = host
	}
	return fmt.Sprintf("https://%s/web/cluster/%s/session/%s?recordingType=beam", host, cluster, r.UUID)
}

func cutPort(hostport string) (host, port string, ok bool) {
	for i := len(hostport) - 1; i >= 0; i-- {
		switch hostport[i] {
		case ':':
			return hostport[:i], hostport[i+1:], true
		case ']':
			return hostport, "", false
		}
	}
	return hostport, "", false
}

// File is beams.json: the beams running, by lane, and the history the
// CLI reads to say which beams a piece of work went through.
type File struct {
	// Lanes maps each running lane to its beam's UUID.
	Lanes map[string]string `json:"lanes,omitempty"`
	Beams []Record          `json:"beams"`
}

// Running reports whether uuid is a running lane's beam.
func (f *File) Running(uuid string) bool {
	for _, u := range f.Lanes {
		if u == uuid {
			return true
		}
	}
	return false
}

// maxHistory bounds the file: a week of 11-hour beams with room to spare.
const maxHistory = 64

// Find returns the record for uuid, or nil.
func (f *File) Find(uuid string) *Record {
	for i := range f.Beams {
		if f.Beams[i].UUID == uuid {
			return &f.Beams[i]
		}
	}
	return nil
}

// Put adds r, or replaces the record with its UUID.
func (f *File) Put(r Record) {
	if old := f.Find(r.UUID); old != nil {
		*old = r
		return
	}
	f.Beams = append(f.Beams, r)
	if len(f.Beams) > maxHistory {
		f.Beams = f.Beams[len(f.Beams)-maxHistory:]
	}
}

// Since returns the beams in use at any point after t, oldest first.
func (f *File) Since(t time.Time) []Record {
	var out []Record
	for _, r := range f.Beams {
		if r.Ended.IsZero() || r.Ended.After(t) {
			out = append(out, r)
		}
	}
	return out
}

// Path is <config dir>/beams.json.
func Path() (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "beams.json"), nil
}

// Load reads beams.json; a missing file is an empty one.
func Load() (*File, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &File{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &f, nil
}

// Save writes beams.json atomically, readable only by the user.
func Save(f *File) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// A temp file of its own (CreateTemp makes it 0600), so two writers
	// can never rename each other's file away.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".beams-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
