package jingleleg

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// JMI actions (XEP-0353).
const (
	JMIPropose = "propose"
	JMIAccept  = "accept"
	JMIProceed = "proceed"
	JMIReject  = "reject"
	JMIRetract = "retract"
	JMIFinish  = "finish"
)

// JMI is one Jingle Message Initiation action (XEP-0353) carried in a <message/>.
type JMI struct {
	Action string // propose, accept, proceed, reject, retract, finish
	ID     string // the proposed Jingle session id
	// Descriptions lists the offered media (propose only), usually one <description media="audio"/>.
	Descriptions []Description
	// Reason is the <reason/> of retract/finish (and optionally reject), nil if absent.
	Reason *Reason
}

// ParseJMI decodes one JMI payload (the raw child XML of a <message/>). ok=false when the
// payload is not a JMI action element.
func ParseJMI(payload []byte) (j JMI, ok bool, err error) {
	var probe struct {
		XMLName      xml.Name      `xml:""`
		ID           string        `xml:"id,attr"`
		Descriptions []Description `xml:"description"`
		Reason       *Reason       `xml:"reason"`
	}
	if err := xml.Unmarshal(payload, &probe); err != nil {
		return JMI{}, false, fmt.Errorf("jingleleg: parse jmi: %w", err)
	}
	switch probe.XMLName.Local {
	case JMIPropose, JMIAccept, JMIProceed, JMIReject, JMIRetract, JMIFinish:
	default:
		return JMI{}, false, nil
	}
	// Propose rides in NSJMI0/1; accept/proceed/reject/retract in NSJMI0 (finish in NSJMI1).
	switch probe.XMLName.Space {
	case NSJMI0, NSJMI1, "":
	default:
		return JMI{}, false, nil
	}
	return JMI{
		Action:       probe.XMLName.Local,
		ID:           probe.ID,
		Descriptions: probe.Descriptions,
		Reason:       probe.Reason,
	}, true, nil
}

// Marshal encodes the JMI action as a <message/> child element.
func (j JMI) Marshal() ([]byte, error) {
	ns := NSJMI0
	if j.Action == JMIFinish {
		ns = NSJMI1
	}
	var buf bytes.Buffer
	e := xml.NewEncoder(&buf)
	start := xml.StartElement{Name: xml.Name{Space: ns, Local: j.Action}}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "id"}, Value: j.ID})
	if err := e.EncodeToken(start); err != nil {
		return nil, err
	}
	for _, d := range j.Descriptions {
		if err := e.Encode(d); err != nil {
			return nil, err
		}
	}
	if j.Reason != nil {
		if err := e.Encode(j.Reason); err != nil {
			return nil, err
		}
	}
	if err := e.EncodeToken(xml.EndElement{Name: start.Name}); err != nil {
		return nil, err
	}
	if err := e.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
