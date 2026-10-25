// Package jingleleg is a Jingle (XMPP native call) implementation of the pkg/leg phone-leg
// API (ADR-014). It bridges XEP-0166 Jingle sessions and XEP-0353 Jingle Message Initiation
// to the pkg/calls state machine, with media on a WebRTC Engine behind the Engine seam.
//
// The package is transport-agnostic: it never owns an XMPP connection. The host (the embedded
// jmpchat-matrix-bridge connector, or any XMPP client) forwards Jingle <iq> payloads and JMI
// <message> payloads in through HandleIQ/HandleMessage and sends what the Transport interface
// asks it to send. That keeps the fork-side diff to a stanza-forwarding shim.
//
// Protocol dialect note: the mapping of "call" to JMI propose/proceed and session-initiate
// follows XEP-0353 §2 (the party that receives <proceed/> initiates the Jingle session) and
// supports the direct session-initiate flow as a fallback (Config.ProposeFirst=false). Which
// flow the Cheogram/JMP call gateway uses is UNVERIFIED (docs/RESEARCH.md); both are accepted
// inbound, and outbound defaults to the JMI flow.
package jingleleg

import (
	"encoding/xml"
	"fmt"
)

// Namespaces.
const (
	NSJingle  = "urn:xmpp:jingle:1"
	NSRTP     = "urn:xmpp:jingle:apps:rtp:1"
	NSRTPInfo = "urn:xmpp:jingle:apps:rtp:info:1"
	NSICEUDP  = "urn:xmpp:jingle:transports:ice-udp:1"
	NSDTLS    = "urn:xmpp:jingle:apps:dtls:0"
	NSSSMA    = "urn:xmpp:jingle:apps:rtp:ssma:0"
	NSGroup   = "urn:xmpp:jingle:apps:grouping:0"
	NSHDRExt  = "urn:xmpp:jingle:apps:rtp:rtp-hdrext:0"
	NSJMI0    = "urn:xmpp:jingle-message:0"
	NSJMI1    = "urn:xmpp:jingle-message:1"
)

// Jingle actions used here.
const (
	ActionSessionInitiate  = "session-initiate"
	ActionSessionAccept    = "session-accept"
	ActionSessionTerminate = "session-terminate"
	ActionSessionInfo      = "session-info"
)

// Jingle is the <jingle/> element (XEP-0166).
type Jingle struct {
	XMLName   xml.Name  `xml:"urn:xmpp:jingle:1 jingle"`
	Action    string    `xml:"action,attr"`
	SID       string    `xml:"sid,attr"`
	Initiator string    `xml:"initiator,attr,omitempty"`
	Responder string    `xml:"responder,attr,omitempty"`
	Contents  []Content `xml:"content"`
	Reason    *Reason   `xml:"reason,omitempty"`
	// InfoName is the child element name of <session-info/> in NSRTPInfo ("ringing", "hold",
	// "mute", "unhold", "active", ...), "" if there is no session-info.
	InfoName string `xml:"-"`
}

// Content is <content/> (XEP-0166).
type Content struct {
	XMLName     xml.Name         `xml:"content"`
	Creator     string           `xml:"creator,attr"`
	Name        string           `xml:"name,attr"`
	Senders     string           `xml:"senders,attr,omitempty"`
	Description *Description     `xml:"description,omitempty"`
	Transport   *JingleTransport `xml:"transport,omitempty"`
}

// Description is the RTP application description (XEP-0167).
type Description struct {
	XMLName xml.Name `xml:"urn:xmpp:jingle:apps:rtp:1 description"`
	Media   string   `xml:"media,attr"`
	// RTCPMux declares RFC 5761 RTP/RTCP multiplexing (we always mux; without the element
	// peers read our muxed RTCP sender-reports as audio: periodic "blip blip blip").
	RTCPMux      bool          `xml:"rtcp-mux,omitempty"`
	SSRC         uint32        `xml:"ssrc,attr,omitempty"`
	PayloadTypes []PayloadType `xml:"payload-type"`
	Sources      []Source      `xml:"source"`
	SSRCGroups   []SSRCGroup   `xml:"ssrc-group"`
	HDRExts      []HDRExt      `xml:"rtp-hdrext"`
}

