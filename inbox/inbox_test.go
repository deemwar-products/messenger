package inbox

import (
	"testing"

	"github.com/deemwar-products/messenger/envelope"
)

// Append then Since: offsets advance so a poller sees only new messages.
func TestInbox_AppendAndSince(t *testing.T) {
	box, err := Open(t.TempDir() + "/inbox.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	for _, txt := range []string{"a", "b", "c"} {
		if err := box.Append(envelope.Inbound("hook", "s", txt, "Hook")); err != nil {
			t.Fatal(err)
		}
	}
	all, next, err := box.Since(0)
	if err != nil || len(all) != 3 || next != 3 {
		t.Fatalf("since(0): len=%d next=%d err=%v", len(all), next, err)
	}
	tail, next2, _ := box.Since(2)
	if len(tail) != 1 || tail[0].Text != "c" || next2 != 3 {
		t.Fatalf("since(2): %+v next=%d", tail, next2)
	}
	// A fresh inbox is empty, not an error.
	empty, _ := Open(t.TempDir() + "/none.ndjson")
	msgs, n, err := empty.Since(0)
	if err != nil || len(msgs) != 0 || n != 0 {
		t.Fatalf("empty inbox: msgs=%d n=%d err=%v", len(msgs), n, err)
	}
}
