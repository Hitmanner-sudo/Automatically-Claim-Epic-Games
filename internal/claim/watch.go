package claim

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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

	pressed     bool   // the order button has been pressed
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

var reSubmit = regexp.MustCompile(`(?i)(confirm|place|submit|create|complete|finali[sz]e|process|commit|execute)[-_/]?order|order[-_/]?(confirm|submit|place|complete)|quickpurchase|/orders?/?$|/(checkout|purchase)/(api/)?(complete|confirm|submit|place|finali[sz]e)\b`)

// canChange reports whether a request method can change anything at the
// other end. Reads and the browser's own pre-flight questions cannot;
// everything else, whatever it is called and however it is spelled, can.
func canChange(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD", "OPTIONS":
		return false
	}
	return true
}

// isOrderSubmit reports whether a request address is one that places an order
// (as opposed to previewing or looking one up).
func isOrderSubmit(method, raw string) bool {
	if !canChange(method) {
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

var (
	// rePaymentPath: addresses under which a store takes orders.
	rePaymentPath = regexp.MustCompile(`(?i)/(purchase|purchases|orders?|checkout|pay|payments?|buy|acquire|redeem|claim|transactions?)(/|$)`)
	// reHarmless: requests under those addresses that are known not to
	// order anything (seen on Epic's real checkout before the button is
	// pressed, or self-evidently bookkeeping).
	reHarmless = regexp.MustCompile(`(?i)(order-?preview|get-earn-reward|initiali[sz]e|developer-privacy|eligib\w*|validat\w*|/config|telemetry|/track|/log|metrics?|/rum|/events?)\b`)
)

// looksLikeOrder reports whether a request that is not at a known order
// address could still be one: it changes something, and it goes to a
// payment host or to an address under which orders are taken. Once the
// order button has been pressed, such a request is treated as the order.
func looksLikeOrder(method, raw string) bool {
	if !canChange(method) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	if reHarmless.MatchString(u.Path) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.Contains(host, "payment") || strings.Contains(host, "checkout") || rePaymentPath.MatchString(u.Path)
}

// mayPlaceOrder reports whether a request, seen while no claim is in
// progress, could place an order or states a price: such a request is never
// let out unsupervised.
func mayPlaceOrder(method, rawURL, postData string) bool {
	if !canChange(method) {
		return false
	}
	if isOrderSubmit(method, rawURL) || looksLikeOrder(method, rawURL) {
		return true
	}
	v, _, _ := judgeBody([]byte(postData))
	return v == Paid
}

// judgeBody reads the totals out of a request or response body: JSON (one
// document or several), a form post, a multi-part form, or any of these
// wrapped in base64. readable is false when the body is none of these
// (compressed, binary, some format of its own): what it says about the
// price is then simply not known.
func judgeBody(body []byte) (v Verdict, facts []PriceFact, readable bool) {
	return judgeBodyDepth(body, 0)
}

// isFieldName reports whether s looks like the name of a form field: one to
// a hundred characters, each a letter, a digit or one of "_.-[]:$ ".
//
// (Written out by hand on purpose. As a regular expression,
// `^[\p{L}\p{N}_.\-\[\]:$ ]{1,100}$`, it took almost four megabytes to
// prepare and kept two of them, every time the program started, for a
// check that is made during a claim only. A test holds the two to the same
// answers.)
func isFieldName(s string) bool {
	n := 0
	for _, r := range s {
		if n++; n > 100 {
			return false
		}
		switch r {
		case '_', '.', '-', '[', ']', ':', '$', ' ':
			continue
		}
		// (A byte that is not valid UTF-8 arrives here as U+FFFD, which
		// is neither.)
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return false
		}
	}
	return n >= 1
}

// lastSegment reduces a form field's name to its last part: "order[total]"
// and "order.total" are both "total".
func lastSegment(name string) string {
	name = strings.TrimRight(name, "]")
	if i := strings.LastIndexAny(name, "[."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func judgeBodyDepth(body []byte, depth int) (v Verdict, facts []PriceFact, readable bool) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
	if trimmed == "" {
		return Unknown, nil, true
	}
	if json.Valid([]byte(trimmed)) {
		v, facts = JudgeJSON([]byte(trimmed))
		return v, facts, true
	}
	// Several JSON documents one after another.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		n := 0
		for {
			var doc any
			err := dec.Decode(&doc)
			if err == io.EOF {
				break
			}
			if err != nil {
				return Unknown, nil, false
			}
			n++
			walk(doc, "", 0, &facts)
		}
		return JudgeTotals(facts), facts, n > 0
	}
	if strings.HasPrefix(trimmed, "--") && strings.Contains(trimmed, "Content-Disposition") {
		facts = multipartTotals(trimmed)
		return JudgeTotals(facts), facts, true
	}
	// The same, wrapped in base64.
	if depth < 2 && len(trimmed) >= 8 {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
			if raw, err := enc.DecodeString(trimmed); err == nil {
				if utf8.Valid(raw) {
					if v, facts, ok := judgeBodyDepth(raw, depth+1); ok {
						return v, facts, true
					}
				}
				return Unknown, nil, false
			}
		}
	}
	// A form post: every part must look like "name=value".
	if !strings.Contains(trimmed, "=") {
		return Unknown, nil, false
	}
	vals, err := url.ParseQuery(trimmed)
	if err != nil || len(vals) == 0 {
		return Unknown, nil, false
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if !isFieldName(k) {
			return Unknown, nil, false
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, val := range vals[k] {
			if totalKeys[keyName(lastSegment(k))] {
				if f, ok := totalOf(val); ok {
					facts = append(facts, PriceFact{Path: k, Value: f})
				}
				continue
			}
			// A field that itself carries JSON.
			if t := strings.TrimSpace(val); len(t) > 1 && (t[0] == '{' || t[0] == '[') {
				if _, inner := JudgeJSON([]byte(t)); len(inner) > 0 {
					facts = append(facts, inner...)
				}
			}
		}
	}
	return JudgeTotals(facts), facts, true
}

var reFormField = regexp.MustCompile(`(?is)Content-Disposition:\s*form-data;\s*name="([^"]*)"[^\r\n]*\r?\n(?:[^\r\n]+\r?\n)*\r?\n(.*?)\r?\n--`)

// multipartTotals reads the total-named fields of a multi-part form.
func multipartTotals(body string) []PriceFact {
	var facts []PriceFact
	for _, m := range reFormField.FindAllStringSubmatch(body, -1) {
		name, val := m[1], strings.TrimSpace(m[2])
		if totalKeys[keyName(lastSegment(name))] {
			if f, ok := totalOf(val); ok {
				facts = append(facts, PriceFact{Path: name, Value: f})
			}
			continue
		}
		if len(val) > 1 && (val[0] == '{' || val[0] == '[') {
			if _, inner := JudgeJSON([]byte(val)); len(inner) > 0 {
				facts = append(facts, inner...)
			}
		}
	}
	return facts
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

	if !canChange(method) {
		rec.Action = "allowed"
		return true
	}
	verdict, facts, readable := judgeBody([]byte(postData))
	rec.Verdict, rec.Totals = verdict.String(), facts
	// A request at a known order address is an order. Once the button has
	// been pressed, so is anything else that goes where orders are taken.
	named := isOrderSubmit(method, rawURL)
	submit := named || (w.pressed && looksLikeOrder(method, rawURL))

	refuse := ""
	switch {
	case verdict == Paid:
		w.paid = true
		refuse = fmt.Sprintf("refused to send a request that states a total above zero (%v); nothing was ordered", facts)
	case w.paid || w.preview == Paid || w.blocked != "":
		// Something said "costs money" earlier in this claim, or a request
		// had to be refused. From then on nothing that could change
		// anything is let out, whatever it says of itself and wherever it
		// is going.
		refuse = "refused to send anything further after a total above zero was seen; nothing was ordered"
	case !submit:
		rec.Action = "allowed"
		return true
	case w.dryRun:
		refuse = "refused to send an order during a dry run"
	case (hasPostData && postData == "") || (named && !readable):
		// (For what is an order only by where it goes, a body in a format
		// of its own is let pass; one the browser does not show is not.)
		refuse = "refused to send an order whose content could not be inspected; nothing was ordered"
	case verdict == Free:
		// The order itself says its total is zero.
	case w.preClick == Free || w.preview == Free:
		// The order does not state a total, but the checkout did, and it
		// was zero.
	default:
		refuse = "refused to send an order whose total could not be confirmed to be zero; nothing was ordered"
	}
	if refuse != "" {
		if w.blocked == "" {
			w.blocked = refuse
		}
		rec.Action = "BLOCKED"
		return false
	}
	if !named {
		rec.Action = "allowed (sent to a payment address after the press; nothing in it speaks of a price)"
		return true
	}
	w.sent = true
	rec.Action = "allowed (order)"
	return true
}

var reBadStatus = regexp.MustCompile(`(?i)^(fail|error|declin|reject)`)

// errorOf collects what a JSON reply says went wrong: its error code and
// its error message, whichever fields they sit in. Both are empty when the
// reply reports no error. The same reply always reads the same.
func errorOf(v any, depth int) (code, message string) {
	if depth > 6 {
		return "", ""
	}
	str := func(x any) string {
		switch c := x.(type) {
		case string:
			return strings.TrimSpace(c)
		case float64:
			if c != 0 {
				return strconv.FormatFloat(c, 'f', -1, 64)
			}
		case map[string]any:
			if len(c) == 0 {
				return ""
			}
			if m, ok := c["message"].(string); ok && strings.TrimSpace(m) != "" {
				return strings.TrimSpace(m)
			}
			raw, _ := json.Marshal(c)
			return string(raw)
		case []any:
			if len(c) == 0 {
				return ""
			}
			raw, _ := json.Marshal(c)
			return string(raw)
		case bool:
			if c {
				return "error"
			}
		}
		return ""
	}
	switch n := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// lookup finds the first of the wanted names (in that order) that
		// the reply carries with something in it.
		lookup := func(wanted ...string) string {
			for _, want := range wanted {
				for _, k := range keys {
					if keyName(k) == want {
						if got := str(n[k]); got != "" {
							return got
						}
					}
				}
			}
			return ""
		}
		code = lookup("errorcode", "error", "errors", "ordererror", "numericerrorcode")
		// An error message is an error by itself; a bare "message" is one
		// only next to something else that reports a failure.
		message = lookup("errormessage", "errordescription")
		// Flags of failure count on the reply itself, not on something that
		// merely travels inside it (a reward that was not earned).
		if code == "" && message == "" && depth == 0 {
			for _, k := range keys {
				child := n[k]
				switch keyName(k) {
				case "success", "ok":
					if b, isBool := child.(bool); isBool && !b {
						code = "the reply says it did not succeed"
					}
				case "failed":
					if b, isBool := child.(bool); isBool && b {
						code = "the reply says it failed"
					}
				case "status":
					if t, isText := child.(string); isText && reBadStatus.MatchString(t) {
						code = "status " + t
					}
				}
				if code != "" {
					break
				}
			}
		}
		if code != "" || message != "" {
			if message == "" {
				if m, ok := n["message"].(string); ok {
					message = strings.TrimSpace(m)
				}
			}
			return code, message
		}
		for _, k := range keys {
			if c, m := errorOf(n[k], depth+1); c != "" || m != "" {
				return c, m
			}
		}
	case []any:
		for _, child := range n {
			if c, m := errorOf(child, depth+1); c != "" || m != "" {
				return c, m
			}
		}
	}
	return "", ""
}

