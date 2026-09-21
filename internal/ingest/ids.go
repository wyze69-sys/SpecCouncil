package ingest

import "strconv"

// IdentifiedBlock is one parsed block paired with its assigned stable unit ID.
type IdentifiedBlock struct {
	ID    string
	Block Block
}

// AssignIDs assigns a stable, deterministic unit ID to each block, preserving
// input order. It is pure: identical input yields identical output, and it uses
// no clock, filesystem, network, or randomness.
//
// A block that starts with an explicit author ID token, such as "REQ-12 the
// system must ...", keeps that token as its base ID; every other block gets the
// generated base ID "u" + Block.Order. Code blocks are never scanned for an
// author ID, because their text is verbatim program text. Duplicate base IDs are
// disambiguated with the smallest free "-2", "-3", ... suffix in input order, so
// the returned IDs are non-empty and unique.
//
// AssignIDs never mutates block text: the author ID is copied out of the text
// and the block is returned byte-for-byte unchanged.
func AssignIDs(blocks []Block) []IdentifiedBlock {
	out := make([]IdentifiedBlock, 0, len(blocks))
	// taken holds every ID already used, so uniqueness is decided in input order.
	taken := make(map[string]struct{}, len(blocks))
	// nextCounter is the next candidate integer >= 2 to try for a base ID; it
	// only advances past unsuffixed literals ("REQ-12-2" in the input) and
	// suffixed literals of the same base, never past this base's own suffixes.
	nextCounter := make(map[string]int, len(blocks))

	for _, blk := range blocks {
		base := generatedBaseID(blk)
		if blk.Kind != BlockCode {
			if token, ok := authorID(blk.Text); ok {
				base = token
			}
		}

		id := base
		if _, dup := taken[id]; dup {
			// Smallest integer >= 2 that is neither taken nor reserved by a
			// literal of this same base ("REQ-12-2" in the input), so a base's
			// own suffixes satisfy requirement 3 exactly.
			n := nextCounter[base]
			if n < 2 {
				n = 2
			}
			for {
				candidate := base + "-" + strconv.Itoa(n)
				if _, used := taken[candidate]; !used {
					id = candidate
					n++
					break
				}
				n++
			}
			nextCounter[base] = n
			// A literal "REQ-12-2" never reserves anything for the bare base
			// "REQ-12-2", but its own suffixes still start at "-2".
			if _, ok := nextCounter[id]; !ok {
				nextCounter[id] = 2
			}
		}

		taken[id] = struct{}{}
		out = append(out, IdentifiedBlock{ID: id, Block: blk})
	}

	return out
}

// generatedBaseID is the fallback base ID of a block: "u" + Block.Order, e.g.
// "u0", "u1". It uses Block.Order rather than the slice index so the rule stays
// correct even when the caller passes a slice whose orders are non-contiguous.
func generatedBaseID(blk Block) string {
	return "u" + strconv.Itoa(blk.Order)
}

// authorID returns the explicit author ID token that starts text, if any, and
// whether one was found.
//
// A token is one ASCII letter, then zero or more ASCII letters or digits, then a
// hyphen, then one or more digits, immediately followed by end of text, a space,
// a tab, a colon, a period, or a ")". The token is returned exactly as written,
// preserving its case. Because it must start at byte 0 and may only be followed
// by an allowed boundary, "REQ12", "-12", "REQ-", "REQ-12abc", and "REQ-12-2"
// are not author IDs.
func authorID(text string) (string, bool) {
	// One leading ASCII letter.
	if len(text) == 0 || !isASCIILetter(text[0]) {
		return "", false
	}

	i := 1
	for i < len(text) && isASCIIAlnum(text[i]) {
		i++
	}
	// A hyphen must follow the leading name.
	if i >= len(text) || text[i] != '-' {
		return "", false
	}
	i++

	// One or more digits must follow the hyphen.
	digitsStart := i
	for i < len(text) && isASCIIDigit(text[i]) {
		i++
	}
	if i == digitsStart {
		return "", false
	}

	if i < len(text) && !isAuthorIDBoundary(text[i]) {
		return "", false
	}
	return text[:i], true
}

// isAuthorIDBoundary reports whether c may immediately follow an author ID token.
func isAuthorIDBoundary(c byte) bool {
	switch c {
	case ' ', '\t', ':', '.', ')':
		return true
	}
	return false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isASCIIAlnum(c byte) bool {
	return isASCIILetter(c) || isASCIIDigit(c)
}
