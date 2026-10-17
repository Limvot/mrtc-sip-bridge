package sipua

import (
	"reflect"
	"strings"
	"testing"
)

func sdpBody(lines ...string) []byte { return []byte(strings.Join(lines, "\r\n") + "\r\n") }

func TestParseOffer(t *testing.T) {
	jmp := sdpBody("v=0", "o=- 1 1 IN IP4 203.0.113.9", "s=-", "c=IN IP4 203.0.113.9", "t=0 0",
		"m=audio 5004 RTP/AVP 0 18 101",
		"a=rtpmap:0 PCMU/8000", "a=rtpmap:18 G729/8000", "a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16", "a=ptime:20")
	cases := []struct {
		name string
		body []byte
		want Offer
	}{
		{"jmp", jmp, Offer{HasSDP: true, PCMU: true, TelephoneEvent: true, TelephoneEventPT: 101, MediaAddr: "203.0.113.9:5004",
			Codecs: []string{"PCMU/0", "G729/18", "telephone-event/101"}}},
		{"pcma only", sdpBody("v=0", "c=IN IP4 198.51.100.1", "m=audio 4000 RTP/AVP 8"),
			Offer{HasSDP: true, MediaAddr: "198.51.100.1:4000", Codecs: []string{"PCMA/8"}}},
		{"dynamic without rtpmap", sdpBody("v=0", "c=IN IP4 198.51.100.1", "m=audio 4000 RTP/AVP 96"),
			Offer{HasSDP: true, MediaAddr: "198.51.100.1:4000", Codecs: []string{"pt96/96"}}},
		{"audio and video", sdpBody("v=0", "c=IN IP4 198.51.100.1", "m=video 6000 RTP/AVP 96", "m=audio 4000 RTP/AVP 0",
			"a=rtpmap:96 VP8/90000"),
			Offer{HasSDP: true, PCMU: true, MediaAddr: "198.51.100.1:4000", Codecs: []string{"PCMU/0"}}},
		{"telephone-event dynamic pt", sdpBody("v=0", "c=IN IP4 198.51.100.1", "m=audio 4000 RTP/AVP 0 96",
			"a=rtpmap:96 telephone-event/8000"),
			Offer{HasSDP: true, PCMU: true, TelephoneEvent: true, TelephoneEventPT: 96, MediaAddr: "198.51.100.1:4000",
				Codecs: []string{"PCMU/0", "telephone-event/96"}}},
		{"empty", nil, Offer{}},
		{"garbage", []byte("this is not sdp"), Offer{HasSDP: true}},
		{"short c line", sdpBody("v=0", "c=IN", "m=audio 4000 RTP/AVP 0"),
			Offer{HasSDP: true, PCMU: true, Codecs: []string{"PCMU/0"}}},
		{"no audio", sdpBody("v=0", "m=video 6000 RTP/AVP 96"), Offer{HasSDP: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseOffer(c.body)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseAddressUser(t *testing.T) {
	for in, want := range map[string]string{
		`<sip:+15551234567@example.com>`:               "+15551234567",
		`"Bob" <sip:bob@example.com>;x=1, <tel:+1555>`: "bob",
		``:        "",
		`garbage`: "",
	} {
		if got := parseAddressUser(in); got != want {
			t.Errorf("parseAddressUser(%q) = %q, want %q", in, got, want)
		}
	}
}
