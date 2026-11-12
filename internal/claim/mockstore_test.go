package claim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// scenario configures the stand-in for Epic's store used by the tests. It
// mimics the real checkout's structure: the #purchase-app root, the
// payment-* class names, the order-preview and confirm-order calls, the
// invisible hCaptcha frames, and the "Unexpected Error" page shown to
// signed-out visitors.
type scenario struct {
	SignedIn     bool   `json:"signedIn"`     // checkout opens directly
	HopWorks     bool   `json:"hopWorks"`     // the sign-in page hands a session over
	Owned        bool   `json:"owned"`        // account already has the offer
	MissingBase  bool   `json:"missingBase"`  // needs a base game
	PreviewTotal int    `json:"previewTotal"` // total in the order preview
	NoPreview    bool   `json:"noPreview"`    // page makes no preview call
	DomTotal     string `json:"domTotal"`     // total shown on the page ("" = none shown)
	ConfirmTotal *int   `json:"confirmTotal"` // totalAmount sent with the order (nil = field omitted)
	EU           bool   `json:"eu"`           // asks for withdrawal-waiver consent
	Captcha      bool   `json:"captcha"`      // shows a challenge after the button is pressed
	CaptchaFirst bool   `json:"captchaFirst"` // shows a challenge before the button is ready
	SlowMs       int    `json:"slowMs"`       // how long the button stays disabled
	ConfirmError string `json:"confirmError"` // Epic rejects the order with this message
	GraphQLDown  bool   `json:"graphqlDown"`  // ownership query unavailable
	German       bool   `json:"german"`       // non-English labels
	OwnedMessage bool   `json:"ownedMessage"` // checkout itself says "already own"
	Region       bool   `json:"region"`       // checkout says unavailable in region
	Pin          bool   `json:"pin"`          // parental PIN prompt
	FormPost     bool   `json:"formPost"`     // order is sent as a form, not JSON
	NewLayout    bool   `json:"newLayout"`    // Epic's 2026 free checkout: no "total" line, "Add to library"
	NewPrice     string `json:"newPrice"`     // price shown by the new layout (default "$0.00")
	NoFreeText   bool   `json:"noFreeText"`   // new layout without the "This is free" sentence
	Throttled    bool   `json:"throttled"`    // "unable to download any more free games"
	// DirectBlocked makes the checkout refuse to open unless it is reached
	// from the store page (as when only the launcher's own flow works).
	DirectBlocked bool   `json:"directBlocked"`
	ProductLabel  string `json:"productLabel"`  // the store page's main button (default "Get")
	ProductGate   bool   `json:"productGate"`   // a "Continue" notice covers the store page first
	SubmitPath    string `json:"submitPath"`    // where the 2026 layout sends its order (default /checkout/api/complete)
	FrameOrder    bool   `json:"frameOrder"`    // the order is sent by a frame from another site
	FrameOrigin   string `json:"frameOrigin"`   // that frame's origin (filled in by the store)
	RetryOnError  bool   `json:"retryOnError"`  // the page re-sends a failed order by itself
	ServiceWorker bool   `json:"serviceWorker"` // a service worker relays the page's requests
	ExtraHTML     string `json:"-"`             // added to the checkout page
	ProductExtra  string `json:"-"`             // added to the store page
	ProductOpens  string `json:"-"`             // offer the store page's button opens (default 1-ns-id)
	// ChallengeFor makes the checkout answer with a Cloudflare-style
	// "One more step" page for this long before the real page is served
	// (the challenge page reloads itself, as the real one does).
	ChallengeFor time.Duration `json:"-"`
	// OnDirect is called when the checkout address is opened directly.
	OnDirect func(offers string) `json:"-"`
}

func intp(i int) *int { return &i }

type mockStore struct {
	t   *testing.T
	sc  scenario
	srv *httptest.Server

	mu       sync.Mutex
	orders   []string // bodies of confirm-order requests that arrived
	previews int
	owned    bool
	cookie   string
	// lastOffers is the offers parameter of the last checkout opened.
	lastOffers string
	firstHit   time.Time
	hits       []string // what pages reported through /hit
}

var mockSeq int

