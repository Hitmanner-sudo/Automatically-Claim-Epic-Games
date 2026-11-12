package claim

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// orderWatch supervises the checkout's network traffic during one claim.
//
// It is the last line of defence against spending money: every request that
// could place an order is held until this code has looked at it, and it is
// let through only when there is positive evidence that the order's total is
// zero and no evidence that it is not.
type orderWatch struct {
	mu sync.Mutex

	dryRun   bool
	preClick Verdict // what the page showed before the button was pressed
	preview  Verdict // what Epic's order preview said

	blocked     string // why an order request was refused, if one was
	paid        bool   // a request or response stated a total above zero
	sent        bool   // an order request was let through
	confirmedOK bool
	confirmErr  string

	owned  bool
	region bool

	log []netRecord
}

type netRecord struct {
	Stage   string      `json:"stage"`
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Status  int         `json:"status,omitempty"`
	Verdict string      `json:"verdict,omitempty"`
	Totals  []PriceFact `json:"totals,omitempty"`
	Action  string      `json:"action,omitempty"`
	Note    string      `json:"note,omitempty"`
}

func newOrderWatch(dryRun bool) *orderWatch { return &orderWatch{dryRun: dryRun} }

var reSubmit = regexp.MustCompile(`(?i)(confirm|place|submit|create|complete|finali[sz]e)[-_/]?order|quickpurchase|/orders/?$|/(checkout|purchase)/(api/)?(complete|confirm|submit|place|finali[sz]e)\b`)

// isOrderSubmit reports whether a request address is one that places an order
// (as opposed to previewing or looking one up).
func isOrderSubmit(method, raw string) bool {
	if method != "POST" && method != "PUT" && method != "PATCH" {
		return false
	}
	path := raw
	if u, err := url.Parse(raw); err == nil {
		path = u.Path
	}
	return reSubmit.MatchString(path)
}

func isOrderPreview(raw string) bool {
	return strings.Contains(strings.ToLower(raw), "order-preview") || strings.Contains(strings.ToLower(raw), "orderpreview")
}

// mayPlaceOrder reports whether a request, seen while no claim is in
// progress, could place an order or states a price: such a request is never
// let out unsupervised.
func mayPlaceOrder(method, rawURL, postData string) bool {
	if method != "POST" && method != "PUT" && method != "PATCH" {
		return false
	}
	if isOrderSubmit(method, rawURL) {
		return true
	}
	v, _ := judgeBody([]byte(postData))
	return v == Paid
}

// judgeBody reads the totals out of a request or response body, whether it
// is JSON or a form post.
func judgeBody(body []byte) (Verdict, []PriceFact) {
	if v, facts := JudgeJSON(body); v != Unknown || json.Valid(body) {
		return v, facts
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return Unknown, nil
	}
	var facts []PriceFact
	for k, vs := range vals {
		if !totalKeys[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			if f, ok := asNumber(v); ok {
				facts = append(facts, PriceFact{Path: k, Value: f})
			}
		}
	}
	return JudgeTotals(facts), facts
}

func shortURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) > 160 {
		raw = raw[:160]
	}
	return raw
}

// onRequest decides whether an outgoing request may leave. It returns false
// to have the browser drop it.
func (w *orderWatch) onRequest(method, rawURL, postData string, hasPostData bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := netRecord{Stage: "request", Method: method, URL: shortURL(rawURL)}
	defer func() { w.log = append(w.log, rec) }()

	if method != "POST" && method != "PUT" && method != "PATCH" {
		rec.Action = "allowed"
		return true
	}
	verdict, facts := judgeBody([]byte(postData))
	rec.Verdict, rec.Totals = verdict.String(), facts
	submit := isOrderSubmit(method, rawURL)

	switch {
	case verdict == Paid:
		w.paid = true
		w.blocked = fmt.Sprintf("refused to send a request that states a total above zero (%v); nothing was ordered", facts)
	case !submit:
		rec.Action = "allowed"
		return true
	case w.dryRun:
		w.blocked = "refused to send an order during a dry run"
	case w.paid || w.preview == Paid:
		// Something said "costs money" earlier in this claim. Nothing the
		// order itself says can outweigh that.
		w.blocked = "refused to send an order after a total above zero was seen; nothing was ordered"
	case hasPostData && postData == "":
		w.blocked = "refused to send an order whose content could not be inspected; nothing was ordered"
	case verdict == Free:
		// The order itself says its total is zero.
	case w.preClick == Free || w.preview == Free:
		// The order does not state a total, but the checkout did, and it
		// was zero.
	default:
		w.blocked = "refused to send an order whose total could not be confirmed to be zero; nothing was ordered"
	}
	if w.blocked != "" {
		rec.Action = "BLOCKED"
		return false
	}
	w.sent = true
	rec.Action = "allowed (order)"
	return true
}

