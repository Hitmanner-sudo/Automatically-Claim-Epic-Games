// Package claim drives Epic's checkout inside the launcher's own embedded
// browser, over the DevTools protocol, to add a free game to the library.
//
// It never enters credentials, never solves or bypasses CAPTCHAs, and never
// lets an order leave the machine unless there is positive evidence that its
// total is zero.
package claim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
)

// Outcome of trying to claim one offer.
type Outcome string

const (
	// Claimed: the order went through.
	Claimed Outcome = "claimed"
	// Owned: the account already had it.
	Owned Outcome = "owned"
	// Unavailable: this account cannot get the offer (region, missing base
	// game, parental controls). Retrying will not help.
	Unavailable Outcome = "unavailable"
	// Captcha: Epic asked for a human check. Nothing was done; retry later.
	Captcha Outcome = "captcha"
	// Throttled: Epic says the account must wait before claiming more.
	Throttled Outcome = "throttled"
	// NotSignedIn: the launcher (or its web session) is signed out.
	NotSignedIn Outcome = "not-signed-in"
	// NotFree: the checkout showed a price above zero. Nothing was ordered.
	NotFree Outcome = "not-free"
	// DryRun: everything was checked but the order was deliberately not placed.
	DryRun Outcome = "dry-run"
	// Failed: something else went wrong; worth retrying later.
	Failed Outcome = "failed"
)

// Result is the outcome with a human-readable explanation for the log.
type Result struct {
	Outcome Outcome
	Detail  string
}

func (r Result) String() string {
	if r.Detail == "" {
		return string(r.Outcome)
	}
	return string(r.Outcome) + ": " + r.Detail
}

// ErrNotSignedIn means the launcher is showing its sign-in page.
var ErrNotSignedIn = errors.New("the Epic Games Launcher is not signed in (open it once and sign in with \"Remember me\" ticked)")

// Options tune a session. The zero value is ready for production use.
type Options struct {
	// DryRun checks everything but does not place orders.
	DryRun bool
	// DiagDir, when set, receives a folder of evidence (screenshot, page,
	// network notes) for every attempt that did not succeed.
	DiagDir string
	// Logf receives progress lines.
	Logf func(format string, args ...any)

	// CheckoutBases are the addresses under which Epic serves its checkout,
	// tried in order. The first is the one the launcher itself uses; the
	// second is one the launcher attaches its own sign-in to; the last is
	// the public web store. Overridden by tests.
	CheckoutBases []string
	// LoginBase is Epic's sign-in site; overridden by tests.
	LoginBase string

	// OpenProduct, when set, asks the launcher itself to show an offer's
	// store page (try counts the attempts, starting at 0, so that different
	// link forms can be tried). It enables the fallback of claiming the way
	// a person does: on the store page, through the launcher's own
	// checkout window.
	OpenProduct func(ctx context.Context, o feed.Offer, try int) error

	// BrowserCheckWait is how long a "checking your browser" page is given
	// to pass by itself.
	BrowserCheckWait time.Duration

	UIWait       time.Duration // for the launcher's interface to come up
	LoginGrace   time.Duration // how long a lone sign-in page is tolerated
	SettleWait   time.Duration // how long to hold out for an Epic web page
	CheckoutWait time.Duration // for the checkout page to become ready
	OrderWait    time.Duration // for the order to be confirmed
}