func newMockStore(t *testing.T, sc scenario) *mockStore {
	t.Helper()
	mockSeq++
	m := &mockStore{t: t, sc: sc, owned: sc.Owned, cookie: fmt.Sprintf("sess%d", mockSeq)}
	mux := http.NewServeMux()
	mux.HandleFunc("/purchase", m.handleCheckout)
	mux.HandleFunc("/store/product/", m.handleProduct)
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = io.WriteString(w, `self.addEventListener('install', function (e) { self.skipWaiting(); });
self.addEventListener('activate', function (e) { e.waitUntil(self.clients.claim()); });
self.addEventListener('fetch', function (e) { e.respondWith(fetch(e.request)); });`)
	})
	mux.HandleFunc("/api/v9/acquire", m.handleConfirm)
	mux.HandleFunc("/payframe", func(w http.ResponseWriter, r *http.Request) {
		// A frame from another site that sends the order when told to.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		total := "null"
		if m.sc.ConfirmTotal != nil {
			total = fmt.Sprint(*m.sc.ConfirmTotal)
		}
		_, _ = io.WriteString(w, `<!doctype html><body><script>
window.addEventListener('message', function (ev) {
  if (!ev.data || ev.data.what !== 'order') return;
  var payload = { offers: ev.data.offers, syncToken: 'tok' };
  if (`+total+` !== null) payload.totalAmount = `+total+`;
  fetch('/purchase/confirm-order', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(payload) })
    .then(function (r) { return r.json(); })
    .then(function (c) { ev.source.postMessage({ what: 'done', error: c.error ? c.message : '' }, '*'); })
    .catch(function () { ev.source.postMessage({ what: 'done', error: 'Sorry, there was an error placing your order.' }, '*'); });
});
</script></body>`)
	})
	mux.HandleFunc("/hit", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits = append(m.hits, r.URL.Query().Get("what"))
		m.mu.Unlock()
	})
	mux.HandleFunc("/purchase/order-preview", m.handlePreview)
	mux.HandleFunc("/purchase/confirm-order", m.handleConfirm)
	mux.HandleFunc("/checkout/api/complete", m.handleConfirm)
	mux.HandleFunc("/graphql", m.handleGraphQL)
	mux.HandleFunc("/id/login", m.handleLogin)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Store</title><body><h1>Store</h1></body>")
	})
	mux.HandleFunc("/frame", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><body>frame</body>")
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	// "localhost" is another site than "127.0.0.1" as far as a browser is
	// concerned, though it is the same server.
	m.sc.FrameOrigin = strings.Replace(m.srv.URL, "127.0.0.1", "localhost", 1)
	return m
}

func (m *mockStore) URL() string { return m.srv.URL }

// hitCount is how often pages reported an event (a click on the order
// button reports "place").
func (m *mockStore) hitCount(what string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.hits {
		if h == what {
			n++
		}
	}
	return n
}

func (m *mockStore) orderCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.orders)
}

func (m *mockStore) lastOrder() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.orders) == 0 {
		return ""
	}
	return m.orders[len(m.orders)-1]
}

func (m *mockStore) hasSession(r *http.Request) bool {
	if m.sc.SignedIn {
		return true
	}
	c, err := r.Cookie(m.cookie)
	return err == nil && c.Value == "1"
}

func (m *mockStore) handleLogin(w http.ResponseWriter, r *http.Request) {
	if m.sc.HopWorks {
		http.SetCookie(w, &http.Cookie{Name: m.cookie, Value: "1", Path: "/"})
		http.Redirect(w, r, r.URL.Query().Get("redirectUrl"), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><title>Sign in to Your Epic Games account | Epic Games</title></head><body>
<h1>Sign in to Epic Games</h1><form><input id="email" name="email"><input id="password" type="password"><button type="submit">Continue</button></form></body></html>`)
}

func (m *mockStore) handlePreview(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.previews++
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"country":   "US",
		"syncToken": "tok",
		"orderResponse": map[string]any{
			"totalPrice": m.sc.PreviewTotal,
			"currency":   "USD",
			"lineOffers": []map[string]any{{"unitPrice": 2499, "discount": 2499 - m.sc.PreviewTotal, "totalPrice": m.sc.PreviewTotal, "quantity": 1}},
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *mockStore) handleConfirm(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.orders = append(m.orders, string(body))
	if m.sc.ConfirmError == "" {
		m.owned = true
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if m.sc.ConfirmError != "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "errors.com.epicgames.purchase", "message": m.sc.ConfirmError})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"confirmation": "ORDER-123", "error": nil})
}

func (m *mockStore) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if !m.sc.GraphQLDown && r.URL.Query().Get("operationName") == "getMappingByPageSlug" {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Query().Get("variables"), `"pageSlug":"revealed-game"`) {
			_, _ = io.WriteString(w, `{"data":{"StorePageMapping":{"mapping":{"pageSlug":"revealed-game","pageType":"productHome","mappings":{"offer":{"id":"real-id","namespace":"real-ns"}}}}}}`)
		} else {
			_, _ = io.WriteString(w, `{"data":{"StorePageMapping":{"mapping":null}}}`)
		}
		return
	}
	if m.sc.GraphQLDown || r.URL.Query().Get("operationName") != "getOffersValidation" || !m.hasSession(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"PersistedQueryNotFound"}]}`)
		return
	}
	m.mu.Lock()
	owned := m.owned
	m.mu.Unlock()
	v := map[string]any{"conflictingOffers": nil, "missingPrerequisites": nil, "fullyOwnedOffers": nil, "unablePartiallyUpgradeOffers": nil}
	if owned {
		v["fullyOwnedOffers"] = []map[string]string{{"namespace": "ns", "offerId": "id"}}
	}
	if m.sc.MissingBase {
		v["missingPrerequisites"] = []map[string]any{{"namespace": "ns", "offerId": "id", "missingPrerequisiteItems": []any{"base"}}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"Entitlements": map[string]any{"cartOffersValidation": v}}})
}

