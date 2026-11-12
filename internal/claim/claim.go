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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// Later: not possible right now, though it may be in a day (a base game
	// is missing from the library, the page says "coming soon").
	Later Outcome = "later"
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
	// Again goes through the checkout also for an offer the account
	// already has, instead of stopping at "already in the library". It is
	// for proving the whole path on a PC: Epic refuses an order for
	// something the account has (and says so), and nothing changes. Every
	// safeguard about the price applies as always.
	Again bool
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

	StorePageWait time.Duration // for "Get" on a store page to open a checkout

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
	if o.StorePageWait == 0 {
		o.StorePageWait = 90 * time.Second
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

	// kids are the frames and workers that the browser attached to a page's
	// connection the moment they were created (see adopt), by session id.
	kids map[string]kid
}

// kid is one frame or worker of an attached page.
type kid struct {
	targetID   string
	kind, url  string
	supervised bool // its requests pass through this program
	fenced     bool // it cannot reach an order address at all (see fence)
	via        *tab // the attached page whose connection it came over
}

// orderWords are what the address of a request that places an order (or
// goes where orders are taken) contains. See fence.
var orderWords = []string{
	"confirm-order", "confirmOrder", "confirm_order", "ConfirmOrder", "place-order", "placeOrder", "place_order", "PlaceOrder",
	"submit-order", "submitOrder", "submit_order", "SubmitOrder", "create-order", "createOrder", "CreateOrder",
	"complete-order", "completeOrder", "CompleteOrder", "quickPurchase", "quickpurchase", "QuickPurchase",
	"/purchase/", "/Purchase/", "/checkout/", "/Checkout/", "/order", "/Order", "/payment", "/Payment", "/pay/",
	"/acquire", "/redeem", "/claim/", "/transaction", "://payment", "://checkout", ".payment", ".checkout.",
}

func reachesOrders(rawURL string) bool {
	for _, w := range orderWords {
		if strings.Contains(rawURL, w) {
			return true
		}
	}
	return false
}

// backgroundWorker reports whether a target is a worker that lives on its
// own (shared between pages, or a service worker) rather than inside one
// page.
func backgroundWorker(kind string) bool { return kind == "shared_worker" || kind == "service_worker" }

// fence is for a background worker that was already running when this
// program got to it. The browser holds back the requests of a frame or of a
// worker it is told about at its start, so that they can be looked at; it
// does not do that for a worker that is already under way. Such a worker is
// instead kept from reaching any order address at all, and what it tries is
// reported (see fencedRequest). Epic's checkout sends its order from the
// page itself, so this takes nothing away from it.
func fence(ctx context.Context, page *cdp.Page, session string) bool {
	if page.CallSession(ctx, session, "Network.enable", nil, nil) != nil {
		return false
	}
	urls := make([]string, 0, len(orderWords))
	for _, w := range orderWords {
		urls = append(urls, "*"+w+"*")
	}
	return page.CallSession(ctx, session, "Network.setBlockedURLs", map[string]any{"urls": urls}, nil) == nil
}

// fencedRequest notes a request of a fenced worker that the browser
// stopped because it was going to an order address.
func (s *Session) fencedRequest(ev cdp.Event) {
	var p struct {
		Request struct {
			URL      string `json:"url"`
			Method   string `json:"method"`
			PostData string `json:"postData"`
		} `json:"request"`
	}
	if json.Unmarshal(ev.Params, &p) != nil || !reachesOrders(p.Request.URL) {
		return
	}
	if w := s.currentWatch(); w != nil {
		w.onFenced(p.Request.Method, p.Request.URL, p.Request.PostData)
	}
}

// isFenced reports whether events of this origin come from a fenced worker.
func (s *Session) isFenced(t *tab, session string) bool {
	if session == "" {
		return t.fenced.Load()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kids[session].fenced
}

// tab is one attached launcher page with its own request supervision.
type tab struct {
	id   string
	page *cdp.Page
	done chan struct{}
	// swBypassed: the browser was told to keep service workers out of this
	// page's network traffic, so that every request is seen here.
	swBypassed bool
	// auto: the browser hands this page's frames and workers over as they
	// are created, each held still until it is supervised.
	auto bool
	// closing: the page is being let go of; nothing that could change
	// anything is let out of it any more.
	closing atomic.Bool
	// fenced: this is a background worker that was already running; it is
	// kept away from order addresses (see fence).
	fenced atomic.Bool
	kind   string // the target's type: page, iframe, worker, ...
	// lanes: one for each frame or worker attached through this page's
	// connection, by session id. Only the page's event loop touches it.
	lanes map[string]*lane
}

// lane handles what concerns one frame or worker attached through a page's
// connection, one thing after another and apart from the page itself. The
// launcher's browser never answers a command sent to a frame that has just
// gone away; whoever waits for such an answer must not be the one who
// answers for the page.
type lane struct {
	session string
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	jobs    []func()
	wake    chan struct{}
	gone    atomic.Bool
}

func (l *lane) push(job func()) {
	l.mu.Lock()
	l.jobs = append(l.jobs, job)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *lane) run() {
	for {
		l.mu.Lock()
		var job func()
		if len(l.jobs) > 0 {
			job, l.jobs = l.jobs[0], l.jobs[1:]
		}
		l.mu.Unlock()
		if l.ctx.Err() != nil {
			return
		}
		if job == nil {
			select {
			case <-l.wake:
			case <-l.ctx.Done():
				return
			}
			continue
		}
		job()
	}
}

var errGone = errors.New("the frame or worker has gone away")

// call sends a command to the lane's frame or worker and waits a short
// while for the answer. One that does not come means it has gone away;
// nothing more is sent to it then.
func (l *lane) call(page *cdp.Page, method string, params, result any) error {
	if l.gone.Load() {
		return errGone
	}
	ctx, cancel := context.WithTimeout(l.ctx, 8*time.Second)
	defer cancel()
	err := page.CallSession(ctx, l.session, method, params, result)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		l.gone.Store(true)
	}
	return err
}

