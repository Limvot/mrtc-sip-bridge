package sipua

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

func TestDiagoLegSkipsParseErrors(t *testing.T) {
	calls := 0
	l := &diagoLeg{buf: make([]byte, 1500)}
	l.read = func(b []byte) (int, legHeader, error) {
		calls++
		if calls == 1 {
			return 0, legHeader{}, errors.New("rtp: packet too short")
		}
		b[0] = 0xAA
		return 1, legHeader{SSRC: 7, SequenceNumber: 9, PayloadType: 0}, nil
	}
	var p media.RTPPacket
	if err := l.ReadRTP(&p); err != nil {
		t.Fatal(err)
	}
	if p.SSRC != 7 || p.Seq != 9 || len(p.Payload) != 1 || l.badRTP != 0 {
		t.Fatalf("%+v bad=%d", p, l.badRTP)
	}
}

func TestDiagoLegEOFAndGiveUp(t *testing.T) {
	for _, e := range []error{io.EOF, net.ErrClosed} {
		l := &diagoLeg{read: func([]byte) (int, legHeader, error) { return 0, legHeader{}, e }}
		if err := l.ReadRTP(&media.RTPPacket{}); err != io.EOF {
			t.Fatalf("%v -> %v", e, err)
		}
	}
	bad := errors.New("bad")
	n := 0
	l := &diagoLeg{read: func([]byte) (int, legHeader, error) { n++; return 0, legHeader{}, bad }}
	if err := l.ReadRTP(&media.RTPPacket{}); !errors.Is(err, bad) || n != maxConsecutiveBadRTP {
		t.Fatalf("err=%v n=%d", err, n)
	}
}
