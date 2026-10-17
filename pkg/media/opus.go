package media

import (
	"gopkg.in/hraban/opus.v2"
)

const (
	maxOpusPacket = 1275 // largest Opus packet
	maxDecodePCM  = 960  // 120 ms at 8 kHz, the longest Opus frame
)

type opusEncoder struct{ enc *opus.Encoder }

func newOpusEncoder() (*opusEncoder, error) {
	enc, err := opus.NewEncoder(SampleRate, 1, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	if err := enc.SetBitrate(24000); err != nil {
		return nil, err
	}
	if err := enc.SetInBandFEC(true); err != nil {
		return nil, err
	}
	if err := enc.SetPacketLossPerc(5); err != nil {
		return nil, err
	}
	if err := enc.SetDTX(false); err != nil {
		return nil, err
	}
	return &opusEncoder{enc: enc}, nil
}

// encode encodes one 160-sample frame into out and returns the packet length.
func (e *opusEncoder) encode(pcm []int16, out []byte) (int, error) { return e.enc.Encode(pcm, out) }

type opusDecoder struct{ dec *opus.Decoder }

func newOpusDecoder() (*opusDecoder, error) {
	dec, err := opus.NewDecoder(SampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &opusDecoder{dec: dec}, nil
}

// decode decodes one packet into pcm (len >= 960) and returns the sample count.
func (d *opusDecoder) decode(data []byte, pcm []int16) (int, error) { return d.dec.Decode(data, pcm) }

// plc fills pcm (a whole number of 2.5 ms units) with concealment audio.
func (d *opusDecoder) plc(pcm []int16) error { return d.dec.DecodePLC(pcm) }
