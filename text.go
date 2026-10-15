package main

import (
	"strings"
	"unicode"
)

// Names and titles are shown to other people (shared lists, sharing
// picker, account list). Invisible characters must not change how they
// look: "\u202Enimda" renders as "admin", "Ad\u200bmin" looks like "Admin"
// but is a different name.

// bidiOverride: embeddings, overrides and isolates — they reorder the text
// after them.
func bidiOverride(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// invisible: characters without any width of their own, or blank fillers
// that look like nothing (Hangul fillers). ZWJ/ZWNJ (U+200D, U+200C) and the
// tag characters stay: emoji sequences (flags too) and some scripts need them.
func invisible(r rune) bool {
	switch r {
	case 0x200B, 0x2060, 0xFEFF, 0x00AD, 0x034F, 0x180E, 0x115F, 0x3164, 0xFFA0:
		return true
	}
	return (r >= 0x2061 && r <= 0x2064) || (r >= 0x206A && r <= 0x206F)
}

func lineBreakLike(r rune) bool { return r == 0x2028 || r == 0x2029 }

// cleanName: account, list and passkey names — additionally without the
// directional marks LRM/RLM/ALM.
func cleanName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || bidiOverride(r) || invisible(r) || lineBreakLike(r) ||
			r == 0x200E || r == 0x200F || r == 0x061C {
			return -1
		}
		return r
	}, s)
}

// cleanTitle: task titles — one line, no overrides.
func cleanTitle(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || bidiOverride(r) || invisible(r) || lineBreakLike(r) {
			return -1
		}
		return r
	}, s)
}

// cleanNote: like cleanTitle, but line breaks and tabs stay.
func cleanNote(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if lineBreakLike(r) {
			return '\n'
		}
		if unicode.IsControl(r) || bidiOverride(r) {
			return -1
		}
		return r
	}, s)
}