func (o *Options) fill() {
	if len(o.CheckoutBases) == 0 {
		o.CheckoutBases = []string{
			"https://launcher.store.epicgames.com",
			"https://www.epicgames.com/store",
			"https://store.epicgames.com",
		}
	}
	bases := make([]string, 0, len(o.CheckoutBases))
	for _, b := range o.CheckoutBases {
		bases = append(bases, strings.TrimRight(b, "/"))
	}
	o.CheckoutBases = bases
	if o.LoginBase == "" {
		o.LoginBase = "https://www.epicgames.com"
	}
	o.LoginBase = strings.TrimRight(o.LoginBase, "/")
	if o.UIWait == 0 {
		o.UIWait = 4 * time.Minute
	}
	if o.LoginGrace == 0 {
		o.LoginGrace = 40 * time.Second
	}
	if o.SettleWait == 0 {
		o.SettleWait = 45 * time.Second
	}
	if o.CheckoutWait == 0 {
		o.CheckoutWait = 50 * time.Second
	}
	if o.BrowserCheckWait == 0 {
		o.BrowserCheckWait = 150 * time.Second
	}
	if o.OrderWait == 0 {
		o.OrderWait = 90 * time.Second
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// storeClientID is the public OAuth client of Epic's web store; the sign-in
// page uses it to hand an existing session over to the store.
const storeClientID = "875a3b57d3a640a6b7f9b4e883463ab4"

// ---------------------------------------------------------------- targets

func isLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "epicgames.com" && !strings.HasSuffix(host, ".epicgames.com") {
		return false
	}
	return strings.HasPrefix(u.Path, "/id/login") || strings.HasPrefix(u.Path, "/id/register") || u.Path == "/id" || u.Path == "/id/"
}

// isStoreHost reports whether a host (with optional port) is one of Epic's
// store sites: the public one, the launcher's own, or a configured checkout
// origin. A checkout address with a path (such as www.epicgames.com/store)
// does not make its whole host a store site.
func isStoreHost(hostport string, bases []string) bool {
	name := strings.ToLower(hostport)
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	if name == "store.epicgames.com" || strings.HasSuffix(name, ".store.epicgames.com") {
		return true
	}
	for _, b := range bases {
		if u, err := url.Parse(b); err == nil && strings.Trim(u.Path, "/") == "" && strings.EqualFold(u.Host, hostport) {
			return true
		}
	}
	return false
}

// storeOrigin returns the origin of a page if it belongs to Epic's store,
// or "" otherwise. Only such pages may query the store's API.
func storeOrigin(pageURL string, bases []string) string {
	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !isStoreHost(u.Host, bases) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// targetScore ranks a debuggable target as a place to run the checkout in.
// Negative means unusable.
func targetScore(t cdp.Target, bases []string) int {
	if t.Type != "page" || t.WebSocketDebuggerURL == "" {
		return -1
	}
	if isLoginURL(t.URL) {
		return -1
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return 0
	}
	host := strings.ToLower(u.Hostname())
	if isStoreHost(u.Host, bases) {
		return 3
	}
	if host == "epicgames.com" || strings.HasSuffix(host, ".epicgames.com") {
		return 2
	}
	return 1
}

// survey summarises the launcher's pages.
type survey struct {
	Best      cdp.Target
	BestScore int
	Login     bool // a sign-in page is showing
	Pages     int
}

func surveyTargets(ts []cdp.Target, bases []string) survey {
	s := survey{BestScore: -1}
	for _, t := range ts {
		if t.Type != "page" {
			continue
		}
		s.Pages++
		if isLoginURL(t.URL) {
			s.Login = true
		}
		if sc := targetScore(t, bases); sc > s.BestScore {
			s.Best, s.BestScore = t, sc
		}
	}
	return s
}

// ---------------------------------------------------------------- session

// Session is a connection to one launcher page, through which offers are
// claimed one after another.
type Session struct {
	base string
	opt  Options
	// page is the launcher page currently being driven: the one the session
	// attached to first (home), or a checkout window the launcher opened.
	page *cdp.Page

	mu    sync.Mutex
	watch *orderWatch

	home *tab
	tabs []*tab // every attached page, home first
}

// tab is one attached launcher page with its own request supervision.
type tab struct {
	id   string
	page *cdp.Page
	done chan struct{}
}

// fetchPatterns select the requests that pass through this program before
// they leave the browser (see orderWatch).
func fetchPatterns() []map[string]any {
	patterns := []map[string]any{}
	for _, p := range []string{"*order*", "*Order*", "*checkout*", "*purchase*"} {
		patterns = append(patterns,
			map[string]any{"urlPattern": p, "requestStage": "Request"},
			map[string]any{"urlPattern": p, "requestStage": "Response"},
		)
	}
	return patterns
}

// attach connects to a launcher page and puts its order-related requests
// under supervision. A page whose requests cannot be supervised is not used.
func (s *Session) attach(ctx context.Context, t cdp.Target) (*tab, error) {
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page, err := cdp.OpenPage(pctx, t)
	if err != nil {
		return nil, fmt.Errorf("attach to launcher page: %w", err)
	}
	nt := &tab{id: t.ID, page: page, done: make(chan struct{})}
	go s.eventLoop(nt)
	if err := page.Call(pctx, "Page.enable", nil, nil); err != nil {
		_ = page.Close()
		<-nt.done
		return nil, fmt.Errorf("attach to launcher page: %w", err)
	}
	if err := page.Call(pctx, "Fetch.enable", map[string]any{"patterns": fetchPatterns()}, nil); err != nil {
		_ = page.Close()
		<-nt.done
		return nil, fmt.Errorf("cannot supervise the checkout's network requests, so nothing will be ordered: %w", err)
	}
	s.mu.Lock()
	s.tabs = append(s.tabs, nt)
	s.mu.Unlock()
	return nt, nil
}

func (s *Session) attached(id string) *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tabs {
		if t.id == id {
			return t
		}
	}
	return nil
}

// goHome drops every page adopted along the way and returns to the page the
// session started on.
func (s *Session) goHome() {
	s.mu.Lock()
	extra := append([]*tab(nil), s.tabs[1:]...)
	s.tabs = s.tabs[:1]
	s.mu.Unlock()
	for _, t := range extra {
		_ = t.page.Close()
		<-t.done
	}
	s.page = s.home.page
}

// isCheckoutURL reports whether an address is one of Epic's checkout pages.
func isCheckoutURL(raw string, bases []string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	epic := host == "epicgames.com" || strings.HasSuffix(host, ".epicgames.com") || isStoreHost(u.Host, bases)
	if !epic {
		for _, b := range bases {
			if bu, err := url.Parse(b); err == nil && strings.EqualFold(bu.Host, u.Host) {
				epic = true
			}
		}
	}
	path := strings.ToLower(u.Path)
	return epic && (strings.Contains(path, "/purchase") || strings.Contains(path, "/checkout"))
}

// targetIDs lists the launcher's current pages.
func (s *Session) targetIDs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if targets, err := cdp.Targets(tctx, s.base); err == nil {
		for _, t := range targets {
			out[t.ID] = true
		}
	}
	return out
}

