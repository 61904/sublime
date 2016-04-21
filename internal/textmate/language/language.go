// Copyright 2013 The lime Authors.
// Use of this source code is governed by a 2-clause
// BSD-style license that can be found in the LICENSE file.

package language

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/limetext/backend/log"
	"github.com/limetext/loaders"
	"github.com/limetext/sublime/internal/textmate"
	"github.com/limetext/text"
	"github.com/quarnster/parser"
)

const maxiter = 10000

type (
	// For loading tmLanguage files
	Language struct {
		UnpatchedLanguage
	}

	Provider struct {
		sync.Mutex
		scope map[string]string
	}

	UnpatchedLanguage struct {
		FileTypes      []string
		FirstLineMatch string
		Name           string
		RootPattern    RootPattern `json:"patterns"`
		Repository     map[string]*Pattern
		ScopeName      string
	}

	Named struct {
		Name string
	}

	Capture struct {
		Key int
		Named
	}

	Captures []Capture

	Pattern struct {
		Named
		Include        string
		Match          textmate.Regex
		Captures       Captures
		Begin          textmate.Regex
		BeginCaptures  Captures
		End            textmate.Regex
		EndCaptures    Captures
		Patterns       []Pattern
		owner          *Language // needed for include directives
		cachedData     string
		cachedPat      *Pattern
		cachedPatterns []*Pattern
		cachedMatch    textmate.MatchObject
		hits           int
		misses         int
	}

	RootPattern struct {
		Pattern
	}

	Parser struct {
		l    *Language
		data []rune
	}
)

func Load(filename string) (*Language, error) {
	d, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("Couldn't load file %s: %s", filename, err)
	}
	var l Language
	if err := loaders.LoadPlist(d, &l); err != nil {
		return nil, err
	}
	provider.Add(l.ScopeName, filename)
	return &l, nil
}

var (
	provider Provider
	failed   = make(map[string]bool)
)

func init() {
	provider.scope = make(map[string]string)
}

func (t *Provider) GetLanguage(id string) (*Language, error) {
	if l, err := t.LanguageFromScope(id); err != nil {
		return Load(id)
	} else {
		return l, err
	}
}

func (t *Provider) LanguageFromScope(id string) (*Language, error) {
	t.Lock()
	s, ok := t.scope[id]
	t.Unlock()
	if !ok {
		return nil, errors.New("Can't handle id " + id)
	} else {
		return Load(s)
	}
}

func (t *Provider) Add(scope, filename string) {
	t.Lock()
	defer t.Unlock()
	t.scope[scope] = filename
}

func (p Pattern) String() (ret string) {
	ret = fmt.Sprintf(`---------------------------------------
Name:    %s
Match:   %s
Begin:   %s
End:     %s
Include: %s
`, p.Name, p.Match, p.Begin, p.End, p.Include)
	ret += fmt.Sprintf("<Sub-Patterns>\n")
	for i := range p.Patterns {
		inner := fmt.Sprintf("%s", p.Patterns[i])
		ret += fmt.Sprintf("\t%s\n", strings.Replace(strings.Replace(inner, "\t", "\t\t", -1), "\n", "\n\t", -1))
	}
	ret += fmt.Sprintf("</Sub-Patterns>\n---------------------------------------")
	return
}

func (r *RootPattern) String() (ret string) {
	for i := range r.Patterns {
		ret += fmt.Sprintf("\t%s\n", r.Patterns[i])
	}
	return
}

func (s *Language) String() string {
	return fmt.Sprintf("%s\n%s\n%s\n", s.ScopeName, s.RootPattern, s.Repository)
}

func (p *Pattern) tweak(l *Language) {
	p.owner = l
	p.Name = strings.TrimSpace(p.Name)
	for i := range p.Patterns {
		p.Patterns[i].tweak(l)
	}
}

func (l *Language) tweak() {
	l.RootPattern.tweak(l)
	for k := range l.Repository {
		p := l.Repository[k]
		p.tweak(l)
		l.Repository[k] = p
	}
}

func (l *Language) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &l.UnpatchedLanguage); err != nil {
		return err
	}
	l.tweak()
	return nil
}

func (l *Language) Copy() *Language {
	ret := &Language{}
	ret.FileTypes = make([]string, len(l.FileTypes))
	copy(ret.FileTypes, l.FileTypes)
	ret.FirstLineMatch = l.FirstLineMatch
	ret.Name = l.Name
	ret.RootPattern.Pattern = *l.RootPattern.Pattern.copy(ret)
	ret.Repository = make(map[string]*Pattern)
	for key, pat := range l.Repository {
		ret.Repository[key] = pat.copy(ret)
	}
	ret.ScopeName = l.ScopeName
	return ret
}

