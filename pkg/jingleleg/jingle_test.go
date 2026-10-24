package jingleleg

import (
	"strings"
	"testing"
)

func TestJingleRoundTrip(t *testing.T) {
	in := &Jingle{
		Action:    ActionSessionInitiate,
		SID:       "sid123",
		Initiator: "alice@example.com",
		Contents: []Content{{
			Creator: "initiator",
			Name:    "voice",
			Senders: "both",
			Description: &Description{
				Media: "audio",
				PayloadTypes: []PayloadType{
					{ID: 111, Name: "opus", ClockRate: 48000, Channels: 2,
						Parameters: []Param{{Name: "minptime", Value: "10"}, {Name: "useinbandfec", Value: "1"}}},
					{ID: 101, Name: "telephone-event", ClockRate: 8000},
				},
				Sources: []Source{{SSRC: 0xdeadbeef, Params: []Param{{Name: "cname", Value: "stream"}}}},
			},
			Transport: &JingleTransport{
				Ufrag: "ufrag1",
				Pwd:   "pwd1",
				Candidates: []Candidate{
					{Component: 1, Foundation: "1", Generation: 0, IP: "203.0.113.1", Port: 45678,
						Priority: 2122260223, Protocol: "udp", Type: "host", ID: "c1"},
				},
				Fingerprints: []Fingerprint{{Hash: "sha-256", Setup: "actpass",
					Value: "1A:2B:3C:4D:5E:6F:70:81:92:A3:B4:C5:D6:E7:F8:09:1A:2B:3C:4D:5E:6F:70:81:92:A3:B4:C5:D6:E7:F8:09"}},
			},
		}},
	}
	raw, err := MarshalJingle(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseJingle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionSessionInitiate || out.SID != "sid123" || out.Initiator != "alice@example.com" {
		t.Fatalf("header mismatch: %+v", out)
	}
	d := out.AudioDescription()
	if d == nil || d.Media != "audio" || len(d.PayloadTypes) != 2 {
		t.Fatalf("description mismatch: %+v", d)
	}
	if d.PayloadTypes[0].Name != "opus" || d.PayloadTypes[0].ClockRate != 48000 || len(d.PayloadTypes[0].Parameters) != 2 {
		t.Fatalf("payload-type mismatch: %+v", d.PayloadTypes[0])
	}
	if len(d.Sources) != 1 || d.Sources[0].SSRC != 0xdeadbeef {
		t.Fatalf("sources mismatch: %+v", d.Sources)
	}
	tr := out.AudioTransport()
	if tr == nil || tr.Ufrag != "ufrag1" || tr.Pwd != "pwd1" {
		t.Fatalf("transport mismatch: %+v", tr)
	}
	if len(tr.Candidates) != 1 || tr.Candidates[0].IP != "203.0.113.1" || tr.Candidates[0].Type != "host" {
		t.Fatalf("candidate mismatch: %+v", tr.Candidates)
	}
	if len(tr.Fingerprints) != 1 || tr.Fingerprints[0].Hash != "sha-256" || tr.Fingerprints[0].Setup != "actpass" {
		t.Fatalf("fingerprint mismatch: %+v", tr.Fingerprints)
	}
}

func TestJingleTerminateReason(t *testing.T) {
	raw, err := MarshalJingle(&Jingle{
		Action: ActionSessionTerminate,
		SID:    "s1",
		Reason: &Reason{Condition: ReasonDeclined, Text: "no thanks"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "decline") || !strings.Contains(string(raw), "no thanks") {
		t.Fatalf("reason not marshalled: %s", raw)
	}
	j, err := ParseJingle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if j.Reason == nil || j.Reason.Condition != ReasonDeclined || j.Reason.Text != "no thanks" {
		t.Fatalf("reason mismatch: %+v", j.Reason)
	}
}

func TestJingleSessionInfo(t *testing.T) {
	raw, err := MarshalJingle(&Jingle{Action: ActionSessionInfo, SID: "s1", InfoName: "ringing"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ringing") {
		t.Fatalf("info not marshalled: %s", raw)
	}
	j, err := ParseJingle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if j.InfoName != "ringing" {
		t.Fatalf("info name mismatch: %q", j.InfoName)
	}
}

func TestParseJingleRejectsForeignPayload(t *testing.T) {
	if _, err := ParseJingle([]byte(`<iq xmlns="jabber:client"><ping/></iq>`)); err == nil {
		t.Fatal("expected error for non-jingle payload")
	}
}

func TestJMIRoundTrip(t *testing.T) {
	cases := []JMI{
		{Action: JMIPropose, ID: "sid1", Descriptions: []Description{{Media: "audio"}}},
		{Action: JMIProceed, ID: "sid1"},
		{Action: JMIReject, ID: "sid1", Reason: &Reason{Condition: ReasonDeclined}},
		{Action: JMIRetract, ID: "sid1", Reason: &Reason{Condition: ReasonCancel}},
		{Action: JMIFinish, ID: "sid1", Reason: &Reason{Condition: ReasonSuccess}},
		{Action: JMIAccept, ID: "sid1"},
	}
	for _, in := range cases {
		raw, err := in.Marshal()
		if err != nil {
			t.Fatalf("%s: %v", in.Action, err)
		}
		out, ok, err := ParseJMI(raw)
		if err != nil || !ok {
			t.Fatalf("%s: parse failed ok=%v err=%v raw=%s", in.Action, ok, err, raw)
		}
		if out.Action != in.Action || out.ID != in.ID {
			t.Fatalf("%s: mismatch %+v", in.Action, out)
		}
		if in.Reason != nil && (out.Reason == nil || out.Reason.Condition != in.Reason.Condition) {
			t.Fatalf("%s: reason mismatch %+v", in.Action, out.Reason)
		}
		if in.Action == JMIPropose && len(out.Descriptions) != 1 {
			t.Fatalf("propose descriptions lost: %+v", out)
		}
	}
}

func TestParseJMIRejectsOtherMessages(t *testing.T) {
	if _, ok, err := ParseJMI([]byte(`<body>hi</body>`)); ok || err != nil {
		t.Fatalf("body must not parse as jmi (ok=%v err=%v)", ok, err)
	}
}

func TestE164FromJID(t *testing.T) {
	cases := map[string]string{
		"+15551234567@cheogram.com":       "+15551234567",
		"+15551234567@cheogram.com/phone": "+15551234567",
		"alice@example.com":               "",
		"+33733;phone-context=ca-us.phone-context.soprani.ca@cheogram.com": "",
	}
	for in, want := range cases {
		if got := e164FromJID(in); got != want {
			t.Errorf("e164FromJID(%q) = %q, want %q", in, got, want)
		}
	}
}