// PayloadType is one codec (XEP-0167).
type PayloadType struct {
	XMLName    xml.Name `xml:"payload-type"`
	ID         uint8    `xml:"id,attr"`
	Name       string   `xml:"name,attr"`
	ClockRate  uint32   `xml:"clockrate,attr,omitempty"`
	Channels   uint8    `xml:"channels,attr,omitempty"`
	Maxptime   uint32   `xml:"maxptime,attr,omitempty"`
	Ptime      uint32   `xml:"ptime,attr,omitempty"`
	Parameters []Param  `xml:"parameter"`
}

// Param is a codec <parameter/> (XEP-0167).
type Param struct {
	XMLName xml.Name `xml:"parameter"`
	Name    string   `xml:"name,attr"`
	Value   string   `xml:"value,attr"`
}

// Source is an SSRC source description (XEP-0339).
type Source struct {
	XMLName xml.Name `xml:"urn:xmpp:jingle:apps:rtp:ssma:0 source"`
	SSRC    uint32   `xml:"ssrc,attr"`
	Params  []Param  `xml:"parameter"`
}

// SSRCGroup groups SSRCs (XEP-0338/0339), e.g. semantics "FID".
type SSRCGroup struct {
	XMLName   xml.Name `xml:"urn:xmpp:jingle:apps:grouping:0 ssrc-group"`
	Semantics string   `xml:"semantics,attr"`
	Sources   []Source `xml:"source"`
}

// HDRExt is one negotiated RTP header extension (XEP-0294).
type HDRExt struct {
	XMLName xml.Name `xml:"urn:xmpp:jingle:apps:rtp:rtp-hdrext:0 rtp-hdrext"`
	ID      uint8    `xml:"id,attr"`
	URI     string   `xml:"uri,attr"`
	Senders string   `xml:"senders,attr,omitempty"`
}

// JingleTransport is the ICE-UDP transport (XEP-0176) with DTLS fingerprints (XEP-0176 §5 DTLS).
type JingleTransport struct {
	XMLName      xml.Name      `xml:"urn:xmpp:jingle:transports:ice-udp:1 transport"`
	Ufrag        string        `xml:"ufrag,attr,omitempty"`
	Pwd          string        `xml:"pwd,attr,omitempty"`
	Candidates   []Candidate   `xml:"candidate"`
	Fingerprints []Fingerprint `xml:"fingerprint"`
}

// Candidate is one ICE candidate (XEP-0176).
type Candidate struct {
	XMLName    xml.Name `xml:"candidate"`
	Component  int      `xml:"component,attr"`
	Foundation string   `xml:"foundation,attr"`
	Generation int      `xml:"generation,attr"`
	ID         string   `xml:"id,attr"`
	IP         string   `xml:"ip,attr"`
	Port       int      `xml:"port,attr"`
	Priority   int      `xml:"priority,attr"`
	Protocol   string   `xml:"protocol,attr"`
	Type       string   `xml:"type,attr"`
	RelAddr    string   `xml:"rel-addr,attr,omitempty"`
	RelPort    int      `xml:"rel-port,attr,omitempty"`
}

// Fingerprint is a DTLS fingerprint (urn:xmpp:jingle:apps:dtls:0).
type Fingerprint struct {
	XMLName xml.Name `xml:"urn:xmpp:jingle:apps:dtls:0 fingerprint"`
	Hash    string   `xml:"hash,attr"`
	Setup   string   `xml:"setup,attr,omitempty"`
	Value   string   `xml:",chardata"`
}

// Reason is <reason/>: one condition element (XEP-0166 §7.2.1, e.g. success, busy, cancel,
// declined, timeout, general-error) plus optional <text/>.
type Reason struct {
	XMLName   xml.Name `xml:"reason"`
	Condition string   `xml:"-"` // element name of the condition, e.g. "success"
	Text      string   `xml:"text,omitempty"`
}

// End-condition names (XEP-0166 §7.2.1).
// End-condition names (XEP-0166 §7.2.1). Easy to get wrong: it is <declined/>, not <decline/>,
// and <general-error/>, not <failed/>.
const (
	ReasonSuccess               = "success"
	ReasonBusy                  = "busy"
	ReasonCancel                = "cancel"
	ReasonDeclined              = "declined"
	ReasonTimeout               = "timeout"
	ReasonGone                  = "gone"
	ReasonGeneralError          = "general-error"
	ReasonAlternativeSession    = "alternative-session"
	ReasonFailedApplication     = "failed-application"
	ReasonFailedTransport       = "failed-transport"
	ReasonUnsupportedApps       = "unsupported-applications"
	ReasonUnsupportedTransports = "unsupported-transports"
	ReasonIncompatibleParams    = "incompatible-parameters"
	ReasonConnectivityError     = "connectivity-error"
	ReasonMediaError            = "media-error"
	ReasonSecurityError         = "security-error"
	ReasonExpired               = "expired"
)

