package phone

import (
	"errors"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		input, cc, want string
		err             error
	}{
		{"+15551234567", "1", "+15551234567", nil},
		{" +1 (555) 123-4567 ", "1", "+15551234567", nil},
		{"5551234567", "1", "+15551234567", nil},
		{"15551234567", "1", "+15551234567", nil},
		{"555.123.4567", "1", "+15551234567", nil},
		{"1-555-123-4567", "1", "+15551234567", nil},
		{"tel:+15551234567", "1", "+15551234567", nil},
		{"011 44 20 7946 0958", "1", "+442079460958", nil},
		{"0044 20 7946 0958", "1", "+442079460958", nil},
		{"+44 20 7946 0958", "1", "+442079460958", nil},
		{"020 7946 0958", "44", "+442079460958", nil},
		{"2079460958", "44", "+442079460958", nil},
		{"+15551234567", "44", "+15551234567", nil},
		{"611", "1", "", ErrShortCode},
		{"123456", "1", "", ErrShortCode},
		{"1234567", "1", "", ErrInvalid},
		{"555123456", "1", "", ErrInvalid},
		{"25551234567", "1", "", ErrInvalid},
		{"5551234567", "", "", ErrInvalid},
		{"", "1", "", ErrInvalid},
		{"+", "1", "", ErrInvalid},
		{"+0123456789", "1", "", ErrInvalid},
		{"+1234567890123456", "1", "", ErrInvalid},
		{"555-CALL-NOW", "1", "", ErrInvalid},
		{"1+5551234567", "1", "", ErrInvalid},
		{"anonymous", "1", "", ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.input+"/"+tt.cc, func(t *testing.T) {
			got, err := Normalize(tt.input, tt.cc)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("Normalize(%q, %q) error = %v, want %v", tt.input, tt.cc, err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Normalize(%q, %q) = %q, %v; want %q", tt.input, tt.cc, got, err, tt.want)
			}
		})
	}
}

func TestIsE164(t *testing.T) {
	tests := map[string]bool{
		"+15551234567":      true,
		"+1234567":          true,
		"+123456":           false,
		"+123456789012345":  true,
		"+1234567890123456": false,
		"+0123456789":       false,
		"15551234567":       false,
		"+1555123456a":      false,
		"":                  false,
		"+":                 false,
	}
	for in, want := range tests {
		if got := IsE164(in); got != want {
			t.Errorf("IsE164(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDigitsFromDigitsRoundTrip(t *testing.T) {
	if d := Digits("+15551234567"); d != "15551234567" {
		t.Fatalf("Digits = %q", d)
	}
	e, ok := FromDigits("15551234567")
	if !ok || e != "+15551234567" {
		t.Fatalf("FromDigits = %q, %v", e, ok)
	}
	for _, bad := range []string{"", "abc", "0123456789", "123"} {
		if e, ok := FromDigits(bad); ok || e != "" {
			t.Errorf("FromDigits(%q) = %q, %v; want \"\", false", bad, e, ok)
		}
	}
}