// onFenced records a request that a background worker made to an order
// address and that the browser stopped (see fence in claim.go). What such a
// worker sends cannot be held back and checked, so nothing of the kind is
// let out; if it could have changed anything, the claim ends here.
func (w *orderWatch) onFenced(method, rawURL, postData string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := netRecord{Stage: "request", Method: method, URL: shortURL(rawURL), Action: "BLOCKED (from a background worker)"}
	defer func() { w.log = append(w.log, rec) }()
	if !canChange(method) {
		return
	}
	verdict, facts, _ := judgeBody([]byte(postData))
	rec.Verdict, rec.Totals = verdict.String(), facts
	reason := "refused to send a request that a background worker made to an order address (what such a worker sends cannot be checked); nothing was ordered"
	if verdict == Paid {
		w.paid = true
		reason = fmt.Sprintf("refused to send a request, made by a background worker, that states a total above zero (%v); nothing was ordered", facts)
	}
	if w.blocked == "" {
		w.blocked = reason
	}
}

// refuse ends the claim from outside the request rules: from here on
// nothing that could change anything is let out.
func (w *orderWatch) refuse(reason string) {
	w.mu.Lock()
	if w.blocked == "" {
		w.blocked = reason
	}
	w.mu.Unlock()
}

func (w *orderWatch) isPressed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pressed
}

