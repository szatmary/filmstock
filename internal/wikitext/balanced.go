package wikitext

// BalancedBody returns the inner body after `nameLen` bytes past `start` and the
// index just past the closing "}}" (or -1). Mirrors FindTemplate's brace walk.
func BalancedBody(text string, start, nameLen int) (string, int) {
	depth, i := 0, start
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
				return text[start+nameLen : i-2], i
			}
			continue
		}
		i++
	}
	return "", -1
}
