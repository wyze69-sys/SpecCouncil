// Package ingest turns a submitted design document into ordered, deterministic
// structural evidence.
//
// This slice produces raw structural blocks only. Assigning unit IDs, mapping
// blocks to evidence kinds, building a frozen snapshot, and serving them over
// HTTP are later, separately verified slices. The parser here is pure: it never
// consults the clock, filesystem, network, or any random source, and it never
// imports another internal package.
package ingest

import "strings"

// BlockKind classifies one raw structural block of a submitted design.
type BlockKind string

const (
	BlockHeading   BlockKind = "heading"
	BlockParagraph BlockKind = "paragraph"
	BlockListItem  BlockKind = "list_item"
	BlockTableRow  BlockKind = "table_row"
	BlockCode      BlockKind = "code"
)

// fence is the Markdown code fence marker. This slice uses a first-three-
// character test on the trimmed line, so four or more backticks are treated
// exactly like three; nested fences are not supported.
const fence = "```"

// Block is one ordered structural piece of a submitted design document.
// It carries no ID and no evidence kind; those are assigned by later
// ingestion slices.
type Block struct {
	Kind  BlockKind
	Text  string // normalized block text (see ParseBlocks)
	Order int    // 0-based position in document order
}

// ParseBlocks splits a submitted design document into ordered structural
// blocks. It is pure and deterministic: identical input always yields an
// identical slice, independent of environment, and it never consults the
// clock, filesystem, network, or any random source.
//
// Classification is a single forward pass over the lines of content, checked in
// this exact precedence: fenced code block, heading, table row, list item, then
// paragraph. Line endings are normalized before classification, so "\r\n" and
// "\r" input classifies exactly like "\n" input.
//
// Classification reads a TrimSpace-normalized copy of each line while paragraph
// text keeps the line's own bytes so that interior indentation survives. "Blank"
// and "trimmed" always mean Go strings.TrimSpace semantics.
//
// A fenced code block whose text is blank under strings.TrimSpace (for example
// "```\n   \n```") emits no block at all, matching every other block kind; a
// code block that does emit keeps its own bytes untrimmed.
func ParseBlocks(content string) []Block {
	blocks := []Block{}
	paragraph := make([]string, 0, 4)

	// flush emits the pending paragraph, if it has non-empty text, and resets it.
	flush := func() {
		if text := paragraphText(paragraph); text != "" {
			blocks = append(blocks, Block{
				Kind:  BlockParagraph,
				Text:  text,
				Order: len(blocks),
			})
		}
		paragraph = paragraph[:0]
	}

	lines := splitLines(content)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" { // blank line: paragraph boundary, never a block
			flush()
			continue
		}

		if isFence(trimmed) {
			flush()
			i++ // skip the opening fence line itself
			inner := make([]string, 0, 4)
			for ; i < len(lines); i++ {
				if isFence(strings.TrimSpace(lines[i])) {
					break
				}
				inner = append(inner, lines[i]) // code is significant: no trimming
			}
			// An unterminated fence leaves i == len(lines), so every remaining
			// line is part of this block and the loop ends after it. A block
			// whose text is blank under TrimSpace (for example "```\n   \n```")
			// emits nothing, exactly like a byte-empty fence; emitted code text
			// still keeps its own bytes.
			if text := strings.Join(inner, "\n"); strings.TrimSpace(text) != "" {
				blocks = append(blocks, Block{
					Kind:  BlockCode,
					Text:  text,
					Order: len(blocks),
				})
			}
			continue
		}

		if text, ok := headingText(trimmed); ok {
			flush()
			if text != "" {
				blocks = append(blocks, Block{
					Kind:  BlockHeading,
					Text:  text,
					Order: len(blocks),
				})
			}
			continue
		}

		if isTableRow(trimmed) {
			flush()
			blocks = append(blocks, Block{
				Kind:  BlockTableRow,
				Text:  trimmed,
				Order: len(blocks),
			})
			continue
		}

		if text, ok := listItemText(trimmed); ok {
			flush()
			if text != "" {
				blocks = append(blocks, Block{
					Kind:  BlockListItem,
					Text:  text,
					Order: len(blocks),
				})
			}
			continue
		}

		paragraph = append(paragraph, line)
	}

	flush()
	return blocks
}

// paragraphText joins the pending paragraph lines with a single "\n" and trims
// the outer edges of the result. Interior joins, and interior indentation within
// a line, are preserved because the lines keep their own bytes.
func paragraphText(lines []string) string {
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// splitLines normalizes "\r\n" and "\r" to "\n" and splits on the result. It
// drops at most the single trailing empty segment so that a document ending in
// a newline does not manufacture a final blank line; interior blank lines, which
// are paragraph boundaries, are preserved.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}

	normalized := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(content)

	lines := strings.Split(normalized, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// isFence reports whether a normalized line opens or closes a fenced code block.
func isFence(line string) bool {
	return strings.HasPrefix(line, fence)
}

// headingText returns the heading text of a normalized line. A run of one to six
// leading "#" followed by at least one space is a heading; the following spaces
// count as the separator run, so "\t" after "##" is not a heading in this slice.
// A run of seven or more "#" is not a heading, and neither is "#" with no
// following space; both stay paragraph text.
func headingText(line string) (string, bool) {
	hashes := 0
	for hashes < len(line) && line[hashes] == '#' {
		hashes++
	}
	if hashes < 1 || hashes > 6 {
		return "", false
	}
	rest := line[hashes:]
	if !strings.HasPrefix(rest, " ") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// isTableRow reports whether a normalized line is a GitHub-style table row. A
// separator row such as "| --- | :--: |" is a table row in this slice; later
// slices decide semantics.
func isTableRow(line string) bool {
	if !strings.HasPrefix(line, "|") {
		return false
	}
	return strings.Contains(line[1:], "|")
}

// listItemText returns the content of a list item, or false when line is not a
// list item. Unordered markers are "-", "*", and "+"; ordered markers are digits
// followed by "." or ")".
//
// A marker is followed either by a space that starts the content or by nothing at
// all. A bare marker is therefore a list item with empty content, which this
// slice emits nothing for, while a separator-only marker ("- ", "1. ") is a list
// item whose content trims to empty. A digit run whose "." or ")" introduces a
// further digit run ("1.2") is not a marker, so "2024 Review" and "1.2" stay
// paragraph text.
func listItemText(line string) (string, bool) {
	if len(line) >= 1 {
		switch line[0] {
		case '-', '*', '+':
			return markerContent(line[1:])
		}
	}
	if digits, ok := orderedMarker(line); ok {
		return markerContent(line[digits:])
	}
	return "", false
}

// markerContent trims the content that follows a decorative list marker: either a
// single space that introduces the content, or nothing else on the line.
func markerContent(rest string) (string, bool) {
	if rest == "" {
		return "", true // bare marker with no content
	}
	if rest[0] != ' ' {
		return "", false
	}
	return strings.TrimSpace(rest[1:]), true
}

// orderedMarker returns the length of a leading ordered marker ("1", "1. ",
// "2) x") when line starts with digits immediately followed by "." or ")".
// A "." or ")" that introduces a further digit run ("1.2") is not a marker.
func orderedMarker(line string) (int, bool) {
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	if digits >= len(line) {
		return 0, false
	}
	switch line[digits] {
	case '.', ')':
		if digits+1 < len(line) && line[digits+1] >= '0' && line[digits+1] <= '9' {
			return 0, false // "1.2" is not a list marker
		}
		return digits + 1, true
	}
	return 0, false
}