func (r *RootPattern) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &r.Patterns)
}

func (c *Captures) UnmarshalJSON(data []byte) error {
	tmp := make(map[string]Named)
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	for k, v := range tmp {
		i, _ := strconv.ParseInt(k, 10, 32)
		*c = append(*c, Capture{Key: int(i), Named: v})
	}
	sort.Sort(c)
	return nil
}

func (c *Captures) Len() int {
	return len(*c)
}

func (c *Captures) Less(i, j int) bool {
	return (*c)[i].Key < (*c)[j].Key
}

func (c *Captures) Swap(i, j int) {
	(*c)[i], (*c)[j] = (*c)[j], (*c)[i]
}

func (c *Captures) copy() *Captures {
	ret := make(Captures, len(*c))
	copy(ret, *c)
	return &ret
}

func (p *Pattern) FirstMatch(data string, pos int) (pat *Pattern, ret textmate.MatchObject) {
	startIdx := -1
	for i := 0; i < len(p.cachedPatterns); {
		ip, im := p.cachedPatterns[i].Cache(data, pos)
		if im != nil {
			if startIdx < 0 || startIdx > im[0] {
				startIdx, pat, ret = im[0], ip, im
				// This match is right at the start, we're not
				// going to find a better pattern than this,
				// so stop the search
				if im[0] == pos {
					break
				}
			}
			i++
		} else {
			// If it wasn't found now, it'll never be found,
			// so the pattern can be popped from the cache
			copy(p.cachedPatterns[i:], p.cachedPatterns[i+1:])
			p.cachedPatterns = p.cachedPatterns[:len(p.cachedPatterns)-1]
		}
	}
	return
}

func (p *Pattern) Cache(data string, pos int) (pat *Pattern, ret textmate.MatchObject) {
	if p.cachedData == data {
		if p.cachedMatch == nil {
			return nil, nil
		}
		if p.cachedMatch[0] >= pos && p.cachedPat.cachedMatch != nil {
			p.hits++
			return p.cachedPat, p.cachedMatch
		}
	} else {
		p.cachedPatterns = nil
	}
	if p.cachedPatterns == nil {
		p.cachedPatterns = make([]*Pattern, len(p.Patterns))
		for i := range p.cachedPatterns {
			p.cachedPatterns[i] = &p.Patterns[i]
		}
	}
	p.misses++

	if !p.Match.Empty() {
		pat, ret = p, p.Match.Find(data, pos)
	} else if !p.Begin.Empty() {
		pat, ret = p, p.Begin.Find(data, pos)
	} else if p.Include != "" {
		if z := p.Include[0]; z == '#' {
			key := p.Include[1:]
			if p2, ok := p.owner.Repository[key]; ok {
				pat, ret = p2.Cache(data, pos)
			} else {
				log.Fine("Not found in repository: %s", p.Include)
			}
		} else if z == '$' {
			// TODO(q): Implement tmLanguage $ include directives
			log.Warn("Unhandled include directive: %s", p.Include)
		} else if l, err := provider.GetLanguage(p.Include); err != nil {
			if !failed[p.Include] {
				log.Warn("Include directive %s failed: %s", p.Include, err)
			}
			failed[p.Include] = true
		} else {
			return l.RootPattern.Cache(data, pos)
		}
	} else {
		pat, ret = p.FirstMatch(data, pos)
	}
	p.cachedData = data
	p.cachedMatch = ret
	p.cachedPat = pat

	return
}

func (p *Pattern) CreateCaptureNodes(data string, pos int, d parser.DataSource, mo textmate.MatchObject, parent *parser.Node, capt Captures) {
	ranges := make([]text.Region, len(mo)/2)
	parentIndex := make([]int, len(ranges))
	parents := make([]*parser.Node, len(parentIndex))
	for i := range ranges {
		ranges[i] = text.Region{A: mo[i*2+0], B: mo[i*2+1]}
		if i < 2 {
			parents[i] = parent
			continue
		}
		r := ranges[i]
		for j := i - 1; j >= 0; j-- {
			if ranges[j].Covers(r) {
				parentIndex[i] = j
				break
			}
		}
	}

	for _, v := range capt {
		i := v.Key
		if i >= len(parents) || ranges[i].A == -1 {
			continue
		}
		child := &parser.Node{Name: v.Name, Range: ranges[i], P: d}
		parents[i] = child
		if i == 0 {
			parent.Append(child)
			continue
		}
		var p *parser.Node
		for p == nil {
			i = parentIndex[i]
			p = parents[i]
		}
		p.Append(child)
	}
}

