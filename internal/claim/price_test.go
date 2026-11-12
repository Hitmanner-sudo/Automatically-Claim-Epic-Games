package claim

import "testing"

func TestJudgeJSON(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want Verdict
	}{
		{"free order preview", `{"orderResponse":{"totalPrice":0,"currency":"USD","lineOffers":[{"unitPrice":2499,"discount":2499,"totalPrice":0,"quantity":1}]},"syncToken":"x"}`, Free},
		{"paid order preview", `{"orderResponse":{"totalPrice":1999,"lineOffers":[{"unitPrice":1999,"totalPrice":1999}]}}`, Paid},
		{"one line still costs money", `{"orderResponse":{"totalPrice":0,"lineOffers":[{"totalPrice":0},{"totalPrice":499}]}}`, Paid},
		{"confirm body free", `{"useDefault":true,"offers":["abc"],"totalAmount":0,"includeAccountBalance":false}`, Free},
		{"confirm body paid", `{"offers":["abc"],"totalAmount":5999}`, Paid},
		{"totals as strings", `{"totalAmount":"0.00"}`, Free},
		{"paid total as string", `{"totalAmount":"19.99"}`, Paid},
		{"original and discount are not totals", `{"originalPrice":2499,"discountPrice":0,"unitPrice":2499,"discount":2499}`, Unknown},
		{"feed-style price object is not a number", `{"price":{"totalPrice":{"discountPrice":0,"originalPrice":2499}}}`, Unknown},
		{"case-insensitive keys", `{"TotalAmount":1}`, Paid},
		{"no price information", `{"hello":"world","quantity":1}`, Unknown},
		{"not json", `<html>`, Unknown},
		{"negative total is not free", `{"totalAmount":-1}`, Paid},
	}
	for _, c := range cases {
		got, facts := JudgeJSON([]byte(c.doc))
		if got != c.want {
			t.Errorf("%s: got %v (facts %v), want %v", c.name, got, facts, c.want)
		}
	}
}

func TestScanTotalsReportsPaths(t *testing.T) {
	facts, err := ScanTotals([]byte(`{"a":{"totalPrice":0},"b":[{"totalAmount":7}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0].Path != "a.totalPrice" || facts[1].Path != "b[0].totalAmount" || facts[1].Value != 7 {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestJudgeTotalText(t *testing.T) {
	cases := map[string]Verdict{
		"Total $0.00":            Free,
		"0,00 €":                 Free,
		"₹0":                     Free,
		"R$ 0,00":                Free,
		"Total":                  Unknown,
		"":                       Unknown,
		"Free":                   Free,
		"Kostenlos":              Free,
		"Total $19.99":           Paid,
		"1 999,00 ₽":             Paid,
		"$0.01":                  Paid,
		"Total 0.00 (was 24.99)": Paid, // anything non-zero on the total line blocks
	}
	for in, want := range cases {
		if got := JudgeTotalText(in); got != want {
			t.Errorf("JudgeTotalText(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCombine(t *testing.T) {
	if Combine() != Unknown || Combine(Unknown, Unknown) != Unknown {
		t.Error("no evidence must stay unknown")
	}
	if Combine(Unknown, Free) != Free {
		t.Error("one free is enough")
	}
	if Combine(Free, Free, Paid) != Paid || Combine(Paid, Free) != Paid {
		t.Error("paid must outweigh everything")
	}
}

func TestJudgePrices(t *testing.T) {
	cases := []struct {
		prices []string
		want   Verdict
	}{
		{nil, Unknown},
		{[]string{"$0.00"}, Free},
		{[]string{"0,00 €", "0,00 €"}, Free},
		{[]string{"$24.99", "$24.99", "$0.00"}, Unknown}, // full price and discount next to a zero total
		{[]string{"$19.99"}, Unknown},
	}
	for _, c := range cases {
		if got := JudgePrices(c.prices); got != c.want {
			t.Errorf("JudgePrices(%q) = %v, want %v", c.prices, got, c.want)
		}
	}
}
