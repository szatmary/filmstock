package wikitext

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/szatmary/filmstock"
)

// FindTemplate locates the first template whose name matches `name` (case
// variations on the first letter allowed) and returns the raw inner text
// between the opening "{{" and its matching "}}", handling nested braces.
// Returns the inner body (excluding the leading "{{name") and true if found.
func FindTemplate(text, name string) (string, bool) {
	lower := strings.ToLower(text)
	target := "{{" + strings.ToLower(name)
	idx := 0
	for {
		rel := strings.Index(lower[idx:], target)
		if rel < 0 {
			return "", false
		}
		start := idx + rel
		// Character immediately after the name must be a boundary so that
		// "Infobox film" does not match "Infobox filmmaker".
		after := start + len(target)
		if after < len(text) {
			c := text[after]
			if c != ' ' && c != '\n' && c != '\t' && c != '|' && c != '}' && c != '<' {
				idx = start + 2
				continue
			}
		}
		// Walk forward from `start` tracking brace depth.
		depth := 0
		i := start
		for i < len(text)-1 {
			if text[i] == '{' && text[i+1] == '{' {
				depth++
				i += 2
				continue
			}
			if text[i] == '}' && text[i+1] == '}' {
				depth--
				i += 2
				if depth == 0 {
					// inner body starts right after "{{" ; strip the name too
					body := text[start+len(target) : i-2]
					return body, true
				}
				continue
			}
			i++
		}
		return "", false
	}
}

// FindTemplateExact is like FindTemplate but requires the template name to be
// followed (after optional whitespace) by "|" or "}" — so "Infobox television"
// does NOT match "Infobox television episode" or "…season".
func FindTemplateExact(text, name string) (string, bool) {
	lower := strings.ToLower(text)
	target := "{{" + strings.ToLower(name)
	idx := 0
	for {
		rel := strings.Index(lower[idx:], target)
		if rel < 0 {
			return "", false
		}
		start := idx + rel
		j := start + len(target)
		for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
			j++
		}
		if j < len(text) && (text[j] == '|' || text[j] == '}') {
			if body, end := BalancedBody(text, start, len(target)); end >= 0 {
				return body, true
			}
			return "", false
		}
		idx = start + 2
	}
}

// FindAllTemplates returns the inner bodies (text after the template name) of
// every template whose name matches `name`, handling nested braces. Used to pull
// all {{Episode list}} rows from an article.
func FindAllTemplates(text, name string) []string {
	var out []string
	lower := strings.ToLower(text)
	target := "{{" + strings.ToLower(name)
	idx := 0
	for {
		rel := strings.Index(lower[idx:], target)
		if rel < 0 {
			break
		}
		start := idx + rel
		depth, i, end := 0, start, -1
		for i < len(text)-1 {
			if text[i] == '{' && text[i+1] == '{' {
				depth++
				i += 2
				continue
			}
			if text[i] == '}' && text[i+1] == '}' {
				depth--
				i += 2
				if depth == 0 {
					end = i
					break
				}
				continue
			}
			i++
		}
		if end < 0 {
			break
		}
		out = append(out, text[start+len(target):end-2])
		idx = end
	}
	return out
}

// splitParams splits a template body into top-level "|"-separated parameters,
// ignoring pipes nested inside {{...}}, [[...]], {|...|} tables, and <ref>...</ref>.
func splitParams(body string) []string {
	var params []string
	var cur strings.Builder
	braceDepth := 0 // {{ }}
	linkDepth := 0  // [[ ]]
	tableDepth := 0 // {| |}
	i := 0
	for i < len(body) {
		// two-char tokens
		if i+1 < len(body) {
			two := body[i : i+2]
			switch two {
			case "{{":
				braceDepth++
				cur.WriteString(two)
				i += 2
				continue
			case "}}":
				if braceDepth > 0 {
					braceDepth--
				}
				cur.WriteString(two)
				i += 2
				continue
			case "[[":
				linkDepth++
				cur.WriteString(two)
				i += 2
				continue
			case "]]":
				if linkDepth > 0 {
					linkDepth--
				}
				cur.WriteString(two)
				i += 2
				continue
			case "{|":
				tableDepth++
				cur.WriteString(two)
				i += 2
				continue
			case "|}":
				if tableDepth > 0 {
					tableDepth--
					cur.WriteString(two)
					i += 2
					continue
				}
			}
		}
		if body[i] == '|' && braceDepth == 0 && linkDepth == 0 && tableDepth == 0 {
			params = append(params, cur.String())
			cur.Reset()
			i++
			continue
		}
		cur.WriteByte(body[i])
		i++
	}
	params = append(params, cur.String())
	return params
}