func (p *Pattern) CreateNode(data string, pos int, d parser.DataSource, mo textmate.MatchObject) (ret *parser.Node) {
	ret = &parser.Node{Name: p.Name, Range: text.Region{A: mo[0], B: mo[1]}, P: d}
	defer ret.UpdateRange()

	if !p.Match.Empty() {
		p.CreateCaptureNodes(data, pos, d, mo, ret, p.Captures)
	}
	if p.Begin.Empty() {
		return
	}
	if len(p.BeginCaptures) > 0 {
		p.CreateCaptureNodes(data, pos, d, mo, ret, p.BeginCaptures)
	} else {
		p.CreateCaptureNodes(data, pos, d, mo, ret, p.Captures)
	}

	if p.End.Empty() {
		return
	}
	var (
		found  = false
		i, end int
	)
	for i, end = ret.Range.B, len(data); i < len(data); {
		endmatch := p.End.Find(data, i)
		if endmatch != nil {
			end = endmatch[1]
		} else {
			if !found {
				// oops.. no end found at all, set it to the next line
				if e2 := strings.IndexRune(data[i:], '\n'); e2 != -1 {
					end = i + e2
				} else {
					end = len(data)
				}
				break
			} else {
				end = i
				break
			}
		}
		if len(p.cachedPatterns) > 0 {
			// Might be more recursive patterns to apply BEFORE the end is reached
			pattern2, match2 := p.FirstMatch(data, i)
			if match2 != nil && ((endmatch == nil && match2[0] < end) || (endmatch != nil && (match2[0] < endmatch[0] || match2[0] == endmatch[0] && ret.Range.A == ret.Range.B))) {
				found = true
				r := pattern2.CreateNode(data, i, d, match2)
				ret.Append(r)
				i = r.Range.B
				continue
			}
		}
		if endmatch != nil {
			if len(p.EndCaptures) > 0 {
				p.CreateCaptureNodes(data, i, d, endmatch, ret, p.EndCaptures)
			} else {
				p.CreateCaptureNodes(data, i, d, endmatch, ret, p.Captures)
			}
		}
		break
	}
	ret.Range.B = end
	return
}

func (p *Pattern) copy(l *Language) *Pattern {
	ret := &Pattern{}
	ret.Named = p.Named
	ret.Include = p.Include
	ret.Match = *p.Match.Copy()
	if p.Captures != nil {
		ret.Captures = *p.Captures.copy()
	}
	ret.Begin = *p.Begin.Copy()
	if p.BeginCaptures != nil {
		ret.BeginCaptures = *p.BeginCaptures.copy()
	}
	ret.End = *p.End.Copy()
	if p.EndCaptures != nil {
		ret.EndCaptures = *p.EndCaptures.copy()
	}
	ret.owner = l
	for _, pat := range p.Patterns {
		ret.Patterns = append(ret.Patterns, *pat.copy(l))
	}
	return ret
}

func (p *Parser) Data(a, b int) string {
	a = text.Clamp(0, len(p.data), a)
	b = text.Clamp(0, len(p.data), b)
	return string(p.data[a:b])
}

func (p *Parser) patch(lut []int, node *parser.Node) {
	node.Range.A = lut[node.Range.A]
	node.Range.B = lut[node.Range.B]
	for _, child := range node.Children {
		p.patch(lut, child)
	}
}

func (p *Parser) Parse() (*parser.Node, error) {
	sdata := string(p.data)
	rn := parser.Node{P: p, Name: p.l.ScopeName}
	defer func() {
		if r := recover(); r != nil {
			log.Error("Panic during parse: %v\n", r)
			log.Debug("%v", rn)
		}
	}()
	iter := maxiter
	for i := 0; i < len(sdata) && iter > 0; iter-- {
		pat, ret := p.l.RootPattern.Cache(sdata, i)
		nl := strings.IndexAny(sdata[i:], "\n\r")
		if nl != -1 {
			nl += i
		}
		if ret == nil {
			break
		} else if nl > 0 && nl <= ret[0] {
			i = nl
			for i < len(sdata) && (sdata[i] == '\n' || sdata[i] == '\r') {
				i++
			}
		} else {
			n := pat.CreateNode(sdata, i, p, ret)
			rn.Append(n)

			i = n.Range.B
		}
	}
	rn.UpdateRange()
	if len(sdata) != 0 {
		lut := make([]int, len(sdata)+1)
		j := 0
		for i := range sdata {
			lut[i] = j
			j++
		}
		lut[len(sdata)] = len(p.data)
		p.patch(lut, &rn)
	}
	if iter == 0 {
		panic("reached maximum number of iterations")
	}
	return &rn, nil
}

func NewParser(l *Language, data []rune) *Parser {
	return &Parser{l, data}
}