// adoptCheckout looks for a checkout window the launcher has opened by
// itself (it does so when its store asks for a purchase) and, if there is
// one for this offer, continues there. known holds the pages that existed
// before; a page from an earlier attempt is only taken if it shows this
// very offer.
func (s *Session) adoptCheckout(ctx context.Context, o feed.Offer, known map[string]bool) bool {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	targets, err := cdp.Targets(tctx, s.base)
	cancel()
	if err != nil {
		return false
	}
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" || s.attached(t.ID) != nil || !isCheckoutURL(t.URL, s.opt.CheckoutBases) {
			continue
		}
		if known[t.ID] && !strings.Contains(t.URL, o.OfferID) {
			continue
		}
		nt, err := s.attach(ctx, t)
		if err != nil {
			s.opt.Logf("%s: a checkout window appeared but could not be used: %v", o.Title, err)
			continue
		}
		s.page = nt.page
		return true
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Open waits for the launcher's interface to come up behind the DevTools
// endpoint at base, and attaches to the page best suited for the checkout.
func Open(ctx context.Context, base string, opt Options) (*Session, error) {
	opt.fill()
	deadline := time.Now().Add(opt.UIWait)
	var loginSince, plainSince time.Time
	var chosen cdp.Target
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		targets, err := cdp.Targets(tctx, base)
		cancel()
		now := time.Now()
		if err == nil {
			sv := surveyTargets(targets, opt.CheckoutBases)
			switch {
			case sv.BestScore >= 2:
				chosen = sv.Best
			case sv.Login:
				// Only a sign-in page: the launcher is signed out. Allow a
				// little time in case it is still signing in on its own.
				if loginSince.IsZero() {
					loginSince = now
				}
				if now.Sub(loginSince) > opt.LoginGrace {
					return nil, ErrNotSignedIn
				}
			case sv.BestScore >= 0:
				// Pages exist but none is an Epic web page (yet). Settle for
				// the best one if nothing better shows up.
				loginSince = time.Time{}
				if plainSince.IsZero() {
					plainSince = now
				}
				if now.Sub(plainSince) > opt.SettleWait {
					chosen = sv.Best
				}
			default:
				loginSince = time.Time{}
			}
			if !sv.Login {
				loginSince = time.Time{}
			}
		}
		if chosen.WebSocketDebuggerURL != "" {
			break
		}
		if now.After(deadline) {
			if err != nil {
				return nil, fmt.Errorf("the launcher's interface did not come up: %w", err)
			}
			return nil, errors.New("the launcher's interface did not come up in time")
		}
		if err := sleep(ctx, 250*time.Millisecond); err != nil {
			return nil, err
		}
	}

	opt.Logf("driving launcher page %q (%s)", chosen.Title, trimURL(chosen.URL))
	s := &Session{base: base, opt: opt}
	home, err := s.attach(ctx, chosen)
	if err != nil {
		return nil, err
	}
	s.home, s.page = home, home.page
	return s, nil
}

// Close detaches from the launcher.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	tabs := append([]*tab(nil), s.tabs...)
	s.tabs = nil
	s.mu.Unlock()
	for _, t := range tabs {
		_ = t.page.Close()
		<-t.done
	}
}

func (s *Session) currentWatch() *orderWatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watch
}

func (s *Session) setWatch(w *orderWatch) {
	s.mu.Lock()
	s.watch = w
	s.mu.Unlock()
}

type pausedRequest struct {
	RequestID string `json:"requestId"`
	Request   struct {
		URL         string `json:"url"`
		Method      string `json:"method"`
		PostData    string `json:"postData"`
		HasPostData bool   `json:"hasPostData"`
	} `json:"request"`
	ResponseStatusCode  *int   `json:"responseStatusCode"`
	ResponseErrorReason string `json:"responseErrorReason"`
}

