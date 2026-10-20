// Package phone normalizes phone numbers to E.164. It is a reimplementation, with fixes, of the
// Cheogram number handling in jmpchat-matrix-bridge (cheogram.go).
package phone

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalid is returned (wrapped with the input) for anything that is not a valid phone number.
	ErrInvalid = errors.New("phone: not a valid phone number")
	// ErrShortCode is returned for short codes such as "611", which are not E.164 numbers.
	ErrShortCode = errors.New("phone: short code, not an E.164 number")
)

// Normalize converts user or SIP input to E.164 ("+" followed by 7-15 digits, first digit 1-9).
// defaultCC is the default country calling code as digits ("1", "44"); "" means national numbers are rejected.
func Normalize(input, defaultCC string) (string, error) {
	s := strings.TrimSpace(input)
	if len(s) >= 4 && strings.EqualFold(s[:4], "tel:") {
		s = s[4:]
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '-', '.', '(', ')':
			return -1
		}
		return r
	}, s)

	var result string
	if strings.HasPrefix(s, "+") {
		if !allDigits(s[1:]) {
			return "", invalid(input)
		}
		result = s
	} else {
		if s == "" || !allDigits(s) {
			return "", invalid(input)
		}
		switch {
		case defaultCC == "1" && strings.HasPrefix(s, "011"):
			result = "+" + s[3:]
		case strings.HasPrefix(s, "00"):
			result = "+" + s[2:]
		case len(s) <= 6:
			return "", fmt.Errorf("%w: %q", ErrShortCode, input)
		case defaultCC == "":
			return "", invalid(input)
		case defaultCC == "1":
			switch {
			case len(s) == 10:
				result = "+1" + s
			case len(s) == 11 && s[0] == '1':
				result = "+" + s
			default:
				return "", invalid(input)
			}
		default:
			result = "+" + defaultCC + strings.TrimPrefix(s, "0")
		}
	}
	if !IsE164(result) {
		return "", invalid(input)
	}
	return result, nil
}

func invalid(input string) error { return fmt.Errorf("%w: %q", ErrInvalid, input) }

// allDigits reports whether s consists only of ASCII digits (vacuously true for "").
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// IsE164 reports whether s matches ^\+[1-9][0-9]{6,14}$.
func IsE164(s string) bool {
	if len(s) < 8 || len(s) > 16 || s[0] != '+' || s[1] == '0' {
		return false
	}
	return allDigits(s[1:])
}

// Digits strips the leading "+" ("+15551234567" -> "15551234567"). Input is assumed E.164.
func Digits(e164 string) string { return strings.TrimPrefix(e164, "+") }

// FromDigits is the inverse of Digits: returns ("+"+d, true) if that is valid E.164, else ("", false).
func FromDigits(d string) (string, bool) {
	e := "+" + d
	if !IsE164(e) {
		return "", false
	}
	return e, true
}
