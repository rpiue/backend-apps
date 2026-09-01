package chat

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var (
	reNonAlnum     = regexp.MustCompile(`[^a-z0-9\s]`)
	reSpaces       = regexp.MustCompile(`\s+`)
	rePlanWord     = regexp.MustCompile(`\b(plan|basico|medium|premium)\b`)
	rePurchaseWord = regexp.MustCompile(`\b(pagar|pago|comprar|compra|adquirir|activar|qr)\b`)
)

// stripAccents elimina diacríticos (equivalente a normalize NFD + remove marks).
func stripAccents(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return out
}

// collapseRepeats reemplaza 3+ repeticiones del mismo char por 2 (regex (.)\1{2,} -> $1$1).
func collapseRepeats(s string) string {
	var b strings.Builder
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		j := i + 1
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		run := j - i
		if run >= 3 {
			b.WriteRune(runes[i])
			b.WriteRune(runes[i])
		} else {
			for k := 0; k < run; k++ {
				b.WriteRune(runes[i])
			}
		}
		i = j
	}
	return b.String()
}

// collapseAll reemplaza CUALQUIER repetición consecutiva por un solo char ((.)\1+ -> $1).
func collapseAll(s string) string {
	var b strings.Builder
	var prev rune
	first := true
	for _, r := range s {
		if first || r != prev {
			b.WriteRune(r)
			prev = r
			first = false
		}
	}
	return b.String()
}

// normalizeTextForMatch replica normalizeTextForMatch del index.js.
func normalizeTextForMatch(text string) string {
	s := stripAccents(text)
	s = strings.ToLower(s)
	s = collapseRepeats(s)
	s = reNonAlnum.ReplaceAllString(s, " ")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// compactSpamText replica compactSpamText.
func compactSpamText(text string) string {
	compact := normalizeTextForMatch(text)
	compact = collapseAll(compact)
	compact = strings.ReplaceAll(compact, " ", "")
	switch compact {
	case "hola", "ola", "halo", "alo", "hello":
		return "hola"
	}
	return compact
}

func isLowInformationText(text string) bool {
	normalized := normalizeTextForMatch(text)
	compact := compactSpamText(text)
	if compact == "" {
		return true
	}
	if len(compact) < 3 {
		return true
	}
	if len(uniqueRunes(compact)) <= 2 && len(compact) <= 6 {
		return true
	}
	if !regexp.MustCompile(`[aeiou0-9]`).MatchString(compact) && len(compact) <= 5 {
		return true
	}
	words := strings.Fields(normalized)
	if len(words) == 1 && len(compact) <= 4 && hasRepeat3(text) {
		return true
	}
	return false
}

// hasRepeat3 indica si algún carácter aparece 3+ veces seguidas (p.ej. "aaa").
// Equivale al regex `(.)\1{2,}`, que RE2 (Go) no soporta por usar backreference.
func hasRepeat3(s string) bool {
	var prev rune
	run := 0
	for i, r := range s {
		if i > 0 && r == prev {
			run++
			if run >= 3 {
				return true
			}
		} else {
			run = 1
		}
		prev = r
	}
	return false
}

func uniqueRunes(s string) map[rune]struct{} {
	m := map[rune]struct{}{}
	for _, r := range s {
		m[r] = struct{}{}
	}
	return m
}

func looksLikePlanPurchase(text string) bool {
	clean := normalizeTextForMatch(text)
	if clean == "" {
		return false
	}
	return rePlanWord.MatchString(clean) && rePurchaseWord.MatchString(clean)
}

// spamSignal replica spamSignalFromText: {fingerprint, signature}.
func spamSignal(text string) (fingerprint, signature string) {
	normalized := normalizeTextForMatch(text)
	wordsRaw := strings.Fields(normalized)
	set := map[string]struct{}{}
	for _, w := range wordsRaw {
		if len(w) > 2 {
			set[w] = struct{}{}
		}
	}
	uniq := sortedKeys(set)
	compact := compactSpamText(text)
	if compact != "" {
		fingerprint = sha256Hex(compact)
	}
	signature = strings.Join(uniq, " ") + "|" + compact
	return fingerprint, signature
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// orden lexicográfico (como Array.sort por defecto)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// signatureSimilarity replica signatureSimilarity.
func signatureSimilarity(a, b string) float64 {
	aParts := strings.SplitN(a, "|", 2)
	bParts := strings.SplitN(b, "|", 2)
	aWords, aCompact := part(aParts, 0), part(aParts, 1)
	bWords, bCompact := part(bParts, 0), part(bParts, 1)
	if aCompact != "" && bCompact != "" {
		if aCompact == bCompact {
			return 1
		}
		maxLen := max(len(aCompact), len(bCompact))
		if maxLen <= 12 {
			dist := levenshtein(aCompact, bCompact)
			score := 1 - float64(dist)/float64(maxLen)
			if score >= 0.72 {
				return score
			}
		}
	}
	aw := fieldSet(aWords)
	bw := fieldSet(bWords)
	if len(aw) == 0 || len(bw) == 0 {
		return 0
	}
	hit := 0
	for w := range aw {
		if _, ok := bw[w]; ok {
			hit++
		}
	}
	return float64(hit) / float64(max(len(aw), len(bw)))
}

func part(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

func fieldSet(s string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range strings.Fields(s) {
		if w != "" {
			m[w] = struct{}{}
		}
	}
	return m
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		copy(prev, curr)
	}
	return prev[len(rb)]
}

// textScore replica textScore (Jaccard sobre palabras >2).
func textScore(a, b string) float64 {
	aw := wordSetGt2(a)
	bw := wordSetGt2(b)
	if len(aw) == 0 || len(bw) == 0 {
		return 0
	}
	hit := 0
	for w := range aw {
		if _, ok := bw[w]; ok {
			hit++
		}
	}
	return float64(hit) / float64(max(len(aw), len(bw)))
}

func wordSetGt2(s string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range strings.Fields(normalizeTextForMatch(s)) {
		if len(w) > 2 {
			m[w] = struct{}{}
		}
	}
	return m
}

func min3(a, b, c int) int { return min(min(a, b), c) }