// ParseInfobox returns a map of key -> raw value for the template body.
func ParseInfobox(body string) map[string]string {
	out := make(map[string]string)
	for _, p := range splitParams(body) {
		eq := strings.Index(p, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(p[:eq])
		val := strings.TrimSpace(p[eq+1:])
		if key == "" {
			continue
		}
		out[strings.ToLower(key)] = val
	}
	return out
}

var (
	reRef      = regexp.MustCompile(`(?is)<ref[^>]*>.*?</ref>`)
	reRefSelf  = regexp.MustCompile(`(?is)<ref[^>]*/>`)
	ReComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHTMLTag  = regexp.MustCompile(`(?is)<[^>]+>`)
	reWikiLink = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	reBold     = regexp.MustCompile(`'''?`)
	reNbsp     = regexp.MustCompile(`&nbsp;`)
	reWS       = regexp.MustCompile(`[ \t]+`)
)

// CleanText reduces a wikitext fragment to readable plain text.
func CleanText(s string) string {
	s = ReComment.ReplaceAllString(s, "")
	// Self-closing refs MUST be stripped first. <ref name="x"/> also matches
	// reRef's opening tag (its [^>]* happily consumes ` name="x"/`), so reRef
	// would treat it as an opening <ref> and delete everything up to the next
	// genuine </ref>. That silently destroyed most of the body text of ~8% of
	// articles — Harry Potter (film) went from 158k chars to 10k, leaving only
	// the infobox and the trailing categories.
	s = reRefSelf.ReplaceAllString(s, "")
	s = reRef.ReplaceAllString(s, "")
	// [[target|display]] -> display ; [[target]] -> target
	s = reWikiLink.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-2]
		if bar := strings.LastIndex(inner, "|"); bar >= 0 {
			return inner[bar+1:]
		}
		return inner
	})
	// strip common wrapper templates but keep their content-ish payload
	s = stripSimpleTemplates(s)
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = reNbsp.ReplaceAllString(s, " ")
	s = reBold.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = reWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// stripSimpleTemplates removes leftover {{...}} wrappers, keeping a best-effort
// text payload for a few known list templates.
func stripSimpleTemplates(s string) string {
	for {
		open := strings.Index(s, "{{")
		if open < 0 {
			break
		}
		// find matching close
		depth := 0
		i := open
		end := -1
		for i < len(s)-1 {
			if s[i] == '{' && s[i+1] == '{' {
				depth++
				i += 2
				continue
			}
			if s[i] == '}' && s[i+1] == '}' {
				depth--
				i += 2
				if depth == 0 {
					end = i
					break
				}
				continue
			}
			i++
		}
		if end < 0 {
			break
		}
		inner := s[open+2 : end-2]
		s = s[:open] + templatePayload(inner) + s[end:]
	}
	return s
}

// tplName canonicalises a template name for alias matching. MediaWiki treats
// "_" as a space and ignores case, and editors freely use both the spaced and
// unspaced redirects ("Plain list" and "Plainlist" are one template), so squash
// all of that away.
func tplName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '_' || r == '\t' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// listTemplates are the list wrappers whose parameters are the list items, in
// canonical tplName form. Anything absent yields an empty payload, which
// silently deletes the field — the "plain list" spelling alone cost ~3.3k films
// their entire cast — so add aliases here rather than trimming this set.
var listTemplates = map[string]bool{
	"plainlist": true, "flatlist": true, "hlist": true, "horizontallist": true,
	"ubl": true, "ublist": true, "ulist": true, "unbulletedlist": true,
	"bulletedlist": true, "blist": true, "collapsiblelist": true, "pl": true,
}

