package toolcall

import (
	"encoding/json"
	"encoding/xml"
	"html"
	"regexp"
	"strings"
)

var xmlAttrPattern = regexp.MustCompile(`(?is)\b([a-z0-9_:-]+)\s*=\s*("([^"]*)"|'([^']*)')`)
var xmlToolCallsClosePattern = regexp.MustCompile(`(?is)</tool_calls>`)
var xmlInvokeStartPattern = regexp.MustCompile(`(?is)<invoke\b[^>]*\bname\s*=\s*("([^"]*)"|'([^']*)')`)
var cdataBRSeparatorPattern = regexp.MustCompile(`(?i)<br\s*/?>`)

func parseXMLToolCalls(text string) []ParsedToolCall {
	wrappers := findXMLElementBlocks(text, "tool_calls")
	// Some models emit <dsml_s> instead of <tool_calls> as the wrapper.
	if len(wrappers) == 0 {
		wrappers = findXMLElementBlocks(text, "dsml_s")
	}
	if len(wrappers) == 0 {
		repaired := repairMissingXMLToolCallsOpeningWrapper(text)
		if repaired != text {
			wrappers = findXMLElementBlocks(repaired, "tool_calls")
		}
	}
	if len(wrappers) == 0 {
		return nil
	}
	out := make([]ParsedToolCall, 0, len(wrappers))
	for _, wrapper := range wrappers {
		for _, block := range findXMLElementBlocks(wrapper.Body, "invoke") {
			call, ok := parseSingleXMLToolCall(block)
			if !ok {
				continue
			}
			out = append(out, call)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func repairMissingXMLToolCallsOpeningWrapper(text string) string {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<tool_calls") {
		return text
	}

	closeMatches := xmlToolCallsClosePattern.FindAllStringIndex(text, -1)
	if len(closeMatches) == 0 {
		return text
	}
	invokeLoc := xmlInvokeStartPattern.FindStringIndex(text)
	if invokeLoc == nil {
		return text
	}
	closeLoc := closeMatches[len(closeMatches)-1]
	if invokeLoc[0] >= closeLoc[0] {
		return text
	}

	return text[:invokeLoc[0]] + "<tool_calls>" + text[invokeLoc[0]:closeLoc[0]] + "</tool_calls>" + text[closeLoc[1]:]
}

func parseSingleXMLToolCall(block xmlElementBlock) (ParsedToolCall, bool) {
	attrs := parseXMLTagAttributes(block.Attrs)
	name := strings.TrimSpace(html.UnescapeString(attrs["name"]))
	if name == "" {
		return ParsedToolCall{}, false
	}

	inner := strings.TrimSpace(block.Body)
	if strings.HasPrefix(inner, "{") {
		var payload map[string]any
		if err := json.Unmarshal([]byte(inner), &payload); err == nil {
			input := map[string]any{}
			if params, ok := payload["input"].(map[string]any); ok {
				input = params
			}
			if len(input) == 0 {
				if params, ok := payload["parameters"].(map[string]any); ok {
					input = params
				}
			}
			return ParsedToolCall{Name: name, Input: input}, true
		}
	}

	input := map[string]any{}
	for _, paramMatch := range findXMLElementBlocks(inner, "parameter") {
		paramAttrs := parseXMLTagAttributes(paramMatch.Attrs)
		paramName := strings.TrimSpace(html.UnescapeString(paramAttrs["name"]))
		if paramName == "" {
			// Unnamed parameter: use parseInvokeParameterValue to handle
			// CDATA and other formats, then merge into call input.
			value := parseInvokeParameterValue("", paramMatch.Body)
			if m, ok := value.(map[string]any); ok {
				for k, v := range m {
					input[k] = v
				}
			}
			continue
		}
		value := parseInvokeParameterValue(paramName, paramMatch.Body)
		appendMarkupValue(input, paramName, value)
	}

	if len(input) == 0 {
		if strings.TrimSpace(inner) != "" {
			return ParsedToolCall{}, false
		}
		return ParsedToolCall{Name: name, Input: map[string]any{}}, true
	}
	return ParsedToolCall{Name: name, Input: input}, true
}

type xmlElementBlock struct {
	Attrs string
	Body  string
	Start int
	End   int
}

func findXMLElementBlocks(text, tag string) []xmlElementBlock {
	if text == "" || tag == "" {
		return nil
	}
	var out []xmlElementBlock
	pos := 0
	for pos < len(text) {
		start, bodyStart, attrs, ok := findXMLStartTagOutsideCDATA(text, tag, pos)
		if !ok {
			break
		}
		closeStart, closeEnd, ok := findMatchingXMLEndTagOutsideCDATA(text, tag, bodyStart)
		if !ok {
			pos = bodyStart
			continue
		}
		out = append(out, xmlElementBlock{
			Attrs: attrs,
			Body:  text[bodyStart:closeStart],
			Start: start,
			End:   closeEnd,
		})
		pos = closeEnd
	}
	return out
}

func findXMLStartTagOutsideCDATA(text, tag string, from int) (start, bodyStart int, attrs string, ok bool) {
	lower := strings.ToLower(text)
	target := "<" + strings.ToLower(tag)
	for i := maxInt(from, 0); i < len(text); {
		next, advanced, blocked := skipXMLIgnoredSection(text, i)
		if blocked {
			return -1, -1, "", false
		}
		if advanced {
			i = next
			continue
		}
		if strings.HasPrefix(lower[i:], target) && hasXMLTagBoundary(text, i+len(target)) {
			end := findXMLTagEnd(text, i+len(target))
			if end < 0 {
				return -1, -1, "", false
			}
			return i, end + 1, text[i+len(target) : end], true
		}
		i++
	}
	return -1, -1, "", false
}

func findMatchingXMLEndTagOutsideCDATA(text, tag string, from int) (closeStart, closeEnd int, ok bool) {
	lower := strings.ToLower(text)
	openTarget := "<" + strings.ToLower(tag)
	closeTarget := "</" + strings.ToLower(tag)
	depth := 1
	for i := maxInt(from, 0); i < len(text); {
		next, advanced, blocked := skipXMLIgnoredSection(text, i)
		if blocked {
			return -1, -1, false
		}
		if advanced {
			i = next
			continue
		}
		if strings.HasPrefix(lower[i:], closeTarget) && hasXMLTagBoundary(text, i+len(closeTarget)) {
			end := findXMLTagEnd(text, i+len(closeTarget))
			if end < 0 {
				return -1, -1, false
			}
			depth--
			if depth == 0 {
				return i, end + 1, true
			}
			i = end + 1
			continue
		}
		if strings.HasPrefix(lower[i:], openTarget) && hasXMLTagBoundary(text, i+len(openTarget)) {
			end := findXMLTagEnd(text, i+len(openTarget))
			if end < 0 {
				return -1, -1, false
			}
			if !isSelfClosingXMLTag(text[:end]) {
				depth++
			}
			i = end + 1
			continue
		}
		i++
	}
	return -1, -1, false
}

func skipXMLIgnoredSection(text string, i int) (next int, advanced bool, blocked bool) {
	if i < 0 || i >= len(text) {
		return i, false, false
	}
	switch {
	case hasASCIIPrefixFoldAt(text, i, "<![cdata["):
		end := findToolCDATAEnd(text, i+len("<![cdata["))
		if end < 0 {
			return 0, false, true
		}
		return end + len("]]>"), true, false
	case strings.HasPrefix(text[i:], "<!--"):
		end := strings.Index(text[i+len("<!--"):], "-->")
		if end < 0 {
			return 0, false, true
		}
		return i + len("<!--") + end + len("-->"), true, false
	default:
		return i, false, false
	}
}

func hasASCIIPrefixFoldAt(text string, start int, prefix string) bool {
	if start < 0 || len(text)-start < len(prefix) {
		return false
	}
	for j := 0; j < len(prefix); j++ {
		if asciiLower(text[start+j]) != asciiLower(prefix[j]) {
			return false
		}
	}
	return true
}

func asciiLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func findToolCDATAEnd(text string, from int) int {
	if from < 0 || from >= len(text) {
		return -1
	}
	const closeMarker = "]]>"
	firstNonFenceEnd := -1
	for searchFrom := from; searchFrom < len(text); {
		rel := strings.Index(text[searchFrom:], closeMarker)
		if rel < 0 {
			break
		}
		end := searchFrom + rel
		searchFrom = end + len(closeMarker)
		if cdataOffsetIsInsideMarkdownFence(text[from:end]) {
			continue
		}
		if cdataEndLooksStructural(text, searchFrom) {
			return end
		}
		if firstNonFenceEnd < 0 {
			firstNonFenceEnd = end
		}
	}
	return firstNonFenceEnd
}

func cdataEndLooksStructural(text string, after int) bool {
	for after < len(text) {
		switch {
		case text[after] == ' ' || text[after] == '\t' || text[after] == '\r' || text[after] == '\n':
			after++
		case after+1 < len(text) && text[after] == '<' && text[after+1] == '/':
			return true
		default:
			return false
		}
	}
	return false
}

func cdataOffsetIsInsideMarkdownFence(fragment string) bool {
	if fragment == "" {
		return false
	}
	lines := strings.SplitAfter(fragment, "\n")
	inFence := false
	fenceMarker := ""
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !inFence {
			if marker, ok := parseFenceOpen(trimmed); ok {
				inFence = true
				fenceMarker = marker
			}
			continue
		}
		if isFenceClose(trimmed, fenceMarker) {
			inFence = false
			fenceMarker = ""
		}
	}
	return inFence
}

func findXMLTagEnd(text string, from int) int {
	quote := byte(0)
	for i := maxInt(from, 0); i < len(text); i++ {
		ch := text[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '>' {
			return i
		}
	}
	return -1
}

func hasXMLTagBoundary(text string, idx int) bool {
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

func isSelfClosingXMLTag(startTag string) bool {
	return strings.HasSuffix(strings.TrimSpace(startTag), "/")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseXMLTagAttributes(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, m := range xmlAttrPattern.FindAllStringSubmatch(raw, -1) {
		if len(m) < 5 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(m[1]))
		if key == "" {
			continue
		}
		value := m[3]
		if value == "" {
			value = m[4]
		}
		out[key] = value
	}
	return out
}

func parseInvokeParameterValue(paramName, raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if value, ok := extractStandaloneCDATA(trimmed); ok {
		if parsed, ok := parseJSONLiteralValue(value); ok {
			if parsedArray, ok := coerceArrayValue(parsed, paramName); ok {
				return parsedArray
			}
			return parsed
		}
		if parsed, ok := parseStructuredCDATAParameterValue(paramName, value); ok {
			return parsed
		}
		if parsed, ok := parseLooseJSONArrayValue(value, paramName); ok {
			return parsed
		}
		return value
	}
	decoded := html.UnescapeString(extractRawTagValue(trimmed))
	if strings.Contains(decoded, "<") && strings.Contains(decoded, ">") {
		if parsedValue, ok := parseXMLFragmentValue(decoded); ok {
			switch v := parsedValue.(type) {
			case map[string]any:
				if len(v) > 0 {
					if parsedArray, ok := coerceArrayValue(v, paramName); ok {
						return parsedArray
					}
					return v
				}
			case []any:
				return v
			case string:
				text := strings.TrimSpace(v)
				if text == "" {
					return ""
				}
				if parsedText, ok := parseJSONLiteralValue(text); ok {
					if parsedArray, ok := coerceArrayValue(parsedText, paramName); ok {
						return parsedArray
					}
					return parsedText
				}
				if parsedText, ok := parseLooseJSONArrayValue(text, paramName); ok {
					return parsedText
				}
				return v
			default:
				return v
			}
		}
		if parsed := parseStructuredToolCallInput(decoded); len(parsed) > 0 {
			if len(parsed) == 1 {
				if rawValue, ok := parsed["_raw"].(string); ok {
					if parsedText, ok := parseLooseJSONArrayValue(rawValue, paramName); ok {
						return parsedText
					}
					return rawValue
				}
			}
			if parsedArray, ok := coerceArrayValue(parsed, paramName); ok {
				return parsedArray
			}
			return parsed
		}
	}
	if parsed, ok := parseJSONLiteralValue(decoded); ok {
		if parsedArray, ok := coerceArrayValue(parsed, paramName); ok {
			return parsedArray
		}
		return parsed
	}
	if parsed, ok := parseLooseJSONArrayValue(decoded, paramName); ok {
		return parsed
	}
	return decoded
}

func parseStructuredCDATAParameterValue(paramName, raw string) (any, bool) {
	if preservesCDATAStringParameter(paramName) {
		return nil, false
	}
	normalized := normalizeCDATAForStructuredParse(raw)
	if !strings.Contains(normalized, "<") || !strings.Contains(normalized, ">") {
		return nil, false
	}
	if !cdataFragmentLooksExplicitlyStructured(normalized) {
		return nil, false
	}
	parsed, ok := parseXMLFragmentValue(normalized)
	if !ok {
		return nil, false
	}
	switch v := parsed.(type) {
	case []any:
		return v, true
	case map[string]any:
		if len(v) == 0 {
			return nil, false
		}
		return v, true
	default:
		return nil, false
	}
}

func normalizeCDATAForStructuredParse(raw string) string {
	if raw == "" {
		return ""
	}
	normalized := cdataBRSeparatorPattern.ReplaceAllString(raw, "\n")
	return html.UnescapeString(strings.TrimSpace(normalized))
}

// Preserve flat CDATA fragments as strings. Only recover structure when the
// fragment clearly encodes a data shape: multiple sibling elements, nested
// child elements, or an explicit item list.
func cdataFragmentLooksExplicitlyStructured(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}

	dec := xml.NewDecoder(strings.NewReader("<root>" + trimmed + "</root>"))
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	start, ok := tok.(xml.StartElement)
	if !ok || !strings.EqualFold(start.Name.Local, "root") {
		return false
	}

	depth := 0
	directChildren := 0
	firstChildName := ""
	firstChildHasNested := false

	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				directChildren++
				if directChildren == 1 {
					firstChildName = strings.ToLower(strings.TrimSpace(t.Name.Local))
				} else {
					return true
				}
			} else if directChildren == 1 && depth == 1 {
				firstChildHasNested = true
			}
			depth++
		case xml.EndElement:
			if strings.EqualFold(t.Name.Local, "root") {
				if directChildren != 1 {
					return false
				}
				if firstChildName == "item" {
					return true
				}
				return firstChildHasNested
			}
			if depth > 0 {
				depth--
			}
		}
	}
}

func preservesCDATAStringParameter(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content", "file_content", "text", "prompt", "query", "command", "cmd", "script", "code", "old_string", "new_string", "pattern", "path", "file_path":
		return true
	default:
		return false
	}
}
