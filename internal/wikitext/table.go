package wikitext

import (
	"strconv"
	"strings"
)

// Reconstructing a wikitable into the grid a reader sees.
//
// The source order of cells is not the on-screen order. A cell with
// rowspan="4" occupies a column in the next three rows too, so every later cell
// in those rows shifts right — and in a television schedule that shift is the
// difference between a show airing at 8:00 and at 9:00. Reading cells
// positionally without resolving spans produces a grid that looks plausible and
// is wrong, which is worse than failing.
//
// This is the standard table-layout walk: keep a map of slots already claimed by
// an earlier cell's span, and place each new cell in the first free column of
// its row.

// A TableCell is one cell after the grid has been resolved.
type TableCell struct {
	Row, Col   int
	RowSpan    int
	ColSpan    int
	Header     bool
	Text       string // cell content with styling attributes removed
	occupiedBy bool   // set on the slots a span covers, not on the cell itself
}

// A Table is a wikitable resolved to a grid. Cells holds only real cells, each
// carrying the position and extent it occupies.
type Table struct {
	Cells []TableCell
	Rows  int
	Cols  int
}

// At returns the cell covering a position, and whether one does. A position
// covered by another cell's span returns that cell, which is what makes "what
// is in column 3" answerable.
func (t *Table) At(row, col int) (TableCell, bool) {
	for _, c := range t.Cells {
		if row >= c.Row && row < c.Row+c.RowSpan && col >= c.Col && col < c.Col+c.ColSpan {
			return c, true
		}
	}
	return TableCell{}, false
}

// FindTables returns every wikitable in the text, resolved.
//
// Nested tables are not descended into: a schedule grid never contains one, and
// treating an inner table's rows as outer rows would corrupt the alignment.
func FindTables(text string) []*Table {
	var out []*Table
	for i := 0; i < len(text); {
		start := strings.Index(text[i:], "{|")
		if start < 0 {
			break
		}
		start += i
		end, depth := -1, 0
		for j := start; j < len(text)-1; j++ {
			switch {
			case text[j] == '{' && text[j+1] == '|':
				depth++
				j++
			case text[j] == '|' && text[j+1] == '}':
				depth--
				if depth == 0 {
					end = j
				}
				j++
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		if tb := parseTable(text[start : end+2]); tb != nil {
			out = append(out, tb)
		}
		i = end + 2
	}
	return out
}

func parseTable(src string) *Table {
	lines := strings.Split(src, "\n")
	t := &Table{}
	// occupied[row][col] marks slots already claimed by an earlier rowspan.
	occupied := map[int]map[int]bool{}
	claim := func(r, c int) {
		if occupied[r] == nil {
			occupied[r] = map[int]bool{}
		}
		occupied[r][c] = true
	}
	free := func(r, c int) bool { return occupied[r] == nil || !occupied[r][c] }

	row := -1
	col := 0
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(s, "{|"), strings.HasPrefix(s, "|}"):
			continue
		case strings.HasPrefix(s, "|+"): // caption
			continue
		case strings.HasPrefix(s, "|-"): // new row
			row++
			col = 0
			continue
		}
		var cells []string
		var header bool
		switch {
		case strings.HasPrefix(s, "!"):
			header = true
			cells = splitCells(s[1:], "!!")
		case strings.HasPrefix(s, "|"):
			cells = splitCells(s[1:], "||")
		default:
			continue // continuation of a multi-line cell; content only
		}
		if row < 0 {
			row = 0
		}
		for _, raw := range cells {
			attrs, body := splitAttrs(raw)
			rs := spanOf(attrs, "rowspan")
			cs := spanOf(attrs, "colspan")
			for !free(row, col) { // step over slots an earlier rowspan claimed
				col++
			}
			c := TableCell{Row: row, Col: col, RowSpan: rs, ColSpan: cs,
				Header: header, Text: strings.TrimSpace(body)}
			t.Cells = append(t.Cells, c)
			for r := row; r < row+rs; r++ {
				for cc := col; cc < col+cs; cc++ {
					claim(r, cc)
				}
			}
			col += cs
			if row+rs > t.Rows {
				t.Rows = row + rs
			}
			if col > t.Cols {
				t.Cols = col
			}
		}
	}
	if len(t.Cells) == 0 {
		return nil
	}
	return t
}

// splitCells divides a row into cells on the inline separator, without breaking
// on a separator that sits inside a wikilink or a template.
func splitCells(s, sep string) []string {
	var out []string
	depth, last := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "[["), strings.HasPrefix(s[i:], "{{"):
			depth++
			i++
		case strings.HasPrefix(s[i:], "]]"), strings.HasPrefix(s[i:], "}}"):
			if depth > 0 {
				depth--
			}
			i++
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			out = append(out, s[last:i])
			i++
			last = i + 1
		}
	}
	return append(out, s[last:])
}

// splitAttrs separates a cell's HTML attributes from its content. The separator
// is a single "|", which must not be confused with the "||" that divides cells
// or with a "|" inside a wikilink.
func splitAttrs(raw string) (attrs, body string) {
	depth := 0
	for i := 0; i < len(raw); i++ {
		switch {
		case strings.HasPrefix(raw[i:], "[["), strings.HasPrefix(raw[i:], "{{"):
			depth++
			i++
		case strings.HasPrefix(raw[i:], "]]"), strings.HasPrefix(raw[i:], "}}"):
			if depth > 0 {
				depth--
			}
			i++
		case depth == 0 && raw[i] == '|':
			if i+1 < len(raw) && raw[i+1] == '|' {
				return "", raw // "||" is a cell divider, so there are no attributes
			}
			return raw[:i], raw[i+1:]
		}
	}
	return "", raw
}

func spanOf(attrs, name string) int {
	i := strings.Index(strings.ToLower(attrs), name+"=")
	if i < 0 {
		return 1
	}
	v := strings.TrimSpace(attrs[i+len(name)+1:])
	v = strings.TrimLeft(v, `"'`)
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil || n < 1 {
		return 1
	}
	return n
}
