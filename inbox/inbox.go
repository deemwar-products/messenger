// Package inbox is messenger's broker-free inbound store: an append-only NDJSON file of
// envelopes. `listen` appends every inbound message; `serve` reads back GET /inbox?since=N
// where N is a 1-based line offset, so a subscriber polls for new messages without a broker.
package inbox

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/deemwar-products/messenger/envelope"
)

// Inbox is an append-only NDJSON file guarded by a process-local mutex (one writer per
// messenger process). Offsets are 1-based line numbers so a poller advances `since` by
// the count it has consumed.
type Inbox struct {
	path string
	mu   sync.Mutex
}

// Open returns an Inbox at path, creating the parent directory.
func Open(path string) (*Inbox, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return &Inbox{path: path}, nil
}

// Append writes one envelope as an NDJSON line.
func (i *Inbox) Append(env envelope.Envelope) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	f, err := os.OpenFile(i.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Last returns the most recent inbound envelope on channel (and, when thread is
// non-empty, in that thread) — the message a conversational reply is "obviously" for.
// ok=false when the conversation has no messages yet.
func (i *Inbox) Last(channel, thread string) (envelope.Envelope, bool, error) {
	msgs, _, err := i.Since(0)
	if err != nil {
		return envelope.Envelope{}, false, err
	}
	for j := len(msgs) - 1; j >= 0; j-- {
		m := msgs[j]
		if m.Channel != channel {
			continue
		}
		if thread != "" && m.ThreadID != thread {
			continue
		}
		return m, true, nil
	}
	return envelope.Envelope{}, false, nil
}

// Since returns every envelope after 1-based offset `since` (since<=0 means from the
// start), plus the new offset the caller should pass next time. A missing file is empty.
func (i *Inbox) Since(since int) ([]envelope.Envelope, int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f, err := os.Open(i.path)
	if os.IsNotExist(err) {
		if since < 0 {
			since = 0
		}
		return nil, since, nil
	}
	if err != nil {
		return nil, since, err
	}
	defer f.Close()

	var out []envelope.Envelope
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		n++
		if n <= since {
			continue
		}
		var env envelope.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue // skip a corrupt line rather than fail the whole read
		}
		out = append(out, env)
	}
	if err := sc.Err(); err != nil {
		return nil, since, err
	}
	return out, n, nil
}
