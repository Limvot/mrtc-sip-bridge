package calls

import (
	"fmt"
	"strconv"
	"time"
)

// Timeline notice texts (PHASE2-SPEC §8.8). They never contain the number (the room identifies it).
const (
	noticeMissed      = "Missed call"
	noticeDeclined    = "Declined call"
	noticeBusy        = "Missed call (line busy)"
	noticeCodec       = "Missed call (unsupported audio codec)"
	noticeNoAudio     = "Missed call (could not connect audio)"
	noticeEncrypted   = "Missed call: calls need an unencrypted room. This room is encrypted, so the call was sent to voicemail."
	noticeAnsweredFmt = "Call · %s" // formatDuration
)

// Outbound notice texts (PHASE3-SPEC §6.6). Remote reason phrases never appear in notices.
const (
	noticeOutAnsweredFmt = "Outgoing call · %s"
	noticeOutCancelled   = "Cancelled call"
	noticeOutFailedFmt   = "Call failed: %s"
	noticeOutNotPlaced   = "Call not placed"
	noticeOutNoAnswer    = "Call not placed: not answered"
	noticeOutBusyLine    = "Call not placed: another call is in progress"
	noticeOutNotAllowed  = "Call not placed: the bridge configuration does not allow this destination"
	noticeOutSelf        = "Call not placed: that is your own number"
	noticeOutEncrypted   = "Call not placed: calls need an unencrypted room. This room is encrypted."
)

// Fixed "Call failed: <reason>" reasons that are not SIP statuses.
const (
	failReasonNoAudio       = "could not connect audio"
	failReasonNotRegistered = "SIP account is not registered"
	failReasonNotPlaced     = "could not place the call"
	failReasonNoResponse    = "the provider did not respond"
	failReasonNoAnswer      = "no answer"
	failReasonBridgeStop    = "the bridge stopped"
)

// failedNotice returns "Call failed: <reason>".
func failedNotice(reason string) string { return fmt.Sprintf(noticeOutFailedFmt, reason) }

// reasonTexts is the fixed table of SIP final statuses for notices.
var reasonTexts = map[int]string{
	400: "400 Bad Request",
	403: "403 Forbidden",
	404: "404 Not Found",
	408: "no answer (408)",
	410: "410 Gone",
	480: "480 Temporarily Unavailable",
	484: "484 Address Incomplete",
	486: "486 Busy Here",
	487: "487 Request Terminated",
	488: "488 Not Acceptable Here",
	500: "500 Server Internal Error",
	502: "502 Bad Gateway",
	503: "503 Service Unavailable",
	504: "504 Server Time-out",
	600: "600 Busy Everywhere",
	603: "603 Decline",
	604: "604 Does Not Exist Anywhere",
}

// reasonText maps a received SIP final status to a fixed notice text (never the remote reason phrase).
func reasonText(status int) string {
	if t, ok := reasonTexts[status]; ok {
		return t
	}
	return "SIP " + strconv.Itoa(status)
}

// outcomeFor maps a remote final rejection to call.outcome.
func outcomeFor(status int) string {
	switch status {
	case 486, 600:
		return outcomeBusy
	case 603:
		return outcomeDeclined
	case 408, 480:
		return outcomeTimeout
	default:
		return outcomeFailed
	}
}

// outAnsweredNotice is the notice for an answered outbound call of duration d.
func outAnsweredNotice(d time.Duration) string {
	return fmt.Sprintf(noticeOutAnsweredFmt, formatDuration(d))
}

// formatDuration: < 1 min "45s"; < 1 h "1m 20s" (seconds always shown, "2m 0s"); else "1h 02m".
// Rounds down to whole seconds; negative -> "0s".
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
	}
}

// answeredNotice is the notice for an answered call of duration d.
func answeredNotice(d time.Duration) string { return fmt.Sprintf(noticeAnsweredFmt, formatDuration(d)) }
