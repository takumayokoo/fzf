package fzf

import (
	"regexp"
	"sort"
	"strings"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
)

// fuzzy
// 'exact
// ^exact-prefix
// exact-suffix$
// !not-fuzzy
// !'not-exact
// !^not-exact-prefix
// !not-exact-suffix$

type termType int

const (
	termFuzzy termType = iota
	termExact
	termPrefix
	termSuffix
	termEqual
)

type term struct {
	typ           termType
	inv           bool
	text          []rune
	caseSensitive bool
	origText      []rune
}

type termSet []term

// Pattern represents search pattern
type Pattern struct {
	fuzzy         bool
	extended      bool
	caseSensitive bool
	forward       bool
	text          []rune
	termSets      []termSet
	cacheable     bool
	delimiter     Delimiter
	nth           []Range
	procFun       map[termType]func(bool, bool, []rune, []rune) (int, int)
}

var (
	_patternCache map[string]*Pattern
	_splitRegex   *regexp.Regexp
	_cache        ChunkCache
)

func init() {
	_splitRegex = regexp.MustCompile("\\s+")
	clearPatternCache()
	clearChunkCache()
}

func clearPatternCache() {
	// We can uniquely identify the pattern for a given string since
	// search mode and caseMode do not change while the program is running
	_patternCache = make(map[string]*Pattern)
}

func clearChunkCache() {
	_cache = NewChunkCache()
}

// BuildPattern builds Pattern object from the given arguments
func BuildPattern(fuzzy bool, extended bool, caseMode Case, forward bool,
	nth []Range, delimiter Delimiter, runes []rune) *Pattern {

	var asString string
	if extended {
		asString = strings.Trim(string(runes), " ")
	} else {
		asString = string(runes)
	}

	cached, found := _patternCache[asString]
	if found {
		return cached
	}

	caseSensitive, cacheable := true, true
	termSets := []termSet{}

	if extended {
		termSets = parseTerms(fuzzy, caseMode, asString)
	Loop:
		for _, termSet := range termSets {
			for idx, term := range termSet {
				// If the query contains inverse search terms or OR operators,
				// we cannot cache the search scope
				if idx > 0 || term.inv {
					cacheable = false
					break Loop
				}
			}
		}
	} else {
		lowerString := strings.ToLower(asString)
		caseSensitive = caseMode == CaseRespect ||
			caseMode == CaseSmart && lowerString != asString
		if !caseSensitive {
			asString = lowerString
		}
	}

	ptr := &Pattern{
		fuzzy:         fuzzy,
		extended:      extended,
		caseSensitive: caseSensitive,
		forward:       forward,
		text:          []rune(asString),
		termSets:      termSets,
		cacheable:     cacheable,
		nth:           nth,
		delimiter:     delimiter,
		procFun:       make(map[termType]func(bool, bool, []rune, []rune) (int, int))}

	ptr.procFun[termFuzzy] = algo.FuzzyMatch
	ptr.procFun[termEqual] = algo.EqualMatch
	ptr.procFun[termExact] = algo.ExactMatchNaive
	ptr.procFun[termPrefix] = algo.PrefixMatch
	ptr.procFun[termSuffix] = algo.SuffixMatch

	_patternCache[asString] = ptr
	return ptr
}

func parseTerms(fuzzy bool, caseMode Case, str string) []termSet {
	tokens := _splitRegex.Split(str, -1)
	sets := []termSet{}
	set := termSet{}
	switchSet := false
	for _, token := range tokens {
		typ, inv, text := termFuzzy, false, token
		lowerText := strings.ToLower(text)
		caseSensitive := caseMode == CaseRespect ||
			caseMode == CaseSmart && text != lowerText
		if !caseSensitive {
			text = lowerText
		}
		origText := []rune(text)
		if !fuzzy {
			typ = termExact
		}

		if text == "|" {
			switchSet = false
			continue
		}

		if strings.HasPrefix(text, "!") {
			inv = true
			text = text[1:]
		}

		if strings.HasPrefix(text, "'") {
			// Flip exactness
			if fuzzy {
				typ = termExact
				text = text[1:]
			} else {
				typ = termFuzzy
				text = text[1:]
			}
		} else if strings.HasPrefix(text, "^") {
			if strings.HasSuffix(text, "$") {
				typ = termEqual
				text = text[1 : len(text)-1]
			} else {
				typ = termPrefix
				text = text[1:]
			}
		} else if strings.HasSuffix(text, "$") {
			typ = termSuffix
			text = text[:len(text)-1]
		}

		if len(text) > 0 {
			if switchSet {
				sets = append(sets, set)
				set = termSet{}
			}
			set = append(set, term{
				typ:           typ,
				inv:           inv,
				text:          []rune(text),
				caseSensitive: caseSensitive,
				origText:      origText})
			switchSet = true
		}
	}
	if len(set) > 0 {
		sets = append(sets, set)
	}
	return sets
}

