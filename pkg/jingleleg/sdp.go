package jingleleg

// Jingle <-> SDP mapping (the "mapping to SDP" direction of XEP-0167/0176/0339): enough of SDP
// to drive pion/webrtc's Offer/Answer API from Jingle and back. Non-trickle: we wait for ICE
// gathering to complete and carry every candidate in the Jingle transport (fine for calls).

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// sdpFromJingle renders a Jingle session (description + transport) as an SDP session
// description for pc.SetRemoteDescription. role "offer" or "answer".
func sdpFromJingle(j *Jingle, role string) (string, error) {
	d := j.AudioDescription()
	if d == nil {
		return "", fmt.Errorf("jingleleg: no audio description in jingle offer")
	}
	tr := j.AudioTransport()
	if tr == nil {
		return "", fmt.Errorf("jingleleg: no transport in jingle offer")
	}
	var pts []string
	var rtpmap, fmtp []string
	for _, pt := range d.PayloadTypes {
		pts = append(pts, strconv.Itoa(int(pt.ID)))
		ch := pt.Channels
		if ch == 0 {
			ch = 1
		}
		rtpmap = append(rtpmap, fmt.Sprintf("a=rtpmap:%d %s/%d/%d", pt.ID, pt.Name, pt.ClockRate, ch))
		if len(pt.Parameters) > 0 {
			var params []string
			for _, p := range pt.Parameters {
				params = append(params, p.Name+"="+p.Value)
			}
			fmtp = append(fmtp, fmt.Sprintf("a=fmtp:%d %s", pt.ID, strings.Join(params, ";")))
		}
	}
	setup := "actpass"
	for _, fp := range tr.Fingerprints {
		if fp.Setup != "" {
			setup = fp.Setup
		}
		break
	}
	port := 9
	if len(tr.Candidates) > 0 && tr.Candidates[0].Port > 0 {
		port = tr.Candidates[0].Port
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\r\n", args...) }
	w("v=0")
	w("o=- %d %d IN IP4 0.0.0.0", sidHash(j.SID), 1)
	w("s=-")
	w("t=0 0")
	w("a=group:BUNDLE 0")
	w("m=audio %d UDP/TLS/RTP/SAVPF %s", port, strings.Join(pts, " "))
	w("c=IN IP4 0.0.0.0")
	w("a=mid:0")
	w("a=rtcp-mux")
	if tr.Ufrag != "" {
		w("a=ice-ufrag:%s", tr.Ufrag)
	}
	if tr.Pwd != "" {
		w("a=ice-pwd:%s", tr.Pwd)
	}
	for _, fp := range tr.Fingerprints {
		w("a=fingerprint:%s %s", fp.Hash, fp.Value)
		break
	}
	w("a=setup:%s", setup)
	for _, line := range rtpmap {
		b.WriteString(line + "\r\n")
	}
	for _, line := range fmtp {
		b.WriteString(line + "\r\n")
	}
	for _, c := range tr.Candidates {
		w("a=candidate:%s %d %s %d %s %d typ %s",
			orDefault(c.Foundation, "1"), c.Component, strings.ToUpper(orDefault(c.Protocol, "udp")),
			c.Priority, c.IP, c.Port, orDefault(c.Type, "host"))
	}
	if src := firstSource(d); src != nil {
		w("a=ssrc:%d cname:%s", src.SSRC, cnameOf(src))
	}
	w("a=sendrecv")
	return b.String(), nil
}

// jingleFromSDP parses a pion SDP offer/answer into a Jingle description + transport.
func jingleFromSDP(sdp string) (*Description, *JingleTransport, error) {
	d := &Description{Media: "audio"}
	tr := &JingleTransport{}
	byPT := map[uint8]*PayloadType{}
	var order []uint8
	var haveFP bool

	sc := bufio.NewScanner(strings.NewReader(sdp))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		switch {
		case strings.HasPrefix(line, "m=audio"):
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				for _, f := range fields[3:] {
					pt, err := strconv.Atoi(f)
					if err == nil && pt >= 0 && pt <= 127 {
						order = append(order, uint8(pt))
						byPT[uint8(pt)] = &PayloadType{ID: uint8(pt)}
					}
				}
			}
		case strings.HasPrefix(line, "a=rtpmap:"):
			rest := line[len("a=rtpmap:"):]
			sp := strings.IndexByte(rest, ' ')
			if sp < 0 {
				continue
			}
			pt, err := strconv.Atoi(rest[:sp])
			if err != nil {
				continue
			}
			p := byPT[uint8(pt)]
			if p == nil {
				p = &PayloadType{ID: uint8(pt)}
				byPT[uint8(pt)] = p
				order = append(order, uint8(pt))
			}
			spec := rest[sp+1:]
			name, clock, ch := parseRtpmap(spec)
			p.Name = name
			p.ClockRate = clock
			p.Channels = ch
		case strings.HasPrefix(line, "a=fmtp:"):
			rest := line[len("a=fmtp:"):]
			sp := strings.IndexByte(rest, ' ')
			if sp < 0 {
				continue
			}
			pt, err := strconv.Atoi(rest[:sp])
			if err != nil {
				continue
			}
			p := byPT[uint8(pt)]
			if p == nil {
				continue
			}
			for _, kv := range strings.Split(rest[sp+1:], ";") {
				kv = strings.TrimSpace(kv)
				if kv == "" {
					continue
				}
				if eq := strings.IndexByte(kv, '='); eq > 0 {
					p.Parameters = append(p.Parameters, Param{Name: kv[:eq], Value: kv[eq+1:]})
				} else {
					p.Parameters = append(p.Parameters, Param{Name: kv, Value: "1"})
				}
			}
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			tr.Ufrag = line[len("a=ice-ufrag:"):]
		case strings.HasPrefix(line, "a=ice-pwd:"):
			tr.Pwd = line[len("a=ice-pwd:"):]
		case strings.HasPrefix(line, "a=fingerprint:"):
			rest := line[len("a=fingerprint:"):]
			sp := strings.IndexByte(rest, ' ')
			if sp > 0 {
				tr.Fingerprints = append(tr.Fingerprints, Fingerprint{
					Hash: strings.ToLower(rest[:sp]), Value: strings.TrimSpace(rest[sp+1:]),
				})
				haveFP = true
			}
		case strings.HasPrefix(line, "a=setup:"):
			setup := line[len("a=setup:"):]
			if haveFP {
				tr.Fingerprints[len(tr.Fingerprints)-1].Setup = setup
			}
		case strings.HasPrefix(line, "a=candidate:"):
			if c, ok := parseSDPCandidate(line[len("a=candidate:"):]); ok {
				c.ID = fmt.Sprintf("c%d", len(tr.Candidates))
				tr.Candidates = append(tr.Candidates, c)
			}
		case strings.HasPrefix(line, "a=ssrc:"):
			rest := line[len("a=ssrc:"):]
			sp := strings.IndexByte(rest, ' ')
			ssrcStr := rest
			cname := "stream"
			if sp > 0 {
				ssrcStr = rest[:sp]
				if strings.HasPrefix(rest[sp+1:], "cname:") {
					cname = rest[sp+1+len("cname:"):]
				}
			}
			ssrc, err := strconv.ParseUint(ssrcStr, 10, 32)
			if err == nil {
				d.Sources = append(d.Sources, Source{
					SSRC:   uint32(ssrc),
					Params: []Param{{Name: "cname", Value: cname}},
				})
			}
		}
	}
	for _, pt := range order {
		d.PayloadTypes = append(d.PayloadTypes, *byPT[pt])
	}
	if len(d.PayloadTypes) == 0 {
		return nil, nil, fmt.Errorf("jingleleg: no audio payload types in sdp")
	}
	return d, tr, nil
}

