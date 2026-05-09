package toolcall

import "strings"

type toolMarkupNameAlias struct {
	raw       string
	canonical string
	dsmlOnly  bool
}

var toolMarkupNames = []toolMarkupNameAlias{
	{raw: "tool_calls", canonical: "tool_calls"},
	{raw: "tool-calls", canonical: "tool_calls", dsmlOnly: true},
	{raw: "invoke", canonical: "invoke"},
	{raw: "parameter", canonical: "parameter"},
}

type ToolMarkupTag struct {
	Start       int
	End         int
	NameStart   int
	NameEnd     int
	Name        string
	Closing     bool
	SelfClosing bool
	DSMLLike    bool
	Canonical   bool
}

func ContainsToolMarkupSyntaxOutsideIgnored(text string) (hasDSML, hasCanonical bool) {
	for i := 0; i < len(text); {
		next, advanced, blocked := skipXMLIgnoredSection(text, i)
		if blocked {
			return hasDSML, hasCanonical
		}
		if advanced {
			i = next
			continue
		}
		if tag, ok := scanToolMarkupTagAt(text, i); ok {
			if tag.DSMLLike {
				hasDSML = true
			} else {
				hasCanonical = true
			}
			if hasDSML && hasCanonical {
				return true, true
			}
			i = tag.End + 1
			continue
		}
		i++
	}
	return hasDSML, hasCanonical
}

func ContainsToolCallWrapperSyntaxOutsideIgnored(text string) (hasDSML, hasCanonical bool) {
	for i := 0; i < len(text); {
		next, advanced, blocked := skipXMLIgnoredSection(text, i)
		if blocked {
			return hasDSML, hasCanonical
		}
		if advanced {
			i = next
			continue
		}
		if tag, ok := scanToolMarkupTagAt(text, i); ok {
			if tag.Name != "tool_calls" {
				i = tag.End + 1
				continue
			}
			if tag.DSMLLike {
				hasDSML = true
			} else {
				hasCanonical = true
			}
			if hasDSML && hasCanonical {
				return true, true
			}
			i = tag.End + 1
			continue
		}
		i++
	}
	return hasDSML, hasCanonical
}

func FindToolMarkupTagOutsideIgnored(text string, start int) (ToolMarkupTag, bool) {
	for i := maxInt(start, 0); i < len(text); {
		next, advanced, blocked := skipXMLIgnoredSection(text, i)
		if blocked {
			return ToolMarkupTag{}, false
		}
		if advanced {
			i = next
			continue
		}
		if tag, ok := scanToolMarkupTagAt(text, i); ok {
			return tag, true
		}
		i++
	}
	return ToolMarkupTag{}, false
}

func FindMatchingToolMarkupClose(text string, open ToolMarkupTag) (ToolMarkupTag, bool) {
	if text == "" || open.Name == "" || open.Closing || open.End >= len(text) {
		return ToolMarkupTag{}, false
	}
	depth := 1
	for pos := open.End + 1; pos < len(text); {
		tag, ok := FindToolMarkupTagOutsideIgnored(text, pos)
		if !ok {
			return ToolMarkupTag{}, false
		}
		if tag.Name != open.Name {
			pos = tag.End + 1
			continue
		}
		if tag.Closing {
			depth--
			if depth == 0 {
				return tag, true
			}
		} else if !tag.SelfClosing {
			depth++
		}
		pos = tag.End + 1
	}
	return ToolMarkupTag{}, false
}