// IsEmpty returns true if the pattern is effectively empty
func (p *Pattern) IsEmpty() bool {
	if !p.extended {
		return len(p.text) == 0
	}
	return len(p.termSets) == 0
}

// AsString returns the search query in string type
func (p *Pattern) AsString() string {
	return string(p.text)
}

// CacheKey is used to build string to be used as the key of result cache
func (p *Pattern) CacheKey() string {
	if !p.extended {
		return p.AsString()
	}
	cacheableTerms := []string{}
	for _, termSet := range p.termSets {
		if len(termSet) == 1 && !termSet[0].inv {
			cacheableTerms = append(cacheableTerms, string(termSet[0].origText))
		}
	}
	return strings.Join(cacheableTerms, " ")
}

// Match returns the list of matches Items in the given Chunk
func (p *Pattern) Match(chunk *Chunk) []*Item {
	space := chunk

	// ChunkCache: Exact match
	cacheKey := p.CacheKey()
	if p.cacheable {
		if cached, found := _cache.Find(chunk, cacheKey); found {
			return cached
		}
	}

	// ChunkCache: Prefix/suffix match
Loop:
	for idx := 1; idx < len(cacheKey); idx++ {
		// [---------| ] | [ |---------]
		// [--------|  ] | [  |--------]
		// [-------|   ] | [   |-------]
		prefix := cacheKey[:len(cacheKey)-idx]
		suffix := cacheKey[idx:]
		for _, substr := range [2]*string{&prefix, &suffix} {
			if cached, found := _cache.Find(chunk, *substr); found {
				cachedChunk := Chunk(cached)
				space = &cachedChunk
				break Loop
			}
		}
	}

	matches := p.matchChunk(space)

	if p.cacheable {
		_cache.Add(chunk, cacheKey, matches)
	}
	return matches
}

func (p *Pattern) matchChunk(chunk *Chunk) []*Item {
	matches := []*Item{}
	if !p.extended {
		for _, item := range *chunk {
			if sidx, eidx, tlen := p.basicMatch(item); sidx >= 0 {
				matches = append(matches,
					dupItem(item, []Offset{Offset{int32(sidx), int32(eidx), int32(tlen)}}))
			}
		}
	} else {
		for _, item := range *chunk {
			if offsets := p.extendedMatch(item); len(offsets) == len(p.termSets) {
				matches = append(matches, dupItem(item, offsets))
			}
		}
	}
	return matches
}

// MatchItem returns true if the Item is a match
func (p *Pattern) MatchItem(item *Item) bool {
	if !p.extended {
		sidx, _, _ := p.basicMatch(item)
		return sidx >= 0
	}
	offsets := p.extendedMatch(item)
	return len(offsets) == len(p.termSets)
}

func dupItem(item *Item, offsets []Offset) *Item {
	sort.Sort(ByOrder(offsets))
	return &Item{
		text:        item.text,
		origText:    item.origText,
		transformed: item.transformed,
		index:       item.index,
		offsets:     offsets,
		colors:      item.colors,
		rank:        Rank{0, 0, item.index}}
}

func (p *Pattern) basicMatch(item *Item) (int, int, int) {
	input := p.prepareInput(item)
	if p.fuzzy {
		return p.iter(algo.FuzzyMatch, input, p.caseSensitive, p.forward, p.text)
	}
	return p.iter(algo.ExactMatchNaive, input, p.caseSensitive, p.forward, p.text)
}

func (p *Pattern) extendedMatch(item *Item) []Offset {
	input := p.prepareInput(item)
	offsets := []Offset{}
Loop:
	for _, termSet := range p.termSets {
		for _, term := range termSet {
			pfun := p.procFun[term.typ]
			if sidx, eidx, tlen := p.iter(pfun, input, term.caseSensitive, p.forward, term.text); sidx >= 0 {
				if term.inv {
					break Loop
				}
				offsets = append(offsets, Offset{int32(sidx), int32(eidx), int32(tlen)})
				break
			} else if term.inv {
				offsets = append(offsets, Offset{0, 0, 0})
				break
			}
		}
	}
	return offsets
}

func (p *Pattern) prepareInput(item *Item) []Token {
	if item.transformed != nil {
		return item.transformed
	}

	var ret []Token
	if len(p.nth) > 0 {
		tokens := Tokenize(item.text, p.delimiter)
		ret = Transform(tokens, p.nth)
	} else {
		ret = []Token{Token{text: item.text, prefixLength: 0, trimLength: util.TrimLen(item.text)}}
	}
	item.transformed = ret
	return ret
}

func (p *Pattern) iter(pfun func(bool, bool, []rune, []rune) (int, int),
	tokens []Token, caseSensitive bool, forward bool, pattern []rune) (int, int, int) {
	for _, part := range tokens {
		prefixLength := part.prefixLength
		if sidx, eidx := pfun(caseSensitive, forward, part.text, pattern); sidx >= 0 {
			return sidx + prefixLength, eidx + prefixLength, part.trimLength
		}
	}
	return -1, -1, -1 // math.MaxUint16
}
