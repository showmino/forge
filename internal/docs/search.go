package docs

import (
	"strings"
)

const (
	// maxTerms is how many words of a query are searched for.
	maxTerms = 8
	// maxLineRunes is how much of a matched line is returned, around the match.
	maxLineRunes = 160
)

// Terms splits a search query into its words, lowercased, each once.
func Terms(q string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if len(out) == maxTerms {
			break
		}
		dup := false
		for _, o := range out {
			dup = dup || o == w
		}
		if !dup {
			out = append(out, w)
		}
	}
	return out
}

// Line is a line of a page that holds a searched word.
type Line struct {
	// Number counts from 1.
	Number int
	Text   string
}

// Match is how a page matched a search.
type Match struct {
	// TitleWords is how many of the words are in the title.
	TitleWords int
	// LineCount is how many lines hold a word.
	LineCount int
	// Lines are the first lines that hold a word, at most limit.
	Lines []Line
}

// Better orders matches best first: more words in the title, then more
// lines that hold one.
func Better(a, b Match) int {
	if a.TitleWords != b.TitleWords {
		return b.TitleWords - a.TitleWords
	}
	return b.LineCount - a.LineCount
}

// Find reports whether every term is somewhere in the page, its title
// included, and returns the lines that hold one. terms come from Terms.
func Find(md, title string, terms []string, limit int) (Match, bool) {
	if len(terms) == 0 {
		return Match{}, false
	}
	lowTitle := strings.ToLower(title)
	found := make([]bool, len(terms))
	var m Match
	for i, t := range terms {
		if strings.Contains(lowTitle, t) {
			found[i] = true
			m.TitleWords++
		}
	}
	for n, line := range strings.Split(md, "\n") {
		low := strings.ToLower(line)
		at := -1
		for i, t := range terms {
			if j := strings.Index(low, t); j >= 0 {
				found[i] = true
				if at < 0 || j < at {
					at = j
				}
			}
		}
		if at < 0 {
			continue
		}
		m.LineCount++
		if len(m.Lines) < limit {
			m.Lines = append(m.Lines, Line{Number: n + 1, Text: clip(line, low, at)})
		}
	}
	for _, f := range found {
		if !f {
			return Match{}, false
		}
	}
	return m, true
}

// clip trims a line to maxLineRunes runes around the byte offset at in its
// lowercased form low, marking what was cut with an ellipsis.
func clip(line, low string, at int) string {
	line = strings.TrimRight(line, " \t\r")
	r := []rune(line)
	if len(r) <= maxLineRunes {
		return strings.TrimSpace(line)
	}
	// Lowercasing maps rune to rune, so a rune offset in low is one in line.
	pos := len([]rune(low[:at]))
	start := max(0, pos-maxLineRunes/3)
	end := min(len(r), start+maxLineRunes)
	start = max(0, end-maxLineRunes)
	out := strings.TrimSpace(string(r[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}
