package shield

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type invisibleRange struct {
	lo, hi rune
	name   string
}

// invisibleRanges are the zero-width, bidi-control, and tag-character
// ranges usable to hide text from a human reader while a model still sees
// it — the "confusable Unicode" the plan calls out, ported from the
// injection corpus in research/03_prompt_shield.ipynb.
var invisibleRanges = []invisibleRange{
	{0x00AD, 0x00AD, "soft hyphen"},
	{0x061C, 0x061C, "arabic letter mark (bidi control)"},
	{0x180E, 0x180E, "mongolian vowel separator"},
	{0x200B, 0x200F, "zero-width/directional mark"},
	{0x202A, 0x202E, "bidi embedding/override control"},
	{0x2060, 0x2064, "word joiner / invisible operator"},
	{0x2066, 0x2069, "bidi isolate control"},
	{0x206A, 0x206F, "deprecated formatting control"},
	{0xFE00, 0xFE0F, "variation selector"},
	{0xFEFF, 0xFEFF, "zero-width no-break space (BOM)"},
	{0xFFF9, 0xFFFB, "interlinear annotation control"},
	{0xE0000, 0xE007F, "Unicode tag character"},
	{0xE0100, 0xE01EF, "variation selector supplement"},
}

func invisibleName(r rune) (string, bool) {
	for _, rg := range invisibleRanges {
		if r >= rg.lo && r <= rg.hi {
			return rg.name, true
		}
	}
	return "", false
}

// sanitizeUnicode NFC-normalizes code and strips invisible/confusable
// characters — zero-width spaces, bidi controls, the BOM, and Unicode tag
// characters — recording each one removed.
func sanitizeUnicode(code string) (string, []UnicodeRemoval) {
	normalized := norm.NFC.String(code)

	var out strings.Builder
	out.Grow(len(normalized))
	var removed []UnicodeRemoval
	for i := 0; i < len(normalized); {
		r, w := utf8.DecodeRuneInString(normalized[i:])
		// An invalid byte decodes as (RuneError, 1). Writing the decoded rune
		// back would replace it with U+FFFD: ejudge accepts raw bytes and
		// CP1251-encoded sources with Cyrillic literals are common there, so
		// every non-ASCII byte of such a submission would be rewritten — in the
		// code the model diagnoses *and* the code submitted to the judge — with
		// nothing recorded in Removed to show it happened. Pass the byte
		// through untouched instead.
		if r == utf8.RuneError && w <= 1 {
			out.WriteByte(normalized[i])
			i++
			continue
		}
		if name, bad := invisibleName(r); bad {
			removed = append(removed, UnicodeRemoval{
				Name:      name,
				Codepoint: fmt.Sprintf("U+%04X", r),
			})
			i += w
			continue
		}
		out.WriteString(normalized[i : i+w])
		i += w
	}
	return out.String(), removed
}