// laneOf returns the lane of a session of this page's connection, starting
// it if need be. Called from the page's event loop only.
func (t *tab) laneOf(session string) *lane {
	if l := t.lanes[session]; l != nil {
		return l
	}
	if t.lanes == nil {
		t.lanes = map[string]*lane{}
	}
	l := &lane{session: session, wake: make(chan struct{}, 1)}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	t.lanes[session] = l
	go l.run()
	return l
}

func (t *tab) isWorker() bool { return strings.Contains(t.kind, "worker") }

// autoAttach asks the browser to attach every frame of another site and
// every worker a page creates to the page's own connection, and to hold
// each one still until this program has put it under supervision.
var autoAttach = map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}

// fetchPatterns select the requests that pass through this program before
// they leave the browser (see orderWatch).
func fetchPatterns() []map[string]any {
	patterns := []map[string]any{}
	// On the way out: everything a script, a form or a navigation sends,
	// whatever its address, so that no order can leave unseen.
	for _, kind := range []string{"XHR", "Fetch", "Document", "Ping", "Other"} {
		patterns = append(patterns, map[string]any{"urlPattern": "*", "resourceType": kind, "requestStage": "Request"})
	}
	// On the way back: the replies to order previews and submissions.
	for _, p := range []string{"*order*", "*Order*", "*checkout*", "*purchase*"} {
		patterns = append(patterns, map[string]any{"urlPattern": p, "requestStage": "Response"})
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
	nt := &tab{id: t.ID, kind: t.Type, page: page, done: make(chan struct{})}
	go s.eventLoop(nt)
	// (Workers have no page to speak of; their requests are what matters.)
	isWorker := strings.Contains(t.Type, "worker")
	if !isWorker {
		if err := page.Call(pctx, "Page.enable", nil, nil); err != nil {
			_ = page.Close()
			<-nt.done
			return nil, fmt.Errorf("attach to launcher page: %w", err)
		}
	}
	err = page.Call(pctx, "Fetch.enable", map[string]any{"patterns": fetchPatterns()}, nil)
	if backgroundWorker(t.Type) {
		// Found already running: holding its requests back is not to be
		// relied on, so it is fenced off as well.
		if fence(pctx, page, "") {
			nt.fenced.Store(true)
			err = nil
		}
	}
	if err != nil {
		_ = page.Close()
		<-nt.done
		return nil, fmt.Errorf("cannot supervise the checkout's network requests, so nothing will be ordered: %w", err)
	}
	// A service worker would relay the page's requests from outside this
	// supervision. Ask the browser to leave it out; if it will not, the
	// order button is not pressed on a page a service worker controls.
	if !isWorker {
		if err := page.Call(pctx, "Network.enable", nil, nil); err == nil {
			nt.swBypassed = page.Call(pctx, "Network.setBypassServiceWorker", map[string]any{"bypass": true}, nil) == nil
		}
	}
	s.mu.Lock()
	s.tabs = append(s.tabs, nt)
	s.mu.Unlock()
	// Frames of other sites and workers run outside the page; have them
	// handed over as they appear. (Where the browser cannot do that, they
	// are looked for at intervals instead: see superviseFrames.)
	nt.auto = page.Call(pctx, "Target.setAutoAttach", autoAttach, nil) == nil
	// A frame or worker that is being held for whoever attaches to it (the
	// launcher's browser holds a worker that starts afresh while attached)
	// is let run now that it is supervised.
	_ = page.Post("", "Runtime.runIfWaitingForDebugger", nil)
	return nt, nil
}

// attachedChild is what the browser says when it has attached a frame or
// worker to a page's connection.
type attachedChild struct {
	SessionID  string `json:"sessionId"`
	TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
		URL      string `json:"url"`
	} `json:"targetInfo"`
	// Held: it has not run anything yet and waits to be let go.
	Held bool `json:"waitingForDebugger"`
}

// adopt puts a frame or worker that the browser has just attached to a
// page's connection under the same request supervision as the page, and
// only then lets it run. It runs in the child's own lane.
func (s *Session) adopt(t *tab, l *lane, p attachedChild) {
	k := kid{targetID: p.TargetInfo.TargetID, kind: p.TargetInfo.Type, url: trimURL(p.TargetInfo.URL), via: t}
	isWorker := strings.Contains(k.kind, "worker")
	// Whatever happens below, a child that was held is let go in the end
	// (unless that is refused on purpose): one left held would freeze a
	// part of the checkout.
	resume := func() { _ = t.page.Post(l.session, "Runtime.runIfWaitingForDebugger", nil) }

	k.supervised = l.call(t.page, "Fetch.enable", map[string]any{"patterns": fetchPatterns()}, nil) == nil
	if isWorker && !p.Held {
		// A worker that was already under way: the browser does not hold
		// its requests back from now on, whatever it answered.
		k.supervised = false
	}
	if backgroundWorker(k.kind) && !k.supervised {
		fctx, cancel := context.WithTimeout(l.ctx, 5*time.Second)
		k.fenced = fence(fctx, t.page, l.session)
		cancel()
	}
	// Its own frames and workers in turn. (Not waited for: the launcher's
	// browser answers this one only once the child is running.)
	_ = t.page.Post(l.session, "Target.setAutoAttach", autoAttach)
	s.mu.Lock()
	if s.kids == nil {
		s.kids = map[string]kid{}
	}
	if l.ctx.Err() == nil { // (not one that has gone away meanwhile)
		s.kids[l.session] = k
	}
	s.mu.Unlock()

	if !isWorker && !k.supervised {
		// A frame that cannot be supervised over its page's connection is
		// tried over one of its own, while it is still held.
		actx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
		direct := cdp.Target{ID: k.targetID, Type: k.kind, URL: p.TargetInfo.URL, WebSocketDebuggerURL: s.socketOf(k.targetID)}
		_, err := s.attach(actx, direct)
		cancel()
		if err != nil && p.Held {
			if w := s.currentWatch(); w != nil && w.isPressed() {
				// It came into being after the order button was pressed
				// and nothing can look at what it sends: it is not let run,
				// and the claim ends here.
				w.refuse("a part of the checkout that appeared after the order button was pressed could not be supervised (" + k.kind + " " + k.url + "); it was not let run, and nothing further was let out")
				return
			}
			// Before the press it may run: the button is not pressed while
			// anything is unsupervised (see order).
		}
	}
	resume()
}