// templatePayload extracts a human-readable payload from a template's inner text
// for list-style templates; otherwise returns "".
func templatePayload(inner string) string {
	parts := splitParams(inner)
	if len(parts) == 0 {
		return ""
	}
	name := tplName(parts[0])
	switch {
	case listTemplates[name]:
		// remaining params (or the list body) are items
		var items []string
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if strings.Contains(p, "=") { // skip named args like class=
				if i := strings.Index(p, "="); i >= 0 && !strings.Contains(p[:i], "*") {
					continue
				}
			}
			for _, line := range strings.Split(p, "*") {
				line = strings.TrimSpace(line)
				if line != "" {
					items = append(items, line)
				}
			}
		}
		return strings.Join(items, "\n")
	case name == "ill" || name == "illm" || name == "interlanguagelink" ||
		name == "interlanguagelinkmulti":
		// {{ill|Name|lang|Foreign title}} links a person with no English article.
		// Keep the displayed English name; without this the whole credit is lost.
		for _, p := range parts[1:] {
			if lt := strings.TrimPrefix(strings.TrimSpace(p), "lt="); lt != strings.TrimSpace(p) {
				return strings.TrimSpace(lt)
			}
		}
		if len(parts) >= 2 {
			return strings.TrimSpace(parts[1])
		}
	case strings.HasPrefix(name, "sortname"):
		// {{sortname|First|Last|...}} -> "First Last" (a person name, common in
		// episode writer/director fields).
		if len(parts) >= 3 {
			return strings.TrimSpace(parts[1]) + " " + strings.TrimSpace(parts[2])
		}
		if len(parts) >= 2 {
			return strings.TrimSpace(parts[1])
		}
	case strings.HasPrefix(name, "nowrap"), strings.HasPrefix(name, "nobr"):
		if len(parts) > 1 {
			return parts[1]
		}
	}
	return ""
}

var reBrTag2 = regexp.MustCompile(`(?i)<br\s*/?>`)
var reBulletLine2 = regexp.MustCompile(`(?m)^\s*\*+`)
var reAnyWikiLink = regexp.MustCompile(`\[\[[^\[\]]*\]\]`)

// expandLinkBreaks rewrites "[[A and B|Alice<br />Bob]]" into
// "[[A and B|Alice]]\n[[A and B|Bob]]". Directing and composing duos share one
// article but list both names in the link's display text, so a blind <br>->\n
// pass tears the markup in half and yields two junk credits ("[[A and B|Alice"
// and "Bob]]"). Distributing the link keeps both names AND gives them the same
// wiki identity, which is what the shared article means.
func expandLinkBreaks(s string) string {
	return reAnyWikiLink.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-2]
		bar := strings.Index(inner, "|")
		if bar < 0 {
			return m
		}
		target, disp := inner[:bar], inner[bar+1:]
		if !reBrTag2.MatchString(disp) {
			return m
		}
		var out []string
		for _, part := range reBrTag2.Split(disp, -1) {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, "[["+target+"|"+part+"]]")
			}
		}
		if len(out) == 0 {
			return m
		}
		return strings.Join(out, "\n")
	})
}

