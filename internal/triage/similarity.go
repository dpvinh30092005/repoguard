package triage

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/example/repoguard/internal/platform"
)

// Similar is a candidate duplicate.
type Similar struct {
	Issue platform.Issue
	Score float64
}

// FindDuplicates ranks `corpus` by TF-IDF cosine similarity to `issue` and
// returns up to `limit` candidates scoring at least `threshold`. Titles count
// double: they are the densest signal in an issue.
//
// Deliberately dependency-free and deterministic; swap in embeddings later
// behind the same signature if recall becomes a problem.
func FindDuplicates(issue platform.Issue, corpus []platform.Issue, threshold float64, limit int) []Similar {
	docs := make([]map[string]float64, 0, len(corpus)+1)
	docs = append(docs, termFreq(issue))
	others := make([]platform.Issue, 0, len(corpus))
	for _, c := range corpus {
		if c.Number == issue.Number {
			continue
		}
		others = append(others, c)
		docs = append(docs, termFreq(c))
	}

	df := map[string]float64{}
	for _, d := range docs {
		for term := range d {
			df[term]++
		}
	}
	n := float64(len(docs))
	vec := func(tf map[string]float64) map[string]float64 {
		v := make(map[string]float64, len(tf))
		for term, f := range tf {
			v[term] = f * (math.Log((1+n)/(1+df[term])) + 1) // smoothed idf
		}
		return v
	}

	target := vec(docs[0])
	var out []Similar
	for i, other := range others {
		if s := cosine(target, vec(docs[i+1])); s >= threshold {
			out = append(out, Similar{Issue: other, Score: s})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func termFreq(i platform.Issue) map[string]float64 {
	tf := map[string]float64{}
	for _, t := range tokenize(i.Title) {
		tf[t] += 2
	}
	for _, t := range tokenize(i.Body) {
		tf[t]++
	}
	return tf
}

func tokenize(s string) []string {
	s = htmlComment.ReplaceAllString(strings.ToLower(s), " ")
	fields := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := fields[:0]
	for _, f := range fields {
		if len([]rune(f)) > 2 && !stopwords[f] {
			out = append(out, f)
		}
	}
	return out
}

func cosine(a, b map[string]float64) float64 {
	var dot, na, nb float64
	for k, v := range a {
		dot += v * b[k]
		na += v * v
	}
	for _, v := range b {
		nb += v * v
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for that this with when from have has are was were not but you your
		can will just get got its into out our there their what which then than they them been being also
		any all some more most other such only own same very should would could does did doing how why
		steps reproduce expected actual behavior behaviour version please thanks issue bug`) {
		stopwords[w] = true
	}
}