// socketOf is the DevTools address of a target of this browser.
func (s *Session) socketOf(targetID string) string {
	return "ws" + strings.TrimPrefix(s.base, "http") + "/devtools/page/" + targetID
}

// disown forgets a frame or worker that has gone away.
func (s *Session) disown(session string) {
	s.mu.Lock()
	delete(s.kids, session)
	s.mu.Unlock()
}

// adopted reports whether a target is already taken care of as some page's
// frame or worker. (A frame that could not be supervised that way is not:
// it is then tried over a connection of its own.)
func (s *Session) adopted(targetID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.kids {
		if k.targetID == targetID && (k.supervised || k.fenced || strings.Contains(k.kind, "worker")) {
			return true
		}
	}
	return false
}

// watching describes, for the log, what is under supervision next to the
// page being driven.
func (s *Session) watching() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames, workers, fenced, loose := 0, 0, 0, 0
	count := func(kind string, supervised, isFenced bool) {
		if strings.Contains(kind, "worker") {
			workers++
			switch {
			case isFenced:
				fenced++
			case !supervised:
				loose++
			}
			return
		}
		frames++
	}
	for _, k := range s.kids {
		count(k.kind, k.supervised, k.fenced)
	}
	auto := false
	for i, t := range s.tabs {
		auto = auto || t.auto
		if i > 0 && t.page != s.page {
			count(t.kind, true, t.fenced.Load())
		}
	}
	how := "found by looking at intervals"
	if auto {
		how = "handed over by the browser as they are created"
	}
	return fmt.Sprintf("%d frame(s) and %d worker(s) next to the page (%d kept away from order addresses, %d not separately inspectable), %s", frames, workers, fenced, loose, how)
}

// unsupervised lists what is running next to the checkout without its
// requests passing through this program: frames (which stop the order
// button from being pressed) and workers (which, in the launcher's browser,
// send their requests through the page that started them).
func (s *Session) unsupervised() (frames, workers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	direct := map[string]bool{}
	for _, t := range s.tabs {
		direct[t.id] = true
	}
	for _, k := range s.kids {
		if k.supervised || k.fenced || direct[k.targetID] {
			continue
		}
		what := k.kind + " " + k.url
		if strings.Contains(k.kind, "worker") {
			workers = append(workers, what)
		} else {
			frames = append(frames, what)
		}
	}
	sort.Strings(frames)
	sort.Strings(workers)
	return frames, workers
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

// blank takes whatever is loaded off a page. A checkout must not stay open
// (free to retry a request on its own) once nobody supervises it any more.
func blank(page *cdp.Page) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = page.Navigate(ctx, "about:blank")
}

// goHome drops every page adopted along the way and returns to the page the
// session started on.
func (s *Session) goHome() {
	s.mu.Lock()
	var keep, extra []*tab
	for i, t := range s.tabs {
		// (Background workers found running stay fenced for as long as the
		// session lasts: see Close.)
		if i == 0 || t.isWorker() {
			keep = append(keep, t)
		} else {
			extra = append(extra, t)
		}
	}
	s.tabs = keep
	s.mu.Unlock()
	for _, t := range extra {
		s.letGo(t)
	}
	if s.home != nil {
		s.page = s.home.page
	}
}

// stopServiceWorkers stops the browser's service workers. One that a page
// set going outlives the page, and with the page goes the supervision of
// what it sends; stopped, it sends nothing. (They start again by themselves
// when a page needs them.)
func stopServiceWorkers(page *cdp.Page) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if page.Call(ctx, "ServiceWorker.enable", nil, nil) != nil {
		return
	}
	_ = page.Call(ctx, "ServiceWorker.stopAllWorkers", nil, nil)
	_ = page.Call(ctx, "ServiceWorker.disable", nil, nil)
}

// retire takes a page out of use without giving anything it still has in
// hand a way out: from here on nothing that could change anything is let
// through, its content is taken off, and the service workers it may have
// set going are stopped.
func retire(t *tab) {
	t.closing.Store(true)
	if !t.isWorker() {
		blank(t.page)
		stopServiceWorkers(t.page)
	}
}

// letGo ends the supervision of a page: it is retired, what was held back
// at that moment is answered, and only then is the connection closed (which
// would release whatever was still held).
func (s *Session) letGo(t *tab) {
	retire(t)
	time.Sleep(150 * time.Millisecond)
	s.disconnect(t)
}

func (s *Session) disconnect(t *tab) {
	t.closing.Store(true)
	_ = t.page.Close()
	<-t.done
	s.mu.Lock()
	for id, k := range s.kids {
		if k.via == t {
			delete(s.kids, id)
		}
	}
	s.mu.Unlock()
}

// currentTab is the attached page being driven.
func (s *Session) currentTab() *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tabs {
		if t.page == s.page {
			return t
		}
	}
	return nil
}

// superviseFrames puts the frames that run in processes of their own
// (frames from other sites) under the same request supervision as the page.
//
// With known set (the windows that existed before the checkout was opened),
// windows that have appeared since are supervised too.
func (s *Session) superviseFrames(ctx context.Context, known map[string]bool) {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	targets, err := cdp.Targets(tctx, s.base)
	cancel()
	if err != nil {
		return
	}
	listed := map[string]bool{}
	for _, t := range targets {
		listed[t.ID] = true
	}
	s.mu.Lock()
	for id, k := range s.kids {
		if k.via == nil && !listed[k.targetID] {
			delete(s.kids, id) // a worker that could not be looked into, and is gone
		}
	}
	s.mu.Unlock()
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" || s.attached(t.ID) != nil || s.adopted(t.ID) {
			continue
		}
		switch {
		case t.Type == "iframe" || strings.Contains(t.Type, "worker"):
		case t.Type == "page" && known != nil && !known[t.ID] && !isLoginURL(t.URL):
			// A window that was not there before the order button was
			// pressed: whatever opened it, it orders nothing unseen.
		default:
			continue
		}
		if _, err := s.attach(ctx, t); err != nil && ctx.Err() == nil && strings.Contains(t.Type, "worker") {
			// Not asked again; it is named before the button is pressed.
			s.mu.Lock()
			if s.kids == nil {
				s.kids = map[string]kid{}
			}
			s.kids["direct:"+t.ID] = kid{targetID: t.ID, kind: t.Type, url: trimURL(t.URL)}
			s.mu.Unlock()
		}
	}
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