const errorPage = `<!doctype html><html><head><title>Payment by Epic Games</title></head><body>
<div id="purchase-app"><div class="message"><h2>Unexpected Error</h2><p>Uh oh, something went wrong.</p></div></div>
<input type="hidden" id="purchaseToken" value=""><script>window.__epic_web_purchase_dataPreload = {};</script></body></html>`

func (m *mockStore) handleCheckout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	m.mu.Lock()
	if m.firstHit.IsZero() {
		m.firstHit = time.Now()
	}
	challenged := m.sc.ChallengeFor > 0 && time.Since(m.firstHit) < m.sc.ChallengeFor
	m.mu.Unlock()
	if challenged {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>One more step</title><meta http-equiv="refresh" content="1"></head><body>
<div class="cf_challenge_container"><h1 data-t="challengeTitle">One more step</h1><p data-t="challengeSubtitle" class="cf_challenge_text">Please complete a security check to continue</p></div>
<script>window._cf_chl_opt = { cType: 'managed' };</script></body></html>`)
		return
	}
	if !m.hasSession(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, errorPage)
		return
	}
	m.mu.Lock()
	m.lastOffers = r.URL.Query().Get("offers")
	m.mu.Unlock()
	if r.URL.Query().Get("via") != "store" {
		if m.sc.OnDirect != nil {
			m.sc.OnDirect(r.URL.Query().Get("offers"))
		}
		if m.sc.DirectBlocked {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, errorPage)
			return
		}
	}
	if !strings.HasPrefix(r.URL.Query().Get("offers"), "1-") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errorPage)
		return
	}
	sc, _ := json.Marshal(m.sc)
	page := checkoutPage
	if m.sc.NewLayout {
		page = newCheckoutPage
	}
	page = strings.ReplaceAll(page, "<!--EXTRA-->", m.sc.ExtraHTML)
	_, _ = io.WriteString(w, strings.ReplaceAll(page, "__SCENARIO__", string(sc)))
}

// newCheckoutPage imitates the free checkout Epic introduced in 2026: no
// payment-* classes, no "total" line, the button reads "Add to library" and
// the confirmation reads "It's all yours".
const newCheckoutPage = `<!doctype html><html><head><title>Checkout</title>
<style>
 body { margin: 0; font-family: sans-serif; background: #101014; color: #fff; }
 section { width: 520px; margin: 40px auto; padding: 24px; background: #202024; }
 button { width: 220px; height: 48px; }
</style></head><body>
<div id="root"><section role="dialog">
  <h2>Checkout</h2>
  <div class="_item_x1a">Flotsam</div>
  <div class="_cost_k3d" id="cost"></div>
  <p id="free-note"></p>
  <button class="_btn_q9 _primary_aa" id="go" disabled>Add to library</button>
  <p>By selecting 'Add to library', you certify that you are over 18 and an authorized user.</p>
  <div id="extra"></div>
  <!--EXTRA-->
</section></div>
<iframe src="/frame" title="hCaptcha" style="display:none"></iframe>
<script>
var S = __SCENARIO__;
window.clicks = [];
var offers = new URLSearchParams(location.search).getAll('offers');
var btn = document.getElementById('go');
document.getElementById('cost').textContent = S.newPrice || '$0.00';
if (!S.noFreeText) document.getElementById('free-note').textContent = 'This is free. Add it to your library to get started.';
if (S.german) { btn.textContent = 'Zur Bibliothek hinzufügen'; document.getElementById('free-note').textContent = S.noFreeText ? '' : 'Dies ist kostenlos.'; }
if (S.throttled) document.getElementById('extra').textContent = 'Your account is unable to download any more free games at this time. Please wait 24 hours before trying to redeem a free game again.';
setTimeout(function () { btn.disabled = false; }, S.slowMs || 0);
btn.addEventListener('click', function (ev) {
  window.clicks.push({ what: 'place', trusted: ev.isTrusted });
  fetch('/hit?what=place');
  fetch(S.submitPath || '/checkout/api/complete', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ purchaseToken: 'tok', offers: offers }) })
    .then(function (r) { return r.json(); }).then(function (c) {
      if (c.error) document.getElementById('extra').innerHTML = '<div role="alert">' + c.message + '</div>';
      else document.querySelector('section').innerHTML = '<h1>' + (S.german ? 'Alles deins' : "It's all yours") + '</h1><p>Download the Epic Games Launcher to play</p>';
    });
});
</script></body></html>`

const checkoutPage = `<!doctype html><html><head><title>Payment by Epic Games</title>
<style>
 body { margin: 0; font-family: sans-serif; background: #101014; color: #fff; }
 #purchase-app { width: 100vw; height: 100vh; position: relative; }
 .payment-summary { padding: 20px; }
 .payment-order-confirm { position: absolute; right: 30px; bottom: 30px; }
 .payment-btn { width: 220px; height: 48px; }
 .payment-confirm { position: absolute; left: 200px; top: 150px; width: 480px; background: #333; padding: 20px; }
</style></head><body>
<div id="purchase-app">
  <div class="payment-summary">
    <div class="payment-offer">Flotsam</div>
    <div class="payment-price"><span>Price</span> <span class="payment-price__value">$24.99</span></div>
    <div class="payment-price"><span>Sale Discount</span> <span class="payment-price__value">-$24.99</span></div>
    <div id="total-slot"></div>
  </div>
  <div class="payment-order-confirm">
    <button class="payment-btn payment-order-confirm__btn payment-btn--primary" disabled><span class="payment-loading payment-loading--loading"></span><span id="place-label">Place Order</span></button>
  </div>
  <div id="extra"></div>
  <!--EXTRA-->
  <!-- the real checkout always carries invisible captcha frames -->
  <iframe src="/frame" title="hCaptcha" data-hcaptcha-widget-id="x" style="display:none"></iframe>
  <div id="h_captcha_challenge_checkout_free_prod" style="position:absolute;left:0;top:0;width:0;height:0;overflow:hidden"><iframe src="/frame?hcaptcha=challenge" style="width:0;height:0;border:0"></iframe></div>
</div>
<input type="hidden" id="purchaseToken" value="tok">
<script>
var S = __SCENARIO__;
window.__epic_web_purchase_dataPreload = { locale: 'en-US' };
window.clicks = [];
var offers = new URLSearchParams(location.search).getAll('offers');
var btn = document.querySelector('.payment-order-confirm__btn');
var extra = document.getElementById('extra');
if (S.german) document.getElementById('place-label').textContent = 'Bestellung aufgeben';
if (S.domTotal) {
  document.getElementById('total-slot').innerHTML = '<div class="payment-price payment-price--total"><span class="payment-price__label">' + (S.german ? 'Gesamt' : 'Total') + '</span> <span class="payment-price__value">' + S.domTotal + '</span></div>';
}
if (S.ownedMessage) extra.innerHTML = '<div class="payment__errors">You already own this item.</div>';
if (S.region) extra.innerHTML = '<div class="payment__errors">This product is unavailable in your region.</div>';
if (S.pin) extra.innerHTML = '<div class="payment-pin-code"><input class="payment-pin-code__input"></div>';
function challenge() {
  var box = document.getElementById('h_captcha_challenge_checkout_free_prod');
  box.style.cssText = 'position:absolute;left:300px;top:100px;width:400px;height:500px;background:#fff';
  box.querySelector('iframe').style.cssText = 'width:400px;height:500px;border:0';
}
var swPending = !!(S.serviceWorker && navigator.serviceWorker && !sessionStorage.getItem('sw'));
var payFrame = null;
if (S.frameOrder) {
  payFrame = document.createElement('iframe');
  payFrame.id = 'payframe';
  payFrame.src = S.frameOrigin + '/payframe';
  payFrame.style.cssText = 'position:absolute;left:0;top:0;width:10px;height:10px;border:0';
  document.body.appendChild(payFrame);
  window.addEventListener('message', function (ev) {
    if (!ev.data || ev.data.what !== 'done') return;
    if (ev.data.error) fail(ev.data.error);
    else document.getElementById('purchase-app').innerHTML = '<h1>Thanks for your order!</h1>';
  });
}
function ready() {
  if (swPending) return; // the page reloads once its service worker is in control
  setTimeout(function () {
    btn.disabled = false;
    var l = btn.querySelector('.payment-loading');
    if (l) l.remove();
  }, S.slowMs || 0);
}
if (swPending) {
  navigator.serviceWorker.register('/sw.js').then(function () { return navigator.serviceWorker.ready; }).then(function () {
    sessionStorage.setItem('sw', '1');
    location.reload();
  });
}
if (S.captchaFirst) challenge();
if (S.noPreview) ready();
else fetch('/purchase/order-preview', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ offers: offers, namespace: 'ns', lineOffers: [{ offerId: 'id', quantity: 1 }] }) })
  .then(function (r) { return r.json(); }).then(function (p) { window.preview = p; ready(); });