// SplitPeople extracts person credits from a wikitext field, preserving each
// person's [[link target]] as their identity. It unwraps list templates,
// splits bullet/<br>/"X & Y"/"X and Y" lists (not inside [[...]]), strips
// "Story & Screenplay:" label prefixes, and returns {Name, Wiki} per person.
func SplitPeople(raw string) []filmstock.Person {
	s := ReComment.ReplaceAllString(raw, "")
	// Self-closing refs MUST be stripped first. <ref name="x"/> also matches
	// reRef's opening tag (its [^>]* happily consumes ` name="x"/`), so reRef
	// would treat it as an opening <ref> and delete everything up to the next
	// genuine </ref>. That silently destroyed most of the body text of ~8% of
	// articles — Harry Potter (film) went from 158k chars to 10k, leaving only
	// the infobox and the trailing categories.
	s = reRefSelf.ReplaceAllString(s, "")
	s = reRef.ReplaceAllString(s, "")
	s = stripSimpleTemplates(s) // plainlist/ubl/sortname -> items on newlines, [[links]] kept
	s = expandLinkBreaks(s)     // must precede the <br> pass, which would tear links
	s = reBrTag2.ReplaceAllString(s, "\n")
	s = reBulletLine2.ReplaceAllString(s, "\n")

	var out []filmstock.Person
	seen := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// drop a leading role label ("Story & Screenplay:") if the colon isn't in a link
		if i := strings.Index(line, ":"); i >= 0 && !strings.Contains(line[:i], "[[") {
			line = strings.TrimSpace(line[i+1:])
		}
		for _, piece := range splitAmp(line) {
			p := parseOnePerson(piece)
			if p.Name == "" || len(p.Name) > 80 {
				continue
			}
			// Dedup on (target, display name), not target alone: the same
			// article linked twice in one field is one credit, but a duo
			// article ("Jonathan Dayton and Valerie Faris") legitimately
			// supplies two distinct people under a single target.
			key := p.Wiki + "\x00" + p.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// parseOnePerson turns one segment into a Person, capturing the first [[link]]
// as the Wiki identity and the cleaned text as the display Name.
func parseOnePerson(piece string) filmstock.Person {
	piece = strings.TrimSpace(piece)
	wiki, name := "", ""
	if m := reWikiLink.FindStringSubmatch(piece); m != nil {
		// A linked credit takes its display name from INSIDE the link. Cleaning
		// the whole piece dragged in whatever surrounded it, so
		// "[[Chris Hemsworth]] (5)" became the person "Chris Hemsworth (5)" and
		// "[[Charlize Theron]], Juan Carlos Saizarbitoria, ..." became one person
		// named after four. 6,670 people carried names mangled this way.
		inner := m[1]
		disp := inner
		if bar := strings.Index(inner, "|"); bar >= 0 {
			disp = inner[bar+1:]
			inner = inner[:bar]
		}
		wiki = CanonTitle(inner)
		name = strings.TrimSpace(strings.Trim(CleanText(disp), ",;"))
	}
	if name == "" {
		// Unlinked credit: the whole piece is the name.
		name = strings.TrimSpace(strings.Trim(CleanText(piece), ",;"))
	}
	if name == "" && wiki != "" {
		name = wiki
	}
	return filmstock.Person{Name: name, Wiki: wiki}
}

// splitAmp splits on " & "/" and " at the top level (not inside [[...]]).
func splitAmp(s string) []string {
	var out []string
	depth, last, i := 0, 0, 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '[' && s[i+1] == '[' {
			depth++
			i += 2
			continue
		}
		if i+1 < len(s) && s[i] == ']' && s[i+1] == ']' {
			if depth > 0 {
				depth--
			}
			i += 2
			continue
		}
		if depth == 0 {
			if strings.HasPrefix(s[i:], " & ") {
				out = append(out, s[last:i])
				i += 3
				last = i
				continue
			}
			if strings.HasPrefix(s[i:], " and ") {
				out = append(out, s[last:i])
				i += 5
				last = i
				continue
			}
		}
		i++
	}
	return append(out, s[last:])
}

// CanonTitle normalizes a wiki link target to match article titles: strips
// section anchors, underscores→spaces, uppercases the first letter; drops
// File:/Category: etc.
func CanonTitle(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(strings.ReplaceAll(t, "_", " "))
	if t == "" || strings.HasPrefix(t, "File:") || strings.HasPrefix(t, "Image:") ||
		strings.HasPrefix(t, "Category:") || strings.HasPrefix(t, ":") {
		return ""
	}
	r := []rune(t)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// SplitList turns a wikitext value containing multiple entries (bullet lists,
// <br> separators, plainlist templates) into a slice of clean strings.
func SplitList(raw string) []string {
	// normalise list separators to newlines before cleaning
	v := raw
	v = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(v, "\n")
	// bullet items at line start
	v = regexp.MustCompile(`(?m)^\s*\*+`).ReplaceAllString(v, "\n")
	cleaned := CleanText(v)
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimSpace(strings.Trim(line, "•,;"))
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}