// notDone reports a reply to an order that, without reporting an error,
// says the order is not through: still pending, not complete, more to do.
func notDone(doc any) string {
	n, ok := doc.(map[string]any)
	if !ok {
		return "the reply is not what an accepted order looks like"
	}
	for name, child := range n {
		b, isBool := child.(bool)
		switch keyName(name) {
		case "orderpending", "pending":
			if isBool && b {
				return "the reply says the order is still pending"
			}
		case "ordercomplete", "complete", "completed":
			if isBool && !b {
				return "the reply says the order is not complete"
			}
		case "requiresaction", "actionrequired", "verificationrequired", "redirecturl", "challengeurl":
			if (isBool && b) || (!isBool && child != nil && child != "") {
				return "the reply asks for a further step"
			}
		}
	}
	return ""
}

var (
	// Epic's order service refuses an order for something the account
	// already has with exactly this code (seen on a real account).
	reOwnedCode    = regexp.MustCompile(`(?i)orderprocessor\.purchase_limit_error$`)
	reOwnedMessage = regexp.MustCompile(`(?i)\balready own`)
)

// onResponse records what Epic answered.
func (w *orderWatch) onResponse(method, rawURL string, status int, body []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := netRecord{Stage: "response", Method: method, URL: shortURL(rawURL), Status: status}
	defer func() { w.log = append(w.log, rec) }()

	var doc any
	isJSON := len(body) > 0 && json.Unmarshal(body, &doc) == nil
	code, message := "", ""
	if isJSON {
		code, message = errorOf(doc, 0)
	}

	// What a reply says about the account counts only where it reports an
	// error, and only in the error's own fields: the words can appear
	// anywhere else for other reasons (a description, a translation).
	if isJSON && (code != "" || message != "") && (isOrderPreview(rawURL) || isOrderSubmit(method, rawURL)) {
		if reOwnedCode.MatchString(code) || reOwnedMessage.MatchString(message) || reOwnedMessage.MatchString(code) {
			w.owned = true
			rec.Note = "says already owned"
		}
		if reRegionText.MatchString(message) || reRegionText.MatchString(code) {
			w.region = true
			rec.Note = "says unavailable in region"
		}
	}

	if isOrderPreview(rawURL) {
		v, facts := Unknown, []PriceFact(nil)
		if isJSON {
			v, facts = JudgePreview(body)
		}
		rec.Verdict, rec.Totals = v.String(), facts
		if v == Paid {
			w.paid = true
		}
		w.preview = Combine(w.preview, v)
		return
	}

	if isOrderSubmit(method, rawURL) {
		// The message for people first, the code after it.
		said := message
		switch {
		case said == "":
			said = code
		case code != "" && code != message:
			said += " (" + code + ")"
		}
		switch {
		case said != "":
			w.confirmErr = said
			rec.Note = "order rejected: " + said
		case status < 200 || status >= 300:
			w.confirmErr = fmt.Sprintf("HTTP %d", status)
			rec.Note = "order rejected"
		case !isJSON:
			// Accepted or not cannot be told from this; the library has
			// the last word.
			rec.Note = "reply to the order could not be read"
		default:
			if why := notDone(doc); why != "" {
				rec.Note = "order not confirmed: " + why
				break
			}
			w.confirmedOK = true
			rec.Note = "order accepted" + replyShape(doc)
		}
	}
}

// replyShape names the fields of an accepted order's reply (names only, no
// values), so that the log shows what Epic's confirmation looks like.
func replyShape(doc any) string {
	n, ok := doc.(map[string]any)
	if !ok || len(n) == 0 {
		return ""
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		if len(k) <= 40 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 30 {
		keys = keys[:30]
	}
	return " (reply fields: " + strings.Join(keys, ", ") + ")"
}

// markPressed notes that the order button has been pressed, and what the
// page showed at that moment.
func (w *orderWatch) markPressed(v Verdict) {
	w.mu.Lock()
	w.preClick, w.pressed = v, true
	w.mu.Unlock()
}

// confirmationNote is what the reply to the order looked like, for the log.
func (w *orderWatch) confirmationNote() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.log) - 1; i >= 0; i-- {
		if strings.HasPrefix(w.log[i].Note, "order accepted") {
			return strings.TrimPrefix(w.log[i].Note, "order accepted")
		}
	}
	return ""
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
