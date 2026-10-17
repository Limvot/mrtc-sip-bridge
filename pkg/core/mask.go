package core

// MaskUsername returns "ab***yz" (first two and last two runes); "***" if 4 runes or fewer.
// It lives in core (the leaf of the library) so every package can mask SIP usernames
// without depending on pkg/sipua.
func MaskUsername(u string) string {
	r := []rune(u)
	if len(r) <= 4 {
		return "***"
	}
	return string(r[:2]) + "***" + string(r[len(r)-2:])
}