// eventLoop answers every paused request. A request the browser has paused
// stays paused until answered, so this must never block on anything else.
func (s *Session) eventLoop(t *tab) {
	defer close(t.done)
	for {
		select {
		case <-t.page.Done():
			return
		case ev := <-t.page.Events():
			if ev.Method != "Fetch.requestPaused" {
				continue
			}
			var p pausedRequest
			if json.Unmarshal(ev.Params, &p) != nil || p.RequestID == "" {
				continue
			}
			s.answer(t.page, p)
		}
	}
}

func (s *Session) answer(page *cdp.Page, p pausedRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w := s.currentWatch()
	id := map[string]any{"requestId": p.RequestID}

	if p.ResponseStatusCode != nil || p.ResponseErrorReason != "" {
		// Only the replies to order previews and order submissions are read;
		// everything else that merely matched the pattern passes untouched.
		if w != nil && p.ResponseStatusCode != nil && (isOrderPreview(p.Request.URL) || isOrderSubmit(p.Request.Method, p.Request.URL)) {
			var body struct {
				Body          string `json:"body"`
				Base64Encoded bool   `json:"base64Encoded"`
			}
			if err := page.Call(ctx, "Fetch.getResponseBody", id, &body); err == nil {
				w.onResponse(p.Request.Method, p.Request.URL, *p.ResponseStatusCode, decodeBody(body.Body, body.Base64Encoded))
			} else {
				w.onResponse(p.Request.Method, p.Request.URL, *p.ResponseStatusCode, nil)
			}
		}
		_ = page.Call(ctx, "Fetch.continueRequest", id, nil)
		return
	}

	allow := true
	if w != nil {
		allow = w.onRequest(p.Request.Method, p.Request.URL, p.Request.PostData, p.Request.HasPostData)
	}
	if allow {
		_ = page.Call(ctx, "Fetch.continueRequest", id, nil)
		return
	}
	_ = page.Call(ctx, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"}, nil)
}

func trimURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if len(u) > 120 {
		u = u[:120] + "…"
	}
	return u
}

func (s *Session) inspect(ctx context.Context) (pageState, error) {
	var st pageState
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := s.page.Eval(cctx, inspectJS, &st)
	return st, err
}

// checkoutURL is the address of Epic's checkout for one offer under base.
func checkoutURL(base string, o feed.Offer) string {
	return base + "/purchase?highlightColor=0078f2&offers=" + url.QueryEscape(o.PurchaseToken()) + "&orderId&purchaseToken&showNavigation=true"
}

// signInHopURL goes through Epic's sign-in page, which, when the browser
// already has a session, passes it on to the store and continues straight to
// the checkout. When there is no session it shows the sign-in form.
func (s *Session) signInHopURL(o feed.Offer) string {
	q := url.Values{}
	q.Set("noHostRedirect", "true")
	q.Set("redirectUrl", checkoutURL(s.opt.CheckoutBases[len(s.opt.CheckoutBases)-1], o))
	q.Set("client_id", storeClientID)
	return s.opt.LoginBase + "/id/login?" + q.Encode()
}

// ownership asks the store whether the account owns the offer.
type ownership int

const (
	ownUnknown ownership = iota
	ownNo
	ownYes
	ownBlocked // prerequisites missing or conflicting: cannot be bought
)

func (s *Session) ownership(ctx context.Context, st pageState, o feed.Offer) ownership {
	origin := storeOrigin(st.URL, s.opt.CheckoutBases)
	if origin == "" {
		return ownUnknown // only the store's own pages may ask its API
	}
	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.page.Eval(cctx, ownershipJS(origin, o.Namespace, o.OfferID), &res); err != nil || res.Status != 200 {
		return ownUnknown
	}
	return parseOwnership([]byte(res.Body))
}

