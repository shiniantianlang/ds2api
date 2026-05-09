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
	// Phase 1b: strip leading * on closing tags:  </*DSML| → </|DSML|
	// Match *</DSML| where * sits between < and /, replace with </|
	text = strings.ReplaceAll(text, "</*DSML|", "</|DSML|")
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
	return result[:len(result)-1] // trim trailing newline added by last Split
}

func normalizeDSMLToolCallMarkup(text string) (string, bool) {
	if text == "" {
		return "", true
	}
	text = stripMarkdownBoldFromDSMLTags(text)
	hasAliasLikeMarkup, _ := ContainsToolMarkupSyntaxOutsideIgnored(text)
	if !hasAliasLikeMarkup {
		return text, true
	}
	return rewriteDSMLToolMarkupOutsideIgnored(text), true
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
			b.WriteString(text[tag.NameEnd : tag.End+1])
			if text[tag.End] != '>' {
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
