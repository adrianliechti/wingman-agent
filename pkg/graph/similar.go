package graph

import (
	"sort"
	"strings"
	"unicode"
)

// Similarity weights: what a function calls (behavioural signature) is a
// stronger signal than how it is named, but names still help for leaf
// functions that call nothing.
const (
	calleeWeight = 0.7
	nameWeight   = 0.3
)

type Similar struct {
	Node  *Node   `json:"node"`
	Score float64 `json:"score"`
}

type SimilarResult struct {
	Target  *Node     `json:"target"`
	Matches []Similar `json:"matches"`
}

func isCallable(k Kind) bool {
	return k == KindFunction || k == KindMethod || k == KindConstructor
}

// similar ranks other callables by overlap of their callee sets and name
// tokens with the target. Pure graph derivation — no embeddings, no index.
func (g *Graph) similar(target *Node, limit int) []Similar {
	tCallees := g.calleeNames(target.ID)
	tTokens := nameTokens(target.Name)

	var out []Similar
	for _, c := range g.Nodes {
		if c.ID == target.ID || !isCallable(c.Kind) {
			continue
		}
		score := calleeWeight*jaccard(tCallees, g.calleeNames(c.ID)) +
			nameWeight*jaccard(tTokens, nameTokens(c.Name))
		if score <= 0 {
			continue
		}
		out = append(out, Similar{Node: c, Score: score})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Node.Name != out[j].Node.Name {
			return out[i].Node.Name < out[j].Node.Name
		}
		return out[i].Node.File < out[j].Node.File
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (g *Graph) calleeNames(id string) map[string]bool {
	out := map[string]bool{}
	for _, cid := range g.out[id] {
		if n := g.byID[cid]; n != nil {
			out[n.Name] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// nameTokens splits an identifier into lowercased word tokens, handling
// camelCase, acronyms (HTTPServer → http, server) and snake/kebab/dotted names.
func nameTokens(name string) map[string]bool {
	set := map[string]bool{}
	runes := []rune(name)
	var cur []rune

	flush := func() {
		if len(cur) >= 2 {
			set[strings.ToLower(string(cur))] = true
		}
		cur = cur[:0]
	}

	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0:
			prev := runes[i-1]
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				flush() // camelCase boundary
			} else if i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				flush() // acronym → word boundary
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return set
}
