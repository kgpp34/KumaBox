package cloudhypervisor

import (
	"strings"
	"testing"
)

func TestReadHybridVsockReplyStopsAtNewline(t *testing.T) {
	reply, err := readHybridVsockReply(strings.NewReader("OK 1024\nagent-frame\n"))
	if err != nil {
		t.Fatal(err)
	}
	if reply != "OK 1024\n" {
		t.Fatalf("reply = %q", reply)
	}
}

func TestReadHybridVsockReplyIsBounded(t *testing.T) {
	if _, err := readHybridVsockReply(strings.NewReader(strings.Repeat("x", hybridVsockReplyLimit))); err == nil {
		t.Fatal("accepted an unbounded handshake reply")
	}
}
