package toolcall

import (
	"regexp"
	"strings"
)

// dsmlMarkdownBoldRe matches DSML tags wrapped in Markdown bold markers
// that some models (notably DeepSeek-V4) emit, e.g.
//
//	<**DSML|tool_calls**>  →  <|DSML|tool_calls>
//	</**DSML|invoke**>     →  </|DSML|invoke>
//	<**DSML|parameter name="code"**>  →  <|DSML|parameter name="code">
//
// Pattern 1: opening tags — ** before DSML| → strip **, emit |DSML|
// Pattern 2: closing tags — * before /DSML| → strip *
var dsmlMarkdownBoldOpenRe = regexp.MustCompile(`\*{1,2}(DSML\|)`)

func stripMarkdownBoldFromDSMLTags(text string) string {
	if !strings.Contains(text, "*DSML|") {
		return text
	}
	// Phase 1a: strip leading ** on opening tags:  **DSML| → |DSML|
	text = dsmlMarkdownBoldOpenRe.ReplaceAllString(text, "|$1")
	// Phase 1b: strip leading * on closing tags.
	// Both </*DSML| and <*/DSML| appear in the wild; normalize to </|DSML|
	text = strings.ReplaceAll(text, "<*/DSML|", "</|DSML|")
	text = strings.ReplaceAll(text, "</*DSML|", "</|DSML|")
	text = strings.ReplaceAll(text, "<**/DSML|", "</|DSML|")
	text = strings.ReplaceAll(text, "</**DSML|", "</|DSML|")
	// Phase 2: strip trailing ** before > on DSML lines.
	// We only target lines that contain "DSML|" to avoid touching unrelated bold text.
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "DSML|") {
			line = strings.ReplaceAll(line, "**>", ">")
			line = strings.ReplaceAll(line, "*>", ">")
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	result := b.String()
	result = result[:len(result)-1] // trim trailing newline added by last Split

	// Phase 3: flatten nested bare parameter tags.
	// Some models emit:  <|DSML|parameter name="x"><|DSML|parameter>value</|DSML|parameter>
	// The inner bare <|DSML|parameter> has no attributes and must be removed so the parser
	// sees:  <|DSML|parameter name="x">value</|DSML|parameter>
	bareParamOpen := regexp.MustCompile(`<(?:\|)?DSML\|parameter>`)
	result = bareParamOpen.ReplaceAllString(result, "")

	return result
}

func normalizeDSMLToolCallMarkup(text string) (string, bool) {
	if text == "" {
		return "", true
	}
	// Normalize full-width characters that some models emit:
	//   ｜ (U+FF5C, full-width vertical line) → | (U+007C)
	//   ＜ (U+FF1C, full-width less-than)     → < (U+003C)
	//   ＞ (U+FF1E, full-width greater-than)  → > (U+003E)
	//   ／ (U+FF0F, full-width solidus)       → / (U+002F)
	text = strings.ReplaceAll(text, "｜", "|")
	text = strings.ReplaceAll(text, "＜", "<")
	text = strings.ReplaceAll(text, "＞", ">")
	text = strings.ReplaceAll(text, "／", "/")

	// ▐ (U+2590) has dual meaning:
	//   - DSML▐tool_calls → separator (|), when followed by a letter
	//   - name="x"▐\n     → tag close (>), when followed by <, \n, or end of string
	text = normalizeTrailingBlockChars(text, "\u2590", "|", ">")

	// ▁ (U+2581) similarly has dual meaning:
	//   - tool▁calls → underscore (_), inside tag names
	//   - tag▁\n     → tag close (>), at line end or before <
	text = normalizeTrailingBlockChars(text, "\u2581", "_", ">")

	text = stripMarkdownBoldFromDSMLTags(text)
	hasAliasLikeMarkup, _ := ContainsToolMarkupSyntaxOutsideIgnored(text)
	if !hasAliasLikeMarkup {
		return text, true
	}
	return rewriteDSMLToolMarkupOutsideIgnored(text), true
}

// normalizeTrailingBlockChars replaces a block-drawing character that has dual
// meaning in DSML markup. When the character appears before <, \n, or end of
// string it acts as a missing closing angle bracket (tagClose). All remaining
// instances are replaced by defaultCh (e.g. "|" for ▐, "_" for ▁).
func normalizeTrailingBlockChars(text, ch, defaultCh, tagClose string) string {
	if !strings.Contains(text, ch) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + len(text)/4)
	i := 0
	for i < len(text) {
		if strings.HasPrefix(text[i:], ch) {
			// Look at the character after this instance.
			after := i + len(ch)
			if after >= len(text) || text[after] == '<' || text[after] == '\n' {
				b.WriteString(tagClose)
			} else {
				b.WriteString(defaultCh)
			}
			i += len(ch)
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func rewriteDSMLToolMarkupOutsideIgnored(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		next, advanced, blocked := skipXMLIgnoredSection(text, i)
		if blocked {
			b.WriteString(text[i:])
			break
		}
		if advanced {
			b.WriteString(text[i:next])
			i = next
			continue
		}
		tag, ok := scanToolMarkupTagAt(text, i)
		if !ok {
			b.WriteByte(text[i])
			i++
			continue
		}
		if tag.DSMLLike {
			b.WriteByte('<')
			if tag.Closing {
				b.WriteByte('/')
			}
			b.WriteString(tag.Name)
			suffix := text[tag.NameEnd : tag.End+1]
			// Strip trailing markdown bold/italic markers before the closing '>'.
			// e.g. suffix = ` name="foo"**>` → ` name="foo">`
			if idx := strings.LastIndex(suffix, ">"); idx >= 0 {
				before := suffix[:idx]
				for len(before) > 0 && before[len(before)-1] == '*' {
					before = before[:len(before)-1]
				}
				suffix = before + suffix[idx:]
			}
			b.WriteString(suffix)
			if text[tag.End] != '>' && !strings.HasSuffix(suffix, ">") {
				b.WriteByte('>')
			}
			i = tag.End + 1
			continue
		}
		b.WriteString(text[tag.Start : tag.End+1])
		i = tag.End + 1
	}
	return b.String()
}