func scanToolMarkupTagAt(text string, start int) (ToolMarkupTag, bool) {
	if start < 0 || start >= len(text) || text[start] != '<' {
		return ToolMarkupTag{}, false
	}
	lower := strings.ToLower(text)
	i := start + 1
	for i < len(text) && text[i] == '<' {
		i++
	}
	closing := false
	if i < len(text) && text[i] == '/' {
		closing = true
		i++
	}
	i, dsmlLike := consumeToolMarkupNamePrefix(lower, text, i)
	name, nameLen := matchToolMarkupName(lower, i, dsmlLike)
	if nameLen == 0 {
		return ToolMarkupTag{}, false
	}
	nameEnd := i + nameLen
	// Skip markdown bold/italic markers after the tag name (before attributes or closing >),
	// e.g. <**DSML|tool_calls**> — the ** after "tool_calls" should not break boundary detection.
	for nameEnd < len(text) && text[nameEnd] == '*' {
		nameEnd++
	}
	nameEndBeforePipes := nameEnd
	for next, ok := consumeToolMarkupPipe(text, nameEnd); ok; next, ok = consumeToolMarkupPipe(text, nameEnd) {
		nameEnd = next
	}
	hasTrailingPipe := nameEnd > nameEndBeforePipes
	if !hasToolMarkupBoundary(text, nameEnd) {
		return ToolMarkupTag{}, false
	}
	end := findXMLTagEnd(text, nameEnd)
	if end < 0 {
		if !hasTrailingPipe {
			return ToolMarkupTag{}, false
		}
		end = nameEnd - 1
	}
	if hasTrailingPipe {
		if nextLT := strings.IndexByte(text[nameEnd:], '<'); nextLT >= 0 && end >= nameEnd+nextLT {
			end = nameEnd - 1
		}
	}
	trimmed := strings.TrimSpace(text[start : end+1])
	return ToolMarkupTag{
		Start:       start,
		End:         end,
		NameStart:   i,
		NameEnd:     nameEnd,
		Name:        name,
		Closing:     closing,
		SelfClosing: strings.HasSuffix(trimmed, "/>"),
		DSMLLike:    dsmlLike,
		Canonical:   !dsmlLike,
	}, true
}

func IsPartialToolMarkupTagPrefix(text string) bool {
	if text == "" || text[0] != '<' || strings.Contains(text, ">") {
		return false
	}
	lower := strings.ToLower(text)
	i := 1
	for i < len(text) && text[i] == '<' {
		i++
	}
	if i >= len(text) {
		return true
	}
	if text[i] == '/' {
		i++
	}
	for i <= len(text) {
		if i == len(text) {
			return true
		}
		if hasToolMarkupNamePrefix(lower[i:]) {
			return true
		}
		if strings.HasPrefix("dsml", lower[i:]) {
			return true
		}
		next, ok := consumeToolMarkupNamePrefixOnce(lower, text, i)
		if !ok {
			return false
		}
		i = next
	}
	return false
}

func consumeToolMarkupNamePrefix(lower, text string, idx int) (int, bool) {
	dsmlLike := false
	for {
		next, ok := consumeToolMarkupNamePrefixOnce(lower, text, idx)
		if !ok {
			return idx, dsmlLike
		}
		idx = next
		dsmlLike = true
	}
}

func consumeToolMarkupNamePrefixOnce(lower, text string, idx int) (int, bool) {
	if next, ok := consumeToolMarkupPipe(text, idx); ok {
		return next, true
	}
	// Skip markdown bold/italic markers that some models wrap around DSML tags,
	// e.g. <**DSML|tool_calls**> or <*DSML|invoke*>.
	if idx < len(text) && text[idx] == '*' {
		return idx + 1, true
	}
	if idx < len(text) && (text[idx] == ' ' || text[idx] == '\t' || text[idx] == '\r' || text[idx] == '\n') {
		return idx + 1, true
	}
	if strings.HasPrefix(lower[idx:], "dsml") {
		next := idx + len("dsml")
		if next < len(text) && (text[next] == '-' || text[next] == '_') {
			next++
		}
		return next, true
	}
	return idx, false
}

func hasToolMarkupNamePrefix(lowerTail string) bool {
	for _, name := range toolMarkupNames {
		if strings.HasPrefix(lowerTail, name.raw) || strings.HasPrefix(name.raw, lowerTail) {
			return true
		}
	}
	return false
}

func matchToolMarkupName(lower string, start int, dsmlLike bool) (string, int) {
	for _, name := range toolMarkupNames {
		if name.dsmlOnly && !dsmlLike {
			continue
		}
		if strings.HasPrefix(lower[start:], name.raw) {
			return name.canonical, len(name.raw)
		}
	}
	return "", 0
}

func consumeToolMarkupPipe(text string, idx int) (int, bool) {
	if idx >= len(text) {
		return idx, false
	}
	if text[idx] == '|' {
		return idx + 1, true
	}
	if strings.HasPrefix(text[idx:], "｜") {
		return idx + len("｜"), true
	}
	return idx, false
}

func hasToolMarkupBoundary(text string, idx int) bool {
	if idx >= len(text) {
		return true
	}
	switch text[idx] {
	case ' ', '\t', '\n', '\r', '>', '/':
		return true
	default:
		return false
	}
}