// targetIDs lists the launcher's current pages and other targets. It
// returns nil when the browser does not answer: "unknown" must not be taken
// for "there are none" (every window would then count as new).
func (s *Session) targetIDs(ctx context.Context) map[string]bool {
	for attempt := 0; attempt < 2 && ctx.Err() == nil; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		targets, err := cdp.Targets(tctx, s.base)
		cancel()
		if err == nil {
			out := map[string]bool{}
			for _, t := range targets {
				out[t.ID] = true
			}
			return out
		}
	}
	return nil
}

// adoptCheckout looks for a checkout window the launcher has opened by
// itself (it does so when its store asks for a purchase) and, if there is
// one whose address names this offer, continues there.
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
		// Only a window that shows this very offer: what is ordered in it
		// is recorded for this giveaway.
		if !strings.Contains(t.URL, o.OfferID) {
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
	// The pages first, so that nothing they feed (a worker shared between
	// pages, a service worker) is still being fed when its own supervision
	// ends; then every connection.
	for _, t := range tabs {
		if !t.isWorker() {
			retire(t)
		}
	}
	time.Sleep(150 * time.Millisecond)
	for _, t := range tabs {
		if !t.isWorker() {
			s.disconnect(t)
		}
	}
	for _, t := range tabs {
		if t.isWorker() {
			s.disconnect(t)
		}
	}
}