func parseRtpmap(spec string) (name string, clock uint32, channels uint8) {
	channels = 1
	slash := strings.Split(spec, "/")
	if len(slash) == 0 {
		return
	}
	name = slash[0]
	if len(slash) > 1 {
		if v, err := strconv.Atoi(slash[1]); err == nil {
			clock = uint32(v)
		}
	}
	if len(slash) > 2 {
		if v, err := strconv.Atoi(slash[2]); err == nil && v > 0 {
			channels = uint8(v)
		}
	}
	return
}

func parseSDPCandidate(s string) (Candidate, bool) {
	f := strings.Fields(s)
	if len(f) < 8 {
		return Candidate{}, false
	}
	priority, _ := strconv.Atoi(f[3])
	port, _ := strconv.Atoi(f[5])
	comp, _ := strconv.Atoi(f[1])
	c := Candidate{
		Foundation: f[0],
		Component:  comp,
		Protocol:   strings.ToLower(f[2]),
		Priority:   priority,
		IP:         f[4],
		Port:       port,
		Type:       "host",
	}
	for i := 0; i < len(f)-1; i++ {
		if f[i] == "typ" {
			c.Type = f[i+1]
		}
	}
	return c, true
}

func firstSource(d *Description) *Source {
	if len(d.Sources) > 0 {
		return &d.Sources[0]
	}
	return nil
}

func cnameOf(s *Source) string {
	for _, p := range s.Params {
		if p.Name == "cname" {
			return p.Value
		}
	}
	return "stream"
}

// sidHash is a stable non-zero o= session id derived from the Jingle sid.
func sidHash(sid string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(sid); i++ {
		h ^= uint32(sid[i])
		h *= 16777619
	}
	if h == 0 {
		h = 1
	}
	return h
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