var (
	reErrorKey = regexp.MustCompile(`(?i)^(error|errors|errorcode|errormessage|ordererror)$`)
)

// findError looks for a non-empty error field in a JSON reply.
func findError(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch n := v.(type) {
	case map[string]any:
		for k, child := range n {
			if !reErrorKey.MatchString(k) {
				continue
			}
			switch c := child.(type) {
			case string:
				if strings.TrimSpace(c) != "" {
					if m, ok := n["message"].(string); ok && strings.TrimSpace(m) != "" {
						return m
					}
					return c
				}
			case map[string]any:
				if m, ok := c["message"].(string); ok && m != "" {
					return m
				}
				if len(c) > 0 {
					raw, _ := json.Marshal(c)
					return string(raw)
				}
			case []any:
				if len(c) > 0 {
					raw, _ := json.Marshal(c)
					return string(raw)
				}
			case bool:
				if c {
					if m, ok := n["message"].(string); ok && m != "" {
						return m
					}
					return "error"
				}
			}
		}
		for _, child := range n {
			if s := findError(child, depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range n {
			if s := findError(child, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

// onResponse records what Epic answered.
func (w *orderWatch) onResponse(method, rawURL string, status int, body []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := netRecord{Stage: "response", Method: method, URL: shortURL(rawURL), Status: status}
	defer func() { w.log = append(w.log, rec) }()

	var doc any
	isJSON := len(body) > 0 && json.Unmarshal(body, &doc) == nil

	// Only genuine API replies count as statements about the account; a
	// script or translation file that merely contains the words does not.
	if isJSON && (isOrderPreview(rawURL) || isOrderSubmit(method, rawURL)) {
		text := string(body)
		if reOwnedText.MatchString(text) {
			w.owned = true
			rec.Note = "says already owned"
		}
		if reRegionText.MatchString(text) {
			w.region = true
			rec.Note = "says unavailable in region"
		}
	}

	if isOrderPreview(rawURL) {
		v, facts := Unknown, []PriceFact(nil)
		if isJSON {
			v, facts = JudgeJSON(body)
		}
		rec.Verdict, rec.Totals = v.String(), facts
		if v == Paid {
			w.paid = true
		}
		w.preview = Combine(w.preview, v)
		return
	}

	if isOrderSubmit(method, rawURL) {
		msg := ""
		if isJSON {
			msg = findError(doc, 0)
		}
		switch {
		case status >= 200 && status < 300 && msg == "":
			w.confirmedOK = true
			rec.Note = "order accepted"
		case msg != "":
			w.confirmErr = msg
			rec.Note = "order rejected: " + msg
		default:
			w.confirmErr = fmt.Sprintf("HTTP %d", status)
			rec.Note = "order rejected"
		}
	}
}

func (w *orderWatch) setPreClick(v Verdict) {
	w.mu.Lock()
	w.preClick = v
	w.mu.Unlock()
}

func (w *orderWatch) previewVerdict() Verdict {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.preview
}

func (w *orderWatch) blockedReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocked
}

func (w *orderWatch) sawPaid() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paid
}

func (w *orderWatch) submitted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sent
}

// confirmation reports Epic's answer to the order: (true, "") when accepted,
// (false, reason) when rejected, (false, "") when there is no answer yet.
func (w *orderWatch) confirmation() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.confirmedOK, w.confirmErr
}

func (w *orderWatch) ownedHint() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.owned
}

func (w *orderWatch) regionHint() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.region
}

func (w *orderWatch) records() []netRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]netRecord(nil), w.log...)
}

func decodeBody(body string, b64 bool) []byte {
	if !b64 {
		return []byte(body)
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil
	}
	return raw
}