func parseOwnership(body []byte) ownership {
	var doc struct {
		Data *struct {
			Entitlements *struct {
				Validation *struct {
					FullyOwned    []json.RawMessage `json:"fullyOwnedOffers"`
					MissingPrereq []json.RawMessage `json:"missingPrerequisites"`
					Conflicting   []json.RawMessage `json:"conflictingOffers"`
					PartialUpg    []json.RawMessage `json:"unablePartiallyUpgradeOffers"`
				} `json:"cartOffersValidation"`
			} `json:"Entitlements"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Data == nil || doc.Data.Entitlements == nil || doc.Data.Entitlements.Validation == nil {
		return ownUnknown
	}
	v := doc.Data.Entitlements.Validation
	switch {
	case len(v.FullyOwned) > 0:
		return ownYes
	case len(v.MissingPrereq) > 0 || len(v.Conflicting) > 0 || len(v.PartialUpg) > 0:
		return ownBlocked
	}
	return ownNo
}

// parsePageMapping extracts the offer a store page sells.
func parsePageMapping(body []byte) (namespace, offerID string, ok bool) {
	var doc struct {
		Data *struct {
			StorePageMapping *struct {
				Mapping *struct {
					Mappings *struct {
						Offer *struct {
							ID        string `json:"id"`
							Namespace string `json:"namespace"`
						} `json:"offer"`
					} `json:"mappings"`
				} `json:"mapping"`
			} `json:"StorePageMapping"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Data == nil || doc.Data.StorePageMapping == nil ||
		doc.Data.StorePageMapping.Mapping == nil || doc.Data.StorePageMapping.Mapping.Mappings == nil ||
		doc.Data.StorePageMapping.Mapping.Mappings.Offer == nil {
		return "", "", false
	}
	of := doc.Data.StorePageMapping.Mapping.Mappings.Offer
	if of.ID == "" || of.Namespace == "" {
		return "", "", false
	}
	return of.Namespace, of.ID, true
}

// resolve finds the real offer behind a holiday "mystery game" slot, whose
// identifiers in the free-games list are stand-ins, by asking the store
// which offer the revealed game's page sells.
func (s *Session) resolve(ctx context.Context, o feed.Offer) (feed.Offer, error) {
	if o.Slug == "" {
		return o, errors.New("the mystery game has not been revealed yet")
	}
	var lastErr error = errors.New("no store site is configured")
	for _, base := range s.opt.CheckoutBases {
		origin := storeOrigin(base+"/", s.opt.CheckoutBases)
		if origin == "" || origin != base {
			continue // only a store site itself can answer
		}
		nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = s.page.Navigate(nctx, origin+"/")
		cancel()
		deadline := time.Now().Add(25 * time.Second)
		for {
			st, err := s.inspect(ctx)
			if err == nil && storeOrigin(st.URL, s.opt.CheckoutBases) == origin && st.Ready != "loading" {
				var res struct {
					Status int    `json:"status"`
					Body   string `json:"body"`
				}
				cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				err = s.page.Eval(cctx, pageMappingJS(origin, o.Slug), &res)
				cancel()
				if err == nil {
					if ns, id, ok := parsePageMapping([]byte(res.Body)); ok {
						o.Namespace, o.OfferID, o.Placeholder = ns, id, false
						return o, nil
					}
					lastErr = fmt.Errorf("%s did not say which offer the page %q sells (HTTP %d)", origin, o.Slug, res.Status)
				} else {
					lastErr = err
				}
				break
			}
			if ctx.Err() != nil {
				return o, ctx.Err()
			}
			if time.Now().After(deadline) {
				lastErr = fmt.Errorf("%s did not load", origin)
				break
			}
			if err := sleep(ctx, 500*time.Millisecond); err != nil {
				return o, err
			}
		}
	}
	return o, lastErr
}

// Claim tries to add one free offer to the library.
func (s *Session) Claim(ctx context.Context, o feed.Offer) Result {
	if o.Namespace == "" || o.OfferID == "" {
		return Result{Failed, "offer has no identifiers"}
	}
	if o.DiscountPrice != 0 {
		return Result{NotFree, "the free-games feed does not list this offer at zero"}
	}
	if !o.End.IsZero() && time.Until(o.End) < 2*time.Minute {
		return Result{Failed, "the promotion is about to end; not starting an order"}
	}
	if o.Placeholder {
		resolved, err := s.resolve(ctx, o)
		if err != nil {
			return Result{Failed, "could not look up the revealed mystery game: " + err.Error()}
		}
		s.opt.Logf("%s: the mystery game slot resolves to offer %s", o.Title, resolved.PurchaseToken())
		o = resolved
	}

	w := newOrderWatch(s.opt.DryRun)
	s.setWatch(w)
	defer s.setWatch(nil)
	// Whatever window the launcher opens for this offer is left behind
	// afterwards; the next offer starts from the first page again.
	defer s.goHome()

	// 1. Open Epic's checkout for the offer directly, at each of the
	//    addresses it is served under.
	var res Result
	var last pageState
	next := true
	for i, base := range s.opt.CheckoutBases {
		if !next || ctx.Err() != nil {
			break
		}
		if i > 0 {
			s.opt.Logf("%s: checkout did not open at %s (%s); trying %s", o.Title, s.opt.CheckoutBases[i-1], res.Detail, base)
		}
		s.goHome()
		res, last, next = s.checkout(ctx, o, w, checkoutURL(base, o), true)
	}
	// 2. The same through Epic's sign-in page, which hands an existing web
	//    session over to the store.
	if next && ctx.Err() == nil {
		s.opt.Logf("%s: checkout did not open directly (%s); retrying through Epic's sign-in hand-over", o.Title, res.Detail)
		s.goHome()
		res, last, next = s.checkout(ctx, o, w, s.signInHopURL(o), true)
	}
	// 3. The way a person does it: the launcher shows the game's store
	//    page, "Get" is pressed there, and the launcher opens its own
	//    checkout window.
	if next && ctx.Err() == nil && s.opt.OpenProduct != nil && o.Slug != "" {
		s.opt.Logf("%s: no checkout could be opened directly (%s); going through the game's store page instead", o.Title, res.Detail)
		s.goHome()
		res, last = s.viaStorePage(ctx, o, w)
	}
	if res.Outcome != Claimed && res.Outcome != Owned {
		s.saveDiagnostics(o, res, last, w)
	}
	return res
}

var (
	reCaptchaText = regexp.MustCompile(`(?i)captcha|security check|human`)
	reOwnedText   = regexp.MustCompile(`(?i)already own`)
	reRegionText  = regexp.MustCompile(`(?i)unavailable in your region|not available in your (region|country)`)
)

func classifyError(msg string) (Outcome, bool) {
	switch {
	case reOwnedText.MatchString(msg):
		return Owned, true
	case reRegionText.MatchString(msg):
		return Unavailable, true
	case reCaptchaText.MatchString(msg):
		return Captcha, true
	}
	return "", false
}

// checkout runs one attempt at the given checkout address. tryNext reports
// that no working checkout came up there (signed out, an error page, or no
// checkout at all) and that another address should be tried; res then says
// why. It is only ever true when mayFallThrough is.
func (s *Session) checkout(ctx context.Context, o feed.Offer, w *orderWatch, address string, mayFallThrough bool) (res Result, last pageState, tryNext bool) {
	known := s.targetIDs(ctx)
	nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := s.page.Navigate(nctx, address)
	cancel()
	if err != nil && ctx.Err() != nil {
		return Result{Failed, "interrupted"}, last, false
	}
	// A navigation error is not conclusive (redirects can abort the first
	// request); what the page ends up showing decides.
	return s.order(ctx, o, w, mayFallThrough, known)
}

// order takes it from a checkout that is loading (in the current page, or
// in a window the launcher opens for it) to a placed order.
func (s *Session) order(ctx context.Context, o feed.Offer, w *orderWatch, mayFallThrough bool, known map[string]bool) (res Result, last pageState, tryNext bool) {
	// ---- wait for the checkout to be ready
	deadline := time.Now().Add(s.opt.CheckoutWait)
	var errorSince, strangerSince, lastAdopt time.Time
	adopted, sawBrowserCheck := false, false
	for {
		if ctx.Err() != nil {
			return Result{Failed, "interrupted"}, last, false
		}
		// The launcher may answer a checkout address by opening its own
		// checkout window instead of loading it in place.
		if !adopted && time.Since(lastAdopt) > 2*time.Second {
			lastAdopt = time.Now()
			if s.adoptCheckout(ctx, o, known) {
				adopted = true
				s.opt.Logf("%s: the launcher opened its own checkout window; continuing there", o.Title)
				errorSince, strangerSince = time.Time{}, time.Time{}
				deadline = time.Now().Add(s.opt.CheckoutWait)
			}
		}
		st, ierr := s.inspect(ctx)
		if ierr == nil {
			last = st
			switch {
			case st.Login:
				return Result{NotSignedIn, "Epic's web sign-in page came up instead of the checkout"}, last, mayFallThrough
			case st.Owned || w.ownedHint():
				// "Owned" ends all further attempts, so a second opinion is
				// taken from the store when it will give one.
				if s.ownership(ctx, st, o) == ownNo {
					return Result{Failed, "the checkout says this is already owned but the store says it is not; will look again later"}, last, false
				}
				return Result{Owned, "the checkout says this is already in the library"}, last, false
			case st.Region || w.regionHint():
				return Result{Unavailable, "not available in this account's region"}, last, false
			case st.Pin:
				return Result{Unavailable, "the account asks for a parental-control PIN, which this program does not handle"}, last, false
			case st.Throttled:
				return Result{Throttled, "Epic says this account cannot claim more free games right now"}, last, false
			case st.Captcha:
				return Result{Captcha, "Epic asked for a human check before the order"}, last, false
			}
			if st.BrowserCheck && !sawBrowserCheck {
				// Epic's site is checking the browser (a Cloudflare page).
				// It usually passes by itself within a couple of minutes;
				// nothing is clicked meanwhile.
				sawBrowserCheck = true
				s.opt.Logf("%s: Epic's site is running its browser check; waiting for it to pass", o.Title)
				if d := time.Now().Add(s.opt.BrowserCheckWait); d.After(deadline) {
					deadline = d
				}
			}
			if st.Place.clickable() && !st.BrowserCheck {
				break
			}
			if st.AppError != "" && st.Place == nil {
				if errorSince.IsZero() {
					errorSince = time.Now()
				}
				if time.Since(errorSince) > 6*time.Second {
					return Result{Failed, "the checkout shows an error: " + st.AppError}, last, mayFallThrough
				}
			} else {
				errorSince = time.Time{}
			}
			// A fully loaded page that is not a checkout at all (a "not
			// found" page, the store's front page after a redirect, ...).
			if st.Ready == "complete" && !st.App && !st.BrowserCheck {
				if strangerSince.IsZero() {
					strangerSince = time.Now()
				}
				if mayFallThrough && time.Since(strangerSince) > 12*time.Second {
					return Result{Failed, "no checkout at " + trimURL(st.URL)}, last, true
				}
			} else {
				strangerSince = time.Time{}
			}
		}
		if time.Now().After(deadline) {
			if last.BrowserCheck {
				// Backs off like a CAPTCHA: asking again soon would not help.
				return Result{Captcha, "Epic's site kept showing its browser check instead of the checkout"}, last, false
			}
			if ierr != nil {
				return Result{Failed, "the checkout page could not be read: " + ierr.Error()}, last, mayFallThrough
			}
			if last.App && last.AppError == "" && len(last.Buttons) > 0 {
				// A checkout did come up; another address would look the same.
				return Result{Failed, fmt.Sprintf("the checkout loaded but its order button was not recognised or never became ready (buttons on the page: %q)", last.Buttons)}, last, false
			}
			return Result{Failed, "the checkout did not become ready (page: " + trimURL(last.URL) + ")"}, last, mayFallThrough
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return Result{Failed, "interrupted"}, last, false
		}
	}

	// ---- what does the account already have?
	switch s.ownership(ctx, last, o) {
	case ownYes:
		return Result{Owned, "already in the library"}, last, false
	case ownBlocked:
		return Result{Unavailable, "the store says this account cannot get the offer (a required base game is missing, or it conflicts with something owned)"}, last, false
	}

	// ---- is it really free?
	var evidence []string
	dom := Unknown
	for _, t := range last.Totals {
		v := JudgeTotalText(t)
		dom = Combine(dom, v)
		if v != Unknown {
			evidence = append(evidence, fmt.Sprintf("page shows %q (%v)", t, v))
		}
	}
	preload := Unknown
	if last.Preload != "" {
		var facts []PriceFact
		preload, facts = JudgeJSON([]byte(last.Preload))
		if preload != Unknown {
			evidence = append(evidence, fmt.Sprintf("page data totals %v (%v)", facts, preload))
		}
	}
	preview := w.previewVerdict()
	if preview != Unknown {
		evidence = append(evidence, fmt.Sprintf("order preview (%v)", preview))
	}
	// Epic's newer free checkout has no "total" line: it shows the price
	// (zero) and says "This is free".
	shown := JudgePrices(last.Prices)
	if shown == Free {
		evidence = append(evidence, fmt.Sprintf("the only price on the page is %q", last.Prices[0]))
	}
	// Epic's sentence "This is free" counts only while no amount above zero
	// appears anywhere on the page.
	stated := Unknown
	if last.SaysFree && (len(last.Prices) == 0 || shown == Free) {
		stated = Free
		evidence = append(evidence, "the page says it is free")
	}
	pre := Combine(dom, preload, preview, shown, stated)
	if pre == Paid {
		return Result{NotFree, "the checkout does not show a zero total; nothing was ordered: " + strings.Join(evidence, "; ")}, last, false
	}
	if pre != Free {
		// No button is pressed without positive evidence of a zero price.
		return Result{Failed, fmt.Sprintf("the price could not be confirmed to be zero from the checkout page (amounts seen: %q); nothing was ordered", last.Prices)}, last, false
	}
	w.setPreClick(pre)
	how := ""
	if last.Guessed {
		how = "; the order button was identified by elimination, not by its label"
	}
	s.opt.Logf("%s: checkout ready, price check = %v [%s]; button %q%s", o.Title, pre, strings.Join(evidence, "; "), last.Place.Text, how)

	if s.opt.DryRun {
		return Result{DryRun, fmt.Sprintf("checkout reached and \"%s\" is ready; price check = %v; order not placed (dry run)", last.Place.Text, pre)}, last, false
	}

	// ---- place the order
	if err := s.click(ctx, last.Place); err != nil {
		return Result{Failed, "could not press the order button: " + err.Error()}, last, false
	}
	clickedAt := time.Now()
	clicks := 1
	deadline = time.Now().Add(s.opt.OrderWait)
	var lastConsent, lastOwnCheck, errSeen time.Time
	var errText string
	for {
		if ctx.Err() != nil {
			return Result{Failed, "interrupted after the order button was pressed; the library will be checked next time"}, last, false
		}
		if reason := w.blockedReason(); reason != "" {
			if w.sawPaid() {
				return Result{NotFree, reason}, last, false
			}
			return Result{Failed, reason}, last, false
		}
		if ok, msg := w.confirmation(); ok {
			return Result{Claimed, "order confirmed by Epic"}, last, false
		} else if msg != "" {
			if oc, known := classifyError(msg); known {
				return Result{oc, msg}, last, false
			}
			return Result{Failed, "Epic rejected the order: " + msg}, last, false
		}

		st, ierr := s.inspect(ctx)
		if ierr == nil {
			last = st
			switch {
			case st.Success:
				return Result{Claimed, "the checkout shows the order confirmation"}, last, false
			case st.Throttled:
				return Result{Throttled, "Epic says this account cannot claim more free games right now"}, last, false
			case st.Captcha:
				return Result{Captcha, "Epic asked for a human check; the order was not completed"}, last, false
			case st.Owned:
				return Result{Owned, "the checkout says this is already in the library"}, last, false
			case st.Region:
				return Result{Unavailable, "not available in this account's region"}, last, false
			case st.Login:
				return Result{NotSignedIn, "Epic's sign-in page came up during the order"}, last, false
			}
			if st.Consent.clickable() && time.Since(lastConsent) > 2*time.Second {
				// EU/UK accounts must confirm they waive the withdrawal
				// period before the order is accepted.
				lastConsent = time.Now()
				_ = s.click(ctx, st.Consent)
			}
			if len(st.Errors) > 0 {
				if errSeen.IsZero() || st.Errors[0] != errText {
					errSeen, errText = time.Now(), st.Errors[0]
				}
				if oc, known := classifyError(errText); known {
					return Result{oc, errText}, last, false
				}
				if time.Since(errSeen) > 6*time.Second {
					return Result{Failed, "the checkout reports: " + errText}, last, false
				}
			} else {
				errSeen = time.Time{}
			}
			// The first press can land before the page is listening.
			if st.Place.clickable() && st.Consent == nil && !w.submitted() && clicks < 3 && time.Since(clickedAt) > 12*time.Second {
				clicks++
				clickedAt = time.Now()
				_ = s.click(ctx, st.Place)
			}
		}
		if time.Since(lastOwnCheck) > 4*time.Second {
			lastOwnCheck = time.Now()
			if s.ownership(ctx, last, o) == ownYes {
				return Result{Claimed, "now in the library"}, last, false
			}
		}
		if time.Now().After(deadline) {
			if w.submitted() {
				return Result{Failed, "the order was sent but no confirmation arrived in time; the library will be checked next time"}, last, false
			}
			return Result{Failed, "pressing the order button had no effect"}, last, false
		}
		if err := sleep(ctx, 400*time.Millisecond); err != nil {
			return Result{Failed, "interrupted after the order button was pressed; the library will be checked next time"}, last, false
		}
	}
}

// click presses a button the way a person would, falling back to a
// script-driven click if the browser refuses synthetic mouse input.
func (s *Session) click(ctx context.Context, b *btn) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.page.Click(cctx, b.X, b.Y); err == nil {
		return nil
	}
	var ok bool
	if err := s.page.Eval(cctx, fmt.Sprintf("%s(%f, %f)", clickJS, b.X, b.Y), &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("no button at the expected position")
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// saveDiagnostics keeps what the program saw, so that a failed attempt can
// be understood afterwards. Everything stays on this PC.
func (s *Session) saveDiagnostics(o feed.Offer, res Result, last pageState, w *orderWatch) {
	if s.opt.DiagDir == "" {
		return
	}
	name := unsafeName.ReplaceAllString(o.Title, "-")
	if len(name) > 40 {
		name = name[:40]
	}
	dir := filepath.Join(s.opt.DiagDir, time.Now().Format("20060102-150405")+"-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	last.Preload = "" // large, and may carry account details
	summary := map[string]any{
		"offer":   map[string]string{"title": o.Title, "namespace": o.Namespace, "offerId": o.OfferID},
		"outcome": res.Outcome,
		"detail":  res.Detail,
		"page":    last,
		"network": w.records(),
		"dryRun":  s.opt.DryRun,
	}
	if raw, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "summary.json"), raw, 0o600)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if shot, err := s.page.Screenshot(ctx); err == nil && len(shot) > 0 {
		_ = os.WriteFile(filepath.Join(dir, "screenshot.jpg"), shot, 0o600)
	}
	var html string
	if err := s.page.Eval(ctx, `document.documentElement ? document.documentElement.outerHTML.slice(0, 1500000) : ""`, &html); err == nil && html != "" {
		_ = os.WriteFile(filepath.Join(dir, "page.html"), []byte(html), 0o600)
	}
	if targets, err := cdp.Targets(ctx, s.base); err == nil {
		for i := range targets {
			targets[i].WebSocketDebuggerURL = ""
		}
		if raw, err := json.MarshalIndent(targets, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "targets.json"), raw, 0o600)
		}
	}
	s.opt.Logf("%s: details of this attempt were saved to %s", o.Title, dir)
	pruneDiagnostics(s.opt.DiagDir, 12)
}

// pruneDiagnostics keeps only the newest few evidence folders.
func pruneDiagnostics(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	// Names start with a timestamp, so lexical order is chronological.
	for i := 0; i < len(dirs)-keep; i++ {
		_ = os.RemoveAll(filepath.Join(root, dirs[i]))
	}
}
