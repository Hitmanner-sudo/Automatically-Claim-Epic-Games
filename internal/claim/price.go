package claim

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Verdict is what a piece of evidence says about the price of an order.
type Verdict int

const (
	// Unknown: the evidence says nothing usable about the total.
	Unknown Verdict = iota
	// Free: the evidence states a total, and it is zero.
	Free
	// Paid: the evidence states a total above zero. Nothing may be ordered.
	Paid
)

func (v Verdict) String() string {
	switch v {
	case Free:
		return "free"
	case Paid:
		return "PAID"
	default:
		return "unknown"
	}
}

// totalKeys are the JSON field names that carry what the buyer is charged.
// Per-unit, original and discount amounts are deliberately not listed: for a
// 100%-off promotion those are non-zero while the total is zero.
var totalKeys = map[string]bool{
	"totalprice":    true,
	"totalamount":   true,
	"ordertotal":    true,
	"total":         true,
	"grandtotal":    true,
	"payableamount": true,
	"payableprice":  true,
	"amountdue":     true,
	"chargeamount":  true,
	"finalprice":    true,
	"finalamount":   true,
}

// PriceFact is one "total" found in a document.
type PriceFact struct {
	Path  string
	Value float64
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

func walk(v any, path string, depth int, out *[]PriceFact) {
	if depth > 24 {
		return
	}
	switch n := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := n[k]
			p := path + "." + k
			if totalKeys[strings.ToLower(k)] {
				if f, ok := asNumber(child); ok {
					*out = append(*out, PriceFact{Path: strings.TrimPrefix(p, "."), Value: f})
					continue
				}
			}
			walk(child, p, depth+1, out)
		}
	case []any:
		for i, child := range n {
			walk(child, fmt.Sprintf("%s[%d]", path, i), depth+1, out)
		}
	}
}

// ScanTotals finds every "total" amount in a JSON document.
func ScanTotals(raw []byte) ([]PriceFact, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var facts []PriceFact
	walk(doc, "", 0, &facts)
	return facts, nil
}

// JudgeTotals turns the totals of a document into a verdict: any total above
// zero means Paid; otherwise at least one zero total means Free.
func JudgeTotals(facts []PriceFact) Verdict {
	if len(facts) == 0 {
		return Unknown
	}
	for _, f := range facts {
		if f.Value != 0 {
			return Paid
		}
	}
	return Free
}

// JudgeJSON is ScanTotals followed by JudgeTotals. Text that is not JSON is
// Unknown.
func JudgeJSON(raw []byte) (Verdict, []PriceFact) {
	facts, err := ScanTotals(raw)
	if err != nil {
		return Unknown, nil
	}
	return JudgeTotals(facts), facts
}

// JudgeTotalText judges the text of the checkout's "total" line as shown to
// the buyer, in any locale: "$0.00", "0,00 €", "₹0", "Free".
func JudgeTotalText(s string) Verdict {
	hasDigit, nonZero := false, false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			hasDigit = true
			if r != '0' {
				nonZero = true
			}
		}
	}
	switch {
	case nonZero:
		return Paid
	case hasDigit:
		return Free
	}
	low := strings.ToLower(s)
	for _, w := range []string{"free", "gratis", "gratuit", "kostenlos", "бесплатно", "無料", "免费", "免費", "무료", "darmow", "ücretsiz"} {
		if strings.Contains(low, w) {
			return Free
		}
	}
	return Unknown
}

// Combine merges independent pieces of evidence. One Paid outweighs
// everything; otherwise one Free is enough.
func Combine(vs ...Verdict) Verdict {
	out := Unknown
	for _, v := range vs {
		if v == Paid {
			return Paid
		}
		if v == Free {
			out = Free
		}
	}
	return out
}

// JudgePrices judges the list of every amount of money shown on a checkout
// page. If each one is zero, the page shows nothing but a zero price, which
// is what Epic's free checkout looks like. Anything else is inconclusive:
// an ordinary checkout lists the full price and the discount next to the
// total, so non-zero amounts alone do not mean the total is non-zero.
func JudgePrices(prices []string) Verdict {
	if len(prices) == 0 {
		return Unknown
	}
	for _, p := range prices {
		if JudgeTotalText(p) != Free {
			return Unknown
		}
	}
	return Free
}