function fail(text) { extra.innerHTML = '<div class="payment__errors">' + text + '</div>'; }
function send() {
  if (payFrame) { payFrame.contentWindow.postMessage({ what: 'order', offers: offers }, '*'); return; }
  var payload = { useDefault: true, offers: offers, syncToken: 'tok', includeAccountBalance: false };
  if (S.confirmTotal !== null && S.confirmTotal !== undefined) payload.totalAmount = S.confirmTotal;
  var init = { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(payload) };
  if (S.formPost) {
    var form = new URLSearchParams();
    Object.keys(payload).forEach(function (k) { form.set(k, payload[k]); });
    init = { method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded' }, body: form.toString() };
  }
  fetch('/purchase/confirm-order', init).then(function (r) { return r.json(); }).then(function (c) {
    if (c.error) fail(c.message);
    else document.getElementById('purchase-app').innerHTML = '<h1>' + (S.german ? 'Vielen Dank!' : 'Thanks for your order!') + '</h1>';
  }).catch(function () {
    fail(S.german ? 'Fehler' : 'Sorry, there was an error placing your order.');
    // Some checkouts try again by themselves after a network error.
    window.retries = (window.retries || 0) + 1;
    if (S.retryOnError && window.retries <= 4) setTimeout(send, 1000);
  });
}
function submit() {
  if (S.eu && !window.consented) {
    var d = document.createElement('div');
    d.className = 'payment-confirm';
    d.innerHTML = '<p>By clicking below you waive your right of withdrawal.</p><div class="payment-confirm__actions">' +
      '<button class="payment-btn payment-confirm__btn payment-btn--secondary">' + (S.german ? 'Abbrechen' : 'Cancel') + '</button> ' +
      '<button class="payment-btn payment-confirm__btn payment-btn--primary">' + (S.german ? 'Ich stimme zu' : 'I Accept') + '</button></div>';
    document.getElementById('purchase-app').appendChild(d);
    d.querySelector('.payment-btn--primary').addEventListener('click', function (ev) {
      window.clicks.push({ what: 'consent', trusted: ev.isTrusted });
      window.consented = true; d.remove(); submit();
    });
    d.querySelector('.payment-btn--secondary').addEventListener('click', function () { window.cancelled = true; d.remove(); });
    return;
  }
  if (S.captcha) { challenge(); return; }
  send();
}
btn.addEventListener('click', function (ev) {
  window.clicks.push({ what: 'place', trusted: ev.isTrusted });
  fetch('/hit?what=place');
  submit();
});
</script></body></html>`

// handleProduct serves a game's store page: a main button that, like the
// launcher's store, opens the checkout in a window of its own.
func (m *mockStore) handleProduct(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	label := m.sc.ProductLabel
	if label == "" {
		label = "Get"
	}
	opens := m.sc.ProductOpens
	if opens == "" {
		opens = "1-ns-id"
	}
	gate := ""
	if m.sc.ProductGate {
		gate = `<div id="gate" style="position:fixed;left:0;top:0;right:0;bottom:0;background:#000;z-index:5"><p>This game contains mature content recommended only for ages 18+</p><button id="gate-ok" style="width:160px;height:40px">Continue</button></div>
<script>document.getElementById('gate-ok').onclick = function () { document.getElementById('gate').remove(); };</script>`
	}
	_, _ = io.WriteString(w, `<!doctype html><html><head><title>Flotsam | Store</title></head><body style="margin:0;background:#101014;color:#fff">
<h1>Flotsam</h1>
<aside><button data-testid="purchase-cta-button" id="cta" style="width:220px;height:48px">`+label+`</button>
<button aria-label="Add to Cart" style="width:220px;height:48px">Add To Cart</button></aside>
`+m.sc.ProductExtra+`
`+gate+`
<script>
window.presses = 0;
document.getElementById('cta').addEventListener('click', function () {
  window.presses++;
  if (/^get$/i.test(this.textContent)) window.open('/purchase?highlightColor=0078f2&offers=`+opens+`&via=store');
});
</script></body></html>`)
}