// Gone reports whether the connection to the launcher's page has ended: the
// launcher was closed, closed itself (it does that when it has an update of
// its own to install) or crashed. Nothing more can be done in this session
// then; whoever runs it starts the launcher again.
func (s *Session) Gone() bool {
	if s == nil || s.home == nil {
		return false
	}
	select {
	case <-s.home.page.Done():
		return true
	default:
		return false
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
// stays paused until answered, so this must never block on anything else:
// what concerns a frame or worker attached through the page's connection is
// handed to that child's lane.
func (s *Session) eventLoop(t *tab) {
	defer close(t.done)
	defer func() {
		for _, l := range t.lanes {
			l.cancel()
		}
	}()
	for {
		select {
		case <-t.page.Done():
			return
		case ev := <-t.page.Events():
			switch ev.Method {
			case "Target.attachedToTarget":
				var p attachedChild
				if json.Unmarshal(ev.Params, &p) != nil || p.SessionID == "" {
					continue
				}
				l := t.laneOf(p.SessionID)
				l.push(func() { s.adopt(t, l, p) })
				continue
			case "Target.detachedFromTarget":
				var p struct {
					SessionID string `json:"sessionId"`
				}
				if json.Unmarshal(ev.Params, &p) == nil && p.SessionID != "" {
					if l := t.lanes[p.SessionID]; l != nil {
						l.gone.Store(true)
						l.cancel()
						delete(t.lanes, p.SessionID)
					}
					s.disown(p.SessionID)
				}
				continue
			case "Network.requestWillBeSent":
				if s.isFenced(t, ev.SessionID) {
					s.fencedRequest(ev)
				}
				continue
			case "Inspector.targetReloadedAfterCrash":
				// A worker that started afresh while attached is held by
				// the launcher's browser until told to run.
				_ = t.page.Post(ev.SessionID, "Runtime.runIfWaitingForDebugger", nil)
				continue
			}
			if ev.SessionID != "" {
				// A frame's or worker's own request.
				if ev.Method != "Fetch.requestPaused" {
					continue
				}
				var p pausedRequest
				if json.Unmarshal(ev.Params, &p) != nil || p.RequestID == "" {
					continue
				}
				l := t.lanes[ev.SessionID]
				if l == nil {
					continue // it has gone away; nobody is left to answer
				}
				l.push(func() {
					s.answer(t, p, func(method string, params, result any) error { return l.call(t.page, method, params, result) })
				})
				continue
			}
			switch ev.Method {
			case "Page.javascriptDialogOpening":
				s.dismissDialog(t.page, ev.Params)
			case "Fetch.requestPaused":
				var p pausedRequest
				if json.Unmarshal(ev.Params, &p) != nil || p.RequestID == "" {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				s.answer(t, p, func(method string, params, result any) error { return t.page.Call(ctx, method, params, result) })
				cancel()
			}
		}
	}
}

// dismissDialog answers a dialog the page has opened. Unanswered, it would
// hold the page (and every navigation away from it) up. "Leave this page?"
// is answered with yes, so that a checkout can always be left; anything
// else is declined.
func (s *Session) dismissDialog(page *cdp.Page, params json.RawMessage) {
	var d struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(params, &d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = page.Call(ctx, "Page.handleJavaScriptDialog", map[string]any{"accept": d.Type == "beforeunload"}, nil)
}

// answer decides about one request the browser is holding back: one of the
// page's own or one of a frame or worker attached through the page's
// connection (call reaches whichever it is).
func (s *Session) answer(t *tab, p pausedRequest, call func(method string, params, result any) error) {
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
			if err := call("Fetch.getResponseBody", id, &body); err == nil {
				w.onResponse(p.Request.Method, p.Request.URL, *p.ResponseStatusCode, decodeBody(body.Body, body.Base64Encoded))
			} else {
				w.onResponse(p.Request.Method, p.Request.URL, *p.ResponseStatusCode, nil)
			}
		}
		_ = call("Fetch.continueRequest", id, nil)
		return
	}

	var allow bool
	switch {
	case t.closing.Load():
		// The page is being let go of: it gets nothing out that could
		// change anything.
		allow = !canChange(p.Request.Method)
	case w != nil:
		allow = w.onRequest(p.Request.Method, p.Request.URL, p.Request.PostData, p.Request.HasPostData)
	default:
		// No claim is in progress, so nothing should be ordering. A page
		// left over from one must not get an order out on its own.
		allow = !mayPlaceOrder(p.Request.Method, p.Request.URL, p.Request.PostData)
	}
	if allow {
		_ = call("Fetch.continueRequest", id, nil)
		return
	}
	_ = call("Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"}, nil)
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
	own, _ := s.ownershipWhy(ctx, st, o)
	return own
}

// ownershipWhy is ownership with, for "unknown", a few words on why the
// store gave no answer (for the log; nothing of the account is in them).
func (s *Session) ownershipWhy(ctx context.Context, st pageState, o feed.Offer) (ownership, string) {
	origin := storeOrigin(st.URL, s.opt.CheckoutBases)
	if origin == "" {
		// Only the store's own pages may ask its API.
		return ownUnknown, "the checkout is not on one of the store's own pages"
	}
	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.page.Eval(cctx, ownershipJS(origin, o.Namespace, o.OfferID), &res); err != nil {
		return ownUnknown, "the page could not ask: " + clip(err.Error(), 100)
	}
	if res.Status == 0 {
		return ownUnknown, "the request did not get through: " + clip(res.Body, 100)
	}
	said := graphQLError([]byte(res.Body))
	if res.Status != 200 {
		if said != "" {
			return ownUnknown, fmt.Sprintf("HTTP %d: %s", res.Status, said)
		}
		return ownUnknown, fmt.Sprintf("HTTP %d", res.Status)
	}
	own := parseOwnership([]byte(res.Body))
	if own == ownUnknown {
		if said != "" {
			return own, "its answer was an error: " + said
		}
		return own, "its answer has a form this program does not know"
	}
	return own, ""
}

// graphQLError is the first error message of a GraphQL answer, shortened;
// empty when there is none.
func graphQLError(body []byte) string {
	var doc struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	for _, e := range doc.Errors {
		if m := strings.TrimSpace(e.Message); m != "" {
			// (Nothing that could name an account goes into the log.)
			return clip(scrub(m), 100)
		}
	}
	return ""
}

// scrub takes out of a message from the store what could identify an
// account or a session: e-mail addresses, and long runs of letters and
// digits (ids, tokens). Long words without a digit stay: Epic's error
// codes and field names are such words, and they are what the message is
// read for.
func scrub(m string) string {
	return reIdentifier.ReplaceAllStringFunc(m, func(t string) string {
		if strings.Contains(t, "@") || strings.ContainsAny(t, "0123456789") {
			return "…"
		}
		return t
	})
}

var reIdentifier = regexp.MustCompile(`\S+@\S+|[A-Za-z0-9_+/=-]{16,}`)

// clip shortens a text for the log and keeps it on one line.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
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
	defer func() {
		// Whatever window the launcher opened for this offer is left
		// behind, and every checkout is taken off the screen, before this
		// claim's supervision of the network ends.
		s.goHome()
		blank(s.home.page)
		stopServiceWorkers(s.home.page)
		s.setWatch(nil)
	}()

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
	// "purchase_limit_error" is what Epic's order service answers when the
	// account has the item already (seen on a real account, together with
	// the page's notice "Sorry, you already own this item, or it's still
	// being processed.").
	reOwnedText  = regexp.MustCompile(`(?i)already own|purchase_limit`)
	reRegionText = regexp.MustCompile(`(?i)unavailable in your region|not available in your (region|country)`)
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

// priceEvidence weighs everything a checkout page and its network traffic
// say about the total of the order.
//
// Free needs a statement of the order's total, or two things that agree:
//   - the checkout's own total line reads zero, or
//   - Epic's order preview gives the order a total of zero, or
//   - (the 2026 free checkout, which has no total line) every amount of
//     money on the page is zero AND the page says "This is free".
//
// Paid is anything that states a total above zero, and it outweighs
// everything.
func priceEvidence(st pageState, w *orderWatch) (Verdict, []string) {
	var evidence []string
	if w.sawPaid() {
		return Paid, []string{"a total above zero was seen on the network"}
	}
	dom := Unknown
	for _, t := range st.Totals {
		v := JudgeTotalText(t)
		dom = Combine(dom, v)
		if v != Unknown {
			evidence = append(evidence, fmt.Sprintf("page shows %q (%v)", t, v))
		}
	}
	// The data a page was built from can only speak against an order: a
	// zero under some key named "total" may be anything.
	preload := Unknown
	if st.Preload != "" {
		if v, facts := JudgeJSON([]byte(st.Preload)); v == Paid {
			preload = Paid
			evidence = append(evidence, fmt.Sprintf("page data totals %v (%v)", facts, v))
		}
	}
	preview := w.previewVerdict()
	if preview != Unknown {
		evidence = append(evidence, fmt.Sprintf("order preview (%v)", preview))
	}
	anyZero := false
	for _, p := range st.Prices {
		if JudgeTotalText(p) == Free {
			anyZero = true
		}
	}
	// Epic's 2026 free checkout has no "total" line: it shows the price
	// (zero) and says "This is free". Neither counts without the other, and
	// not next to any amount that is not zero or could not be read.
	page := Unknown
	if JudgePrices(st.Prices) == Free {
		evidence = append(evidence, fmt.Sprintf("the only price on the page is %q", st.Prices[0]))
		if st.SaysFree {
			page = Free
			evidence = append(evidence, "the page says it is free")
		}
	}
	v := Combine(dom, preload, preview, page)
	if v == Free && len(st.Prices) > 0 && !anyZero {
		// Amounts are on the page and none of them is zero.
		return Unknown, evidence
	}
	return v, evidence
}

// order takes it from a checkout that is loading (in the current page, or
// in a window the launcher opens for it) to a placed order.
func (s *Session) order(ctx context.Context, o feed.Offer, w *orderWatch, mayFallThrough bool, known map[string]bool) (res Result, last pageState, tryNext bool) {
	// ---- wait until the checkout is ready AND proves the price is zero
	deadline := time.Now().Add(s.opt.CheckoutWait)
	var errorSince, strangerSince, lastAdopt, checkSeen time.Time
	adopted, sawBrowserCheck, ownChecked, sawButton := false, false, false, false
	// ownedBefore: the store says the account has this already, and the
	// run goes on regardless (Options.Again). notOwnedBefore: the store
	// says the account does not have it. (Neither, when it gave no answer.)
	ownedBefore, notOwnedBefore := false, false
	// lost: the launcher was found gone at the last look (see below).
	lost := false
	var evidence []string
	pre := Unknown
	for {
		if ctx.Err() != nil {
			return Result{Failed, "interrupted"}, last, false
		}
		// Once something has spoken of a price above zero, or a request
		// had to be refused, this claim is over: nothing that could change
		// anything is let out any more, so no checkout would get further.
		if w.sawPaid() {
			why := w.blockedReason()
			if why == "" {
				why = "a total above zero was stated on the network; nothing was pressed or ordered"
			}
			return Result{NotFree, why}, last, false
		}
		if why := w.blockedReason(); why != "" {
			return Result{Failed, why}, last, false
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
		// Once the button is there, whatever runs next to the checkout is
		// kept under supervision, before the page is read (so that the
		// press follows the price check at once).
		if sawButton {
			s.superviseFrames(ctx, nil)
		}
		st, ierr := s.inspect(ctx)
		if ierr != nil && s.Gone() {
			// Waiting would not bring it back, and no other address would
			// fare better. (One more turn first, so that what the network
			// watch took in while this look was under way is not lost.)
			if !lost {
				lost = true
				continue
			}
			return Result{Failed, "the launcher went away while the checkout was being opened; nothing was pressed"}, last, false
		}
		if ierr == nil {
			last = st
			switch {
			case st.Login:
				return Result{NotSignedIn, "Epic's web sign-in page came up instead of the checkout"}, last, mayFallThrough
			case (st.Owned || w.ownedHint()) && !(s.opt.Again && st.Place != nil):
				// (With Again and an order button on the page, ready or
				// about to be, the run goes on to it; if it never becomes
				// ready, the wait ends as any other.)
				// "Owned" ends all further attempts, so a second opinion is
				// taken from the store when it will give one.
				if s.ownership(ctx, st, o) == ownNo {
					return Result{Failed, "the checkout says this is already owned but the store says it is not; will look again later"}, last, false
				}
				return Result{Owned, "the checkout says this is already in the library" + s.notPressed()}, last, false
			case st.Region || w.regionHint():
				return Result{Unavailable, "not available in this account's region"}, last, false
			case st.Pin:
				return Result{Unavailable, "the account asks for a parental-control PIN, which this program does not handle"}, last, false
			case st.Throttled:
				return Result{Throttled, "Epic says this account cannot claim more free games right now"}, last, false
			case st.Captcha:
				return Result{Captcha, "Epic asked for a human check before the order"}, last, false
			}
			if st.BrowserCheck {
				checkSeen = time.Now()
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
				sawButton = true
				if st.SW {
					if t := s.currentTab(); t == nil || !t.swBypassed {
						return Result{Failed, "the checkout is run through a service worker that this program cannot look into; nothing was ordered"}, last, false
					}
				}
				if !ownChecked {
					// Frames from other sites get the same supervision.
					s.superviseFrames(ctx, nil)
					s.opt.Logf("%s: checkout is up; its requests are held back and checked, with %s", o.Title, s.watching())
					// What does the account already have? (Asked once; it
					// takes a moment, so the page is read afresh afterwards.)
					ownChecked = true
					own, why := s.ownershipWhy(ctx, st, o)
					switch own {
					case ownUnknown:
						// (Said in the log so that a store that has stopped
						// answering this question does not go unnoticed.)
						s.opt.Logf("%s: the store did not say whether the account has this already (%s); the checkout and Epic's answer to the order decide", o.Title, why)
					case ownNo:
						notOwnedBefore = true
					case ownYes:
						if !s.opt.Again {
							return Result{Owned, "the store says the account has it already; nothing was pressed"}, last, false
						}
						ownedBefore = true
						s.opt.Logf("%s: already in the library; going on all the same, as asked", o.Title)
					case ownBlocked:
						return Result{Later, "the store says this account cannot get the offer right now (a required base game is missing, or it conflicts with something owned)"}, last, false
					}
					continue
				}
				// Is it really free? Judged on the page as it is right now;
				// the click follows at once.
				pre, evidence = priceEvidence(st, w)
				if pre == Paid {
					return Result{NotFree, "the checkout does not show a zero total; nothing was ordered: " + strings.Join(evidence, "; ")}, last, false
				}
				if pre == Free {
					break
				}
				// The button is there but the proof is not (yet): the page
				// may still be filling in. Keep looking until the deadline.
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
			switch {
			case last.BrowserCheck || (sawBrowserCheck && time.Since(checkSeen) < 5*time.Second):
				// (The check's page reloads itself every second or so; the
				// last look may have caught it in between.)
				// Backs off like a CAPTCHA: asking again soon would not help.
				return Result{Captcha, "Epic's site kept showing its browser check instead of the checkout"}, last, false
			case sawButton:
				// No button is pressed without positive evidence of a zero price.
				return Result{Failed, fmt.Sprintf("the price could not be confirmed to be zero from the checkout page (amounts seen: %q); nothing was ordered", last.Prices)}, last, false
			case ierr != nil:
				return Result{Failed, "the checkout page could not be read: " + ierr.Error()}, last, mayFallThrough
			case last.App && last.AppError == "" && len(last.Buttons) > 0:
				// A checkout did come up; another address would look the same.
				return Result{Failed, fmt.Sprintf("the checkout loaded but its order button was not recognised or never became ready (buttons on the page: %q)", last.Buttons)}, last, false
			}
			return Result{Failed, "the checkout did not become ready (page: " + trimURL(last.URL) + ")"}, last, mayFallThrough
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return Result{Failed, "interrupted"}, last, false
		}
	}

	w.setPreClick(pre)
	// Everything that runs next to the checkout must be under the same
	// supervision before anything is pressed.
	frames, workers := s.unsupervised()
	if len(workers) > 0 {
		s.opt.Logf("%s: note: the checkout runs %d worker(s) whose requests cannot be looked into separately (%s)", o.Title, len(workers), strings.Join(workers, ", "))
	}
	if len(frames) > 0 {
		return Result{Failed, "part of the checkout could not be supervised (" + strings.Join(frames, ", ") + "); nothing was ordered"}, last, false
	}
	how := ""
	if last.Guessed {
		how = "; the order button was identified by elimination, not by its label"
	}
	s.opt.Logf("%s: checkout ready, price check = %v [%s]; button %q%s", o.Title, pre, strings.Join(evidence, "; "), last.Place.Text, how)

	if s.opt.DryRun {
		// Rehearse everything short of the press: the pointer is moved onto
		// the button and the last look is taken.
		rehearsal := "the pointer was moved onto the button and the last look before a press passed"
		if err := s.approach(ctx, last.Place, w); err != nil {
			if s.Gone() && ctx.Err() == nil {
				return Result{Failed, "the launcher went away during the rehearsal, before the last look; nothing was pressed"}, last, false
			}
			rehearsal = "but a real run would not have pressed: " + err.Error()
		}
		if ownedBefore {
			rehearsal += " (the game is already in the library)"
		}
		return Result{DryRun, fmt.Sprintf("checkout reached and \"%s\" is ready; price check = %v; %s; order not placed (dry run)", last.Place.Text, pre, rehearsal)}, last, false
	}

	// ---- place the order
	if ctx.Err() != nil {
		return Result{Failed, "interrupted"}, last, false
	}
	pressed := *last.Place
	// Words of thanks that were on the page before the press prove nothing,
	// and neither do "you already own this" or an error shown before it.
	thanksBefore, ownedTextBefore := last.Success, last.Owned
	errorsBefore := map[string]bool{}
	for _, e := range last.Errors {
		errorsBefore[e] = true
	}
	if ownedBefore {
		s.opt.Logf("%s: pressing %q although the game is in the library: Epic is expected to refuse the order, which is what this run is for", o.Title, last.Place.Text)
	}
	// From here on, whatever is sent to where orders are taken is treated
	// as an order (see orderWatch.onRequest).
	w.markPressed(pre)
	if err := s.click(ctx, last.Place, w); err != nil {
		if errors.Is(err, cdp.ErrPressed) {
			return Result{Failed, "interrupted after the order button was pressed; the library will be checked next time"}, last, false
		}
		if ctx.Err() != nil {
			return Result{Failed, "interrupted"}, last, false
		}
		if errors.Is(err, errCalledOff) && w.sawPaid() {
			return Result{NotFree, err.Error()}, last, false
		}
		if s.Gone() {
			return Result{Failed, "the launcher went away just before the order button was to be pressed; nothing was pressed"}, last, false
		}
		if errors.Is(err, errCalledOff) {
			return Result{Failed, err.Error()}, last, false
		}
		return Result{Failed, "could not press the order button: " + err.Error()}, last, false
	}
	s.opt.Logf("%s: pressed %q; waiting for Epic's answer", o.Title, pressed.Text)
	// (A look that failed before the press says nothing about now.)
	lost = false
	clickedAt := time.Now()
	clicks := 1
	deadline = time.Now().Add(s.opt.OrderWait)
	var lastConsent, lastOwnCheck, errSeen, thanksSince time.Time
	lastFrames := time.Now()
	var errText string
	for {
		if ctx.Err() != nil {
			return Result{Failed, "interrupted after the order button was pressed; the library will be checked next time"}, last, false
		}
		// Frames and windows that appear only now are put under supervision
		// as well (looked for often at first: this is when they would come).
		every := time.Second
		if time.Since(clickedAt) < 5*time.Second {
			every = 200 * time.Millisecond
		}
		if time.Since(lastFrames) > every {
			lastFrames = time.Now()
			s.superviseFrames(ctx, known)
		}
		if reason := w.blockedReason(); reason != "" {
			if w.sawPaid() {
				return Result{NotFree, reason}, last, false
			}
			return Result{Failed, reason}, last, false
		}
		if w.sawPaid() {
			// A total above zero turned up after the press (a re-priced
			// preview, for instance). Nothing more is pressed, and the
			// network watch refuses any order from here on.
			if w.submitted() {
				return Result{NotFree, "a total above zero appeared after an order with a total of zero had been sent; nothing further was done or let out. Check the account's order history"}, last, false
			}
			return Result{NotFree, "a total above zero appeared after the order button was pressed; nothing further was done and no order was let out"}, last, false
		}
		if ok, msg := w.confirmation(); ok {
			if shape := w.confirmationNote(); shape != "" {
				s.opt.Logf("%s: Epic accepted the order%s", o.Title, shape)
			}
			return Result{Claimed, "order confirmed by Epic"}, last, false
		} else if msg != "" {
			if oc, known := classifyError(msg); known {
				if oc == Owned {
					return Result{Owned, "Epic says the account has this already (" + msg + ")"}, last, false
				}
				return Result{oc, msg}, last, false
			}
			return Result{Failed, "Epic rejected the order: " + msg}, last, false
		}

		st, ierr := s.inspect(ctx)
		if ierr != nil && s.Gone() {
			// No answer will reach this program any more. (One more turn
			// first: Epic's answer may have come in while this look was
			// under way, and the checks above then find it.)
			if !lost {
				lost = true
				continue
			}
			return Result{Failed, "the launcher went away after the order button was pressed, before Epic's answer was seen; the library will be checked next time"}, last, false
		}
		if ierr == nil {
			last = st
			switch {
			case st.Throttled:
				return Result{Throttled, "Epic says this account cannot claim more free games right now"}, last, false
			case st.Captcha:
				return Result{Captcha, "Epic asked for a human check; the order was not completed"}, last, false
			case st.Owned && !ownedTextBefore:
				return Result{Owned, "after the press, the checkout says this is already in the library"}, last, false
			case st.Region:
				return Result{Unavailable, "not available in this account's region"}, last, false
			case st.Login:
				return Result{NotSignedIn, "Epic's sign-in page came up during the order"}, last, false
			}
			if st.Success && !thanksBefore {
				// The page thanks for the order. "Claimed" is final, so the
				// library gets a say when it will answer: if it still does
				// not list the game after a while, the attempt is counted
				// as failed and repeated later (which settles it either way).
				if thanksSince.IsZero() {
					thanksSince = time.Now()
				}
				switch s.ownership(ctx, st, o) {
				case ownNo:
					if time.Since(thanksSince) > 20*time.Second {
						return Result{Failed, "the checkout showed its confirmation but the library does not list the game yet; will check again later"}, last, false
					}
				default:
					return Result{Claimed, "the checkout shows the order confirmation"}, last, false
				}
			}
			if st.Consent.clickable() && time.Since(lastConsent) > 2*time.Second {
				// EU/UK accounts must confirm they waive the withdrawal
				// period before the order is accepted. Pressed only while
				// nothing speaks of a price above zero.
				if v, _ := priceEvidence(st, w); v != Paid {
					lastConsent = time.Now()
					s.opt.Logf("%s: the checkout asks for a confirmation; pressing %q", o.Title, st.Consent.Text)
					_ = s.click(ctx, st.Consent, w)
				} else {
					return Result{NotFree, "the checkout showed a total above zero before the final confirmation; nothing was confirmed"}, last, false
				}
			}
			fresh := ""
			for _, e := range st.Errors {
				if !errorsBefore[e] {
					fresh = e
					break
				}
			}
			if fresh != "" {
				if errSeen.IsZero() || fresh != errText {
					errSeen, errText = time.Now(), fresh
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
			// The first press can land before the page is listening. Press
			// again only if it is unmistakably the same button, in the same
			// place, on a page whose price is still proven to be zero.
			if st.Place.clickable() && !st.Guessed && st.Consent == nil && !st.Success && !w.submitted() && clicks < 3 &&
				time.Since(clickedAt) > 12*time.Second && st.Place.Text == pressed.Text {
				if v, _ := priceEvidence(st, w); v == Free {
					clicks++
					s.opt.Logf("%s: no order has gone out %v after the press; pressing %q again (press %d)", o.Title, time.Since(clickedAt).Round(time.Second), st.Place.Text, clicks)
					clickedAt = time.Now()
					_ = s.click(ctx, st.Place, w)
				}
			}
		}
		// The library listing the game counts as "claimed" only if it did
		// not list it before the press. (If it did, or gave no answer then,
		// its "yes" now says nothing about this order: Epic's reply and
		// the page decide.)
		if notOwnedBefore && time.Since(lastOwnCheck) > 4*time.Second {
			lastOwnCheck = time.Now()
			if s.ownership(ctx, last, o) == ownYes {
				return Result{Claimed, "now in the library"}, last, false
			}
		}
		if time.Now().After(deadline) {
			if w.submitted() {
				return Result{Failed, "the order was sent but no confirmation arrived in time; the library will be checked next time"}, last, false
			}
			return Result{Failed, "no confirmation appeared after the order button was pressed; the library will be checked next time"}, last, false
		}
		if err := sleep(ctx, 400*time.Millisecond); err != nil {
			return Result{Failed, "interrupted after the order button was pressed; the library will be checked next time"}, last, false
		}
	}
}

// errCalledOff marks a press that was not made because something spoke
// against it at the last moment.
var errCalledOff = errors.New("nothing was pressed")

// stillThere reports, immediately before a press, whether it should still
// be made: nothing has spoken of a price above zero or been refused, and
// what lies under the pointer is still the button that was judged.
func (s *Session) stillThere(ctx context.Context, b *btn, w *orderWatch) error {
	if w.sawPaid() {
		return fmt.Errorf("%w: a total above zero appeared just before the press", errCalledOff)
	}
	if reason := w.blockedReason(); reason != "" {
		return fmt.Errorf("%w: %s", errCalledOff, reason)
	}
	var label *string
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.page.Eval(cctx, fmt.Sprintf("%s(%f, %f)", underPointJS, b.X, b.Y), &label); err != nil {
		return fmt.Errorf("%w: the page could not be asked what lies under the pointer (%v)", errCalledOff, err)
	}
	if label == nil || *label != b.Text {
		return fmt.Errorf("%w: the button moved or changed just before the press", errCalledOff)
	}
	return nil
}

// notPressed is added, in a run with Again, to an answer that ends it
// before anything was pressed: such a run is made to see the press happen,
// and its result must say when it did not.
func (s *Session) notPressed() string {
	if s.opt.Again {
		return "; nothing was pressed"
	}
	return ""
}

// approach does everything click does up to the press, and leaves it at
// that: the pointer ends on the button, nothing is pressed. It returns what
// the last look found.
func (s *Session) approach(ctx context.Context, b *btn, w *orderWatch) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	errStop := errors.New("rehearsal")
	err := s.page.ClickIf(cctx, b.X, b.Y, func() error {
		if err := s.stillThere(cctx, b, w); err != nil {
			return err
		}
		return errStop
	})
	if errors.Is(err, errStop) {
		return nil
	}
	if err == nil {
		// Cannot happen: the guard never lets the press through.
		return errors.New("the rehearsal pressed the button")
	}
	return err
}

// click presses a button the way a person would, falling back to a
// script-driven click if the browser refuses synthetic mouse input. With a
// watch given, the press is called off (errCalledOff) if, at the last
// moment, the price is in doubt or the button is no longer where it was.
func (s *Session) click(ctx context.Context, b *btn, w *orderWatch) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// (Buttons that order nothing, on a store page, are pressed as found.)
	guard := func() error {
		if w == nil {
			return nil
		}
		return s.stillThere(cctx, b, w)
	}
	err := s.page.ClickIf(cctx, b.X, b.Y, guard)
	if err == nil {
		return nil
	}
	// The scripted click is only for a browser that refuses mouse input.
	// It must not follow a mouse click that may have landed (that would be
	// two clicks), nor a claim that has been called off.
	if errors.Is(err, cdp.ErrPressed) || errors.Is(err, errCalledOff) || cctx.Err() != nil {
		return err
	}
	if err := guard(); err != nil {
		return err
	}
	var ok bool
	if err := s.page.Eval(cctx, fmt.Sprintf("%s(%f, %f)", clickJS, b.X, b.Y), &ok); err != nil {
		var thrown *cdp.EvalError
		if errors.As(err, &thrown) || err == cdp.ErrClosed {
			// The script itself failed, or it was never sent (the
			// connection had gone already): nothing was clicked.
			return err
		}
		// It was sent and its answer did not arrive: it may have clicked.
		return fmt.Errorf("%w: %v", cdp.ErrPressed, err)
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
