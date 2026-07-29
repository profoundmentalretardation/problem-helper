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
// The table is the whole of the fix: shield.Strip sanitizes before it strips
// comments precisely so an invisible character wedged into an opener
// (`/<U+200B>/ payload`) cannot hide it from the byte-matching scanners — and
// that only holds for codepoints listed here. Every gap is the same bypass at
// a different codepoint.
var invisibleRanges = []invisibleRange{
	// C0 controls other than tab, CR and LF, plus DEL and the C1 block.
	// Non-printing, and none of them is meaningful in source outside a
	// literal; leaving them through let them sit inside a comment opener.
	{0x0000, 0x0008, "C0 control"},
	{0x000B, 0x000C, "C0 control"},
	{0x000E, 0x001F, "C0 control"},
	{0x007F, 0x009F, "delete / C1 control"},
	{0x00AD, 0x00AD, "soft hyphen"},
	{0x034F, 0x034F, "combining grapheme joiner"},
	{0x115F, 0x1160, "hangul choseong/jungseong filler"},
	{0x061C, 0x061C, "arabic letter mark (bidi control)"},
	{0x180E, 0x180E, "mongolian vowel separator"},
	{0x200B, 0x200F, "zero-width/directional mark"},
	{0x202A, 0x202E, "bidi embedding/override control"},
	{0x2060, 0x2064, "word joiner / invisible operator"},
	{0x2066, 0x2069, "bidi isolate control"},
	{0x206A, 0x206F, "deprecated formatting control"},
	{0x2800, 0x2800, "braille pattern blank"},
	{0x3164, 0x3164, "hangul filler"},
	{0xFE00, 0xFE0F, "variation selector"},
	{0xFEFF, 0xFEFF, "zero-width no-break space (BOM)"},
	{0xFFA0, 0xFFA0, "halfwidth hangul filler"},
	{0xFFF9, 0xFFFB, "interlinear annotation control"},
	{0x1D173, 0x1D17A, "musical symbol format control"},
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