// MarshalXML writes <reason><condition/><text/></reason>.
func (r Reason) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "reason"}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if r.Condition != "" {
		if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: r.Condition}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: r.Condition}}); err != nil {
			return err
		}
	}
	if r.Text != "" {
		if err := e.EncodeElement(struct {
			XMLName xml.Name `xml:"text"`
			T       string   `xml:",chardata"`
		}{T: r.Text}, xml.StartElement{Name: xml.Name{Local: "text"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(xml.EndElement{Name: start.Name})
}

// UnmarshalXML reads <reason/>, taking the first child element as the condition.
func (r *Reason) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "text" {
				var s struct {
					T string `xml:",chardata"`
				}
				if err := d.DecodeElement(&s, &t); err != nil {
					return err
				}
				r.Text = s.T
			} else {
				if r.Condition == "" {
					r.Condition = t.Name.Local
				}
				if err := d.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			return nil
		}
	}
}

// MarshalXML writes <jingle/> with an optional <session-info><name/></session-info>.
func (j Jingle) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "jingle", Space: NSJingle}
	start.Attr = []xml.Attr{
		{Name: xml.Name{Local: "action"}, Value: j.Action},
		{Name: xml.Name{Local: "sid"}, Value: j.SID},
	}
	if j.Initiator != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "initiator"}, Value: j.Initiator})
	}
	if j.Responder != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "responder"}, Value: j.Responder})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, c := range j.Contents {
		if err := e.Encode(c); err != nil {
			return err
		}
	}
	if j.InfoName != "" {
		if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "session-info", Space: NSRTPInfo}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: j.InfoName, Space: NSRTPInfo}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: j.InfoName, Space: NSRTPInfo}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "session-info", Space: NSRTPInfo}}); err != nil {
			return err
		}
	}
	if j.Reason != nil {
		if err := e.Encode(j.Reason); err != nil {
			return err
		}
	}
	return e.EncodeToken(xml.EndElement{Name: start.Name})
}

// ParseJingle decodes one <jingle/> payload (the raw child XML of an <iq>).
func ParseJingle(payload []byte) (*Jingle, error) {
	var probe struct {
		XMLName   xml.Name
		Action    string    `xml:"action,attr"`
		SID       string    `xml:"sid,attr"`
		Initiator string    `xml:"initiator,attr"`
		Responder string    `xml:"responder,attr"`
		Contents  []Content `xml:"content"`
		Reason    *Reason   `xml:"reason"`
		Info      struct {
			Inner xml.Name `xml:",any"`
		} `xml:"session-info"`
	}
	if err := xml.Unmarshal(payload, &probe); err != nil {
		return nil, fmt.Errorf("jingleleg: parse jingle: %w", err)
	}
	if probe.XMLName.Local != "jingle" {
		return nil, fmt.Errorf("jingleleg: not a <jingle/> payload (got <%s>)", probe.XMLName.Local)
	}
	j := &Jingle{
		Action:    probe.Action,
		SID:       probe.SID,
		Initiator: probe.Initiator,
		Responder: probe.Responder,
		Contents:  probe.Contents,
		Reason:    probe.Reason,
		InfoName:  probe.Info.Inner.Local,
	}
	return j, nil
}

// MarshalJingle encodes one <jingle/> payload for an <iq>.
func MarshalJingle(j *Jingle) ([]byte, error) {
	return xml.Marshal(j)
}

// AudioDescription returns the audio RTP description of the first content, or nil.
func (j *Jingle) AudioDescription() *Description {
	for i := range j.Contents {
		if j.Contents[i].Description != nil && j.Contents[i].Description.Media == "audio" {
			return j.Contents[i].Description
		}
	}
	return nil
}

// AudioTransport returns the transport of the first content carrying one, or nil.
func (j *Jingle) AudioTransport() *JingleTransport {
	for i := range j.Contents {
		if j.Contents[i].Transport != nil {
			return j.Contents[i].Transport
		}
	}
	return nil
}
