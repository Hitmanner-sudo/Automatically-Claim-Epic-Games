package claim

import (
	"encoding/json"
	"fmt"
)

// The launcher embeds an old Chromium (version 90), so the scripts below
// stick to conservative JavaScript.

// inspectJS reports the state of whatever page is showing, with emphasis on
// Epic's checkout ("purchase app"). It only reads; it changes nothing.
const inspectJS = `(function () {
  // innerText is what is rendered; textContent would also return the text
  // of inline scripts and data, which says nothing about what is on screen.
  function text(el) { return ((el && (typeof el.innerText === 'string' ? el.innerText : el.textContent)) || '').replace(/\s+/g, ' ').trim(); }
  function box(el) { var r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height }; }
  function shown(el, min) {
    if (!el) return false;
    var r = el.getBoundingClientRect();
    if (r.width < (min || 4) || r.height < (min || 4)) return false;
    var cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none' || parseFloat(cs.opacity) === 0) return false;
    return r.bottom > 0 && r.right > 0 && r.top < (innerHeight || 1e9) && r.left < (innerWidth || 1e9);
  }
  function cls(el) { return (typeof el.className === 'string' ? el.className : (el.getAttribute && el.getAttribute('class')) || '') || ''; }
  function button(el) {
    var b = box(el);
    b.text = text(el).slice(0, 80);
    b.disabled = !!el.disabled || el.getAttribute('aria-disabled') === 'true' || /(^|[\s_-])disabled($|[\s_-])/.test(cls(el));
    b.loading = !!el.querySelector('[class*="loading--loading"], [class*="spinner"], [class*="Spinner"]') || /loading--loading/.test(cls(el));
    return b;
  }

  var out = { url: location.href, title: document.title, ready: document.readyState };
  var body = document.body ? text(document.body) : '';
  out.text = body.slice(0, 1500);

  var path = location.pathname || '';
  out.login = /(^|\.)epicgames\.com$/.test(location.hostname) && /^\/id\/(login|register)/.test(path) ||
    !!document.querySelector('form input[type="password"], input#email[name="email"]');
  // The "One more step / Please complete a security check" page is a
  // Cloudflare check that normally passes by itself and reloads the page.
  var cf = false;
  try { cf = typeof window._cf_chl_opt !== 'undefined'; } catch (errCf) {}
  out.browserCheck = cf || !!document.querySelector('.cf_challenge_container, h1[data-t="challengeTitle"], #challenge-error-text, #challenge-form, #challenge-running') ||
    /just a moment|verify you are human|checking your browser|attention required|one more step|complete (a|the) security check/i.test(document.title + ' ' + body.slice(0, 400));

  var app = document.querySelector('#purchase-app');
  out.app = !!app || /\/(purchase|checkout)(\/|$)/.test(path);
  var msg = app && app.querySelector('.message');
  out.appError = msg ? text(msg).slice(0, 200) : '';

  var scope = app || document;
  var buttons = [];
  var all = scope.querySelectorAll('button, [role="button"], input[type="submit"]');
  for (var i = 0; i < all.length; i++) if (shown(all[i], 8)) buttons.push(all[i]);

  function pick(test) { for (var i = 0; i < buttons.length; i++) if (test(buttons[i])) return buttons[i]; return null; }
  var consent = pick(function (b) { return /payment-confirm__btn/.test(cls(b)) && !/secondary|cancel/i.test(cls(b)); }) ||
    pick(function (b) { return /^(i accept|i agree|ich stimme zu)$/i.test(text(b)); });
  // Epic renamed the button from "Place Order" to "Add to library" for free
  // offers in 2026; both are in use.
  var place = pick(function (b) { return /payment-order-confirm__btn/.test(cls(b)); }) ||
    pick(function (b) { return /^(place order|add to library|zur bibliothek hinzuf\u00fcgen|bestellung abschlie\u00dfen)$/i.test(text(b)); });
  out.guessed = false;
  if (!place && out.app) {
    // Unknown language or renamed classes. Accept the checkout's single
    // primary button, or failing that its single button that is not a way
    // out, but only when there is exactly one candidate. (Nothing is
    // pressed anyway unless the page proves the price is zero.)
    var wayOut = /cancel|close|back|dismiss|abbrechen|schlie|zur\u00fcck|annuler|fermer|retour|cancelar|cerrar|volver|annulla|chiudi|anuluj|zamknij|\u043e\u0442\u043c\u0435\u043d|\u0437\u0430\u043a\u0440\u044b|iptal|kapat|\u53d6\u6d88|\u5173\u95ed|\u95dc\u9589|\u30ad\u30e3\u30f3\u30bb\u30eb|\u9589\u3058\u308b|\ucde8\uc18c|\ub2eb\uae30/i;
    var prim = [], rest = [];
    for (var j = 0; j < buttons.length; j++) {
      var bj = buttons[j];
      if (bj === consent) continue;
      var label = text(bj) + ' ' + (bj.getAttribute('aria-label') || '');
      if (/btn--primary|payment-btn--primary/.test(cls(bj))) prim.push(bj);
      if (text(bj) && !wayOut.test(label)) rest.push(bj);
    }
    if (prim.length === 1) { place = prim[0]; out.guessed = true; }
    else if (prim.length === 0 && rest.length === 1) { place = rest[0]; out.guessed = true; }
  }
  // A real mouse click only lands if the button is inside the visible part
  // of the page and nothing lies on top of it.
  function reachable(el) {
    var r = el.getBoundingClientRect();
    if (r.top < 0 || r.bottom > (innerHeight || 0) || r.left < 0 || r.right > (innerWidth || 0)) {
      try { el.scrollIntoView({ block: 'center', inline: 'center' }); } catch (errScroll) {}
      r = el.getBoundingClientRect();
    }
    var at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return !!at && (at === el || el.contains(at) || at.contains(el));
  }
  // (Bringing a button into view moves the page, so the positions are
  // taken afterwards, and what lies on top is looked at once more.)
  function onTop(el) {
    var r = el.getBoundingClientRect();
    if (r.top < 0 || r.bottom > (innerHeight || 0) || r.left < 0 || r.right > (innerWidth || 0)) return false;
    var at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return !!at && (at === el || el.contains(at) || at.contains(el));
  }
  if (consent === place) consent = null;
  if (place) reachable(place);
  if (consent) reachable(consent);
  out.place = place ? button(place) : null;
  if (out.place) out.place.hit = onTop(place);
  out.consent = consent ? button(consent) : null;
  if (out.consent) out.consent.hit = onTop(consent);
  out.buttons = [];
  for (var bi = 0; bi < buttons.length && out.buttons.length < 10; bi++) out.buttons.push(text(buttons[bi]).slice(0, 40));

  // A CAPTCHA only counts when its challenge is actually on screen: the
  // checkout always carries invisible hCaptcha frames.
  out.captcha = false;
  var frames = document.querySelectorAll('iframe');
  for (var k = 0; k < frames.length; k++) {
    var f = frames[k];
    var id = (f.src || '') + ' ' + (f.id || '') + ' ' + (f.title || '') + ' ' + (f.parentElement ? (f.parentElement.id || '') + ' ' + cls(f.parentElement) : '');
    if (/hcaptcha|h_captcha|recaptcha|arkoselabs|funcaptcha|talon/i.test(id) && shown(f, 120)) out.captcha = true;
  }
  if (/select all (images|squares)/i.test(body)) out.captcha = true;
  out.throttled = /unable to download any more free games|wait 24 hours before trying to redeem/i.test(body);

  out.pin = shown(scope.querySelector('.payment-pin-code, [class*="pin-code"]'), 8);

  out.errors = [];
  var errs = scope.querySelectorAll('.payment__errors, [class*="payment-alert--ERROR"], [class*="alert--error"], [class*="alert--ERROR"], [role="alert"]');
  for (var e = 0; e < errs.length && out.errors.length < 5; e++) {
    var t = text(errs[e]);
    if (t && shown(errs[e], 2)) out.errors.push(t.slice(0, 300));
  }

  // Every amount of money mentioned on the page. A number only counts when
  // a currency sign or code stands next to it ("over 18" is not a price).
  out.prices = [];
  var CUR = '(?:[A-Z]{0,3}\\$|[\u20ac\u00a3\u00a5\u20b9\u20bd\u20a9\u20ba\u20b4\u20ab\u0e3f\u20aa\u20b1\u20a6\u20b2\u20a1\u20b8\u20bc\u20be\u20ae\u20ad\u17db\ufdfc]|z\u0142|K\u010d|kr|Ft|lei|TL|RM|Rp|S\\/|\u043b\u0432|' +
    'USD|EUR|GBP|CAD|AUD|NZD|BRL|RUB|TRY|PLN|JPY|KRW|INR|CNY|MXN|ARS|CLP|COP|PEN|UAH|KZT|CHF|SEK|NOK|DKK|CZK|HUF|RON|BGN|ILS|SAR|AED|QAR|KWD|ZAR|THB|IDR|MYR|PHP|SGD|HKD|TWD|VND|UYU|CRC|GEL)';
  var NUM = '\\d{1,3}(?:[ .,\u00a0\u202f]\\d{3})+(?:[.,]\\d{1,2})?|\\d+(?:[.,]\\d{1,2})?';
  var PRICE = '(?:^|[^A-Za-z0-9])(' + CUR + '\\s?-?(?:' + NUM + ')|-?(?:' + NUM + ')\\s?' + CUR + ')(?![A-Za-z0-9])';
  var checkoutText = app && typeof app.innerText === 'string' && app.innerText ? text(app) : body;
  var priceRe = new RegExp(PRICE, 'g');
  var pm;
  while ((pm = priceRe.exec(checkoutText)) && out.prices.length < 40) {
    // An amount whose digits go on beyond what was recognised ("$0.0001",
    // "12.0,00 \u20ac") has not been read: it counts as an amount that is
    // not known to be zero.
    var from = pm.index + pm[0].length - pm[1].length, to = pm.index + pm[0].length;
    // ("Dead Island 2 0,00 \u20ac" is a title and a price; "1 000,00 \u20ac"
    // is one amount: a digit and a space before the amount belong to it
    // only when it starts with a group of three digits.)
    var lead = checkoutText.slice(Math.max(0, from - 2), from);
    var runsOn = /^[.,]\d/.test(checkoutText.substr(to, 2)) ||
      (/^\d/.test(pm[1]) && (/\d[.,]$/.test(lead) || (/\d[ \u00a0\u202f]$/.test(lead) && /^\d{3}(?!\d)/.test(pm[1]))));
    out.prices.push(runsOn ? 'an amount that could not be read' : pm[1]);
  }
  // Epic's own statement on the free checkout.
  out.saysFree = /\bthis (game |product |item |add-on |bundle )?is free\b/i.test(checkoutText);

  // What the buyer is shown as the total: an element whose class names it
  // ("total" as a word of its own, so not "subtotal") and whose text is an
  // amount of money or a word for free.
  out.totals = [];
  var hasMoney = new RegExp(PRICE);
  var freeWord = /\b(free|gratis|gratuit|kostenlos)\b/i;
  var tot = scope.querySelectorAll('[class*="total" i]');
  for (var m = 0; m < tot.length && out.totals.length < 12; m++) {
    if (!/(^|[\s_-])total([\s_-]|$)/i.test(cls(tot[m]))) continue;
    if (!shown(tot[m], 2)) continue;
    if (tot[m].querySelector('[class*="total" i]')) continue; // keep the innermost only
    var tt = text(tot[m]);
    if (tt && tt.length < 120 && (hasMoney.test(tt) || freeWord.test(tt))) out.totals.push(tt);
  }

  // A service worker in control of the page could relay requests out of
  // this program's sight.
  out.sw = false;
  try { out.sw = !!(navigator.serviceWorker && navigator.serviceWorker.controller); } catch (errSw) {}

  var pre = null;
  try { pre = window.__epic_web_purchase_dataPreload; } catch (err) {}
  try { out.preload = pre ? JSON.stringify(pre).slice(0, 400000) : ''; } catch (err2) { out.preload = ''; }

  out.success = /thanks? for your (order|purchase)|thank you for (buying|your order|your purchase)|order (is )?(complete|confirmed)|it.?s all yours|vielen dank f\u00fcr.{0,40}bestellung|bestellung erfolgreich/i.test(body);
  out.owned = /already own|you own this (item|product|game)/i.test(body);
  out.region = /unavailable in your region|not available in your (region|country)/i.test(body);
  return out;
})()`

// pageState mirrors what inspectJS returns.
type pageState struct {
	URL          string   `json:"url"`
	Title        string   `json:"title"`
	Ready        string   `json:"ready"`
	Text         string   `json:"text"`
	Login        bool     `json:"login"`
	BrowserCheck bool     `json:"browserCheck"`
	App          bool     `json:"app"`
	AppError     string   `json:"appError"`
	Place        *btn     `json:"place"`
	Guessed      bool     `json:"guessed"`
	Buttons      []string `json:"buttons"`
	Consent      *btn     `json:"consent"`
	Captcha      bool     `json:"captcha"`
	Pin          bool     `json:"pin"`
	Errors       []string `json:"errors"`
	Totals       []string `json:"totals"`
	Prices       []string `json:"prices"`
	SaysFree     bool     `json:"saysFree"`
	SW           bool     `json:"sw"`
	Throttled    bool     `json:"throttled"`
	Preload      string   `json:"preload"`
	Success      bool     `json:"success"`
	Owned        bool     `json:"owned"`
	Region       bool     `json:"region"`
}

type btn struct {
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	Text     string  `json:"text"`
	Disabled bool    `json:"disabled"`
	Loading  bool    `json:"loading"`
	// Hit is false when a click at the button's centre would land on
	// something else (it is covered, or outside the visible page). Pages
	// that do not report it are taken at their word.
	Hit *bool `json:"hit"`
}

func (b *btn) clickable() bool {
	return b != nil && !b.Disabled && !b.Loading && b.W >= 8 && b.H >= 8 && (b.Hit == nil || *b.Hit)
}

// offersValidationHash identifies the store's own "getOffersValidation"
// query (the one its cart uses to tell what an account already owns).
const offersValidationHash = "3c9bb0f213f6d0cb6bf056e6b206ba166c8dd59d014618e4d59bff11689f403a"

// ownershipJS asks the store, with the signed-in session of the page it runs
// in, whether the account already owns an offer. Read-only.
func ownershipJS(storeBase, namespace, offerID string) string {
	variables, _ := json.Marshal(map[string]any{"offers": []map[string]string{{"offerId": offerID, "namespace": namespace}}})
	extensions, _ := json.Marshal(map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": offersValidationHash}})
	base, _ := json.Marshal(storeBase)
	v, _ := json.Marshal(string(variables))
	x, _ := json.Marshal(string(extensions))
	return fmt.Sprintf(`(async function () {
  var u = %s + '/graphql?operationName=getOffersValidation&variables=' + encodeURIComponent(%s) + '&extensions=' + encodeURIComponent(%s);
  try {
    var r = await fetch(u, { credentials: 'include', headers: { accept: 'application/json' } });
    var t = await r.text();
    return { status: r.status, body: t.slice(0, 20000) };
  } catch (e) { return { status: 0, body: String(e) }; }
})()`, base, v, x)
}

// pageMappingHash identifies the store's "getMappingByPageSlug" query, which
// tells which offer a store page sells.
const pageMappingHash = "781fd69ec8116125fa8dc245c0838198cdf5283e31647d08dfa27f45ee8b1f30"

// pageMappingJS asks the store which offer belongs to a page. Read-only.
func pageMappingJS(origin, slug string) string {
	variables, _ := json.Marshal(map[string]string{"pageSlug": slug, "locale": "en-US"})
	extensions, _ := json.Marshal(map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": pageMappingHash}})
	base, _ := json.Marshal(origin)
	v, _ := json.Marshal(string(variables))
	x, _ := json.Marshal(string(extensions))
	return fmt.Sprintf(`(async function () {
  var u = %s + '/graphql?operationName=getMappingByPageSlug&variables=' + encodeURIComponent(%s) + '&extensions=' + encodeURIComponent(%s);
  try {
    var r = await fetch(u, { credentials: 'include', headers: { accept: 'application/json' } });
    var t = await r.text();
    return { status: r.status, body: t.slice(0, 20000) };
  } catch (e) { return { status: 0, body: String(e) }; }
})()`, base, v, x)
}

// underPointJS returns the label of the button under a point of the page
// (null when there is none), read the same way inspectJS reads it.
const underPointJS = `(function (x, y) {
  var el = document.elementFromPoint(x, y);
  while (el && !(el.tagName === 'BUTTON' || (el.getAttribute && el.getAttribute('role') === 'button') || (el.tagName === 'INPUT' && el.type === 'submit'))) el = el.parentElement;
  if (!el) return null;
  return (((typeof el.innerText === 'string' ? el.innerText : el.textContent) || '').replace(/\s+/g, ' ').trim()).slice(0, 80);
})`

// clickJS is the fallback for when synthetic mouse input is unavailable.
const clickJS = `(function (x, y) {
  var el = document.elementFromPoint(x, y);
  while (el && !(el.tagName === 'BUTTON' || el.getAttribute('role') === 'button' || el.tagName === 'INPUT')) el = el.parentElement;
  if (!el) return false;
  el.click();
  return true;
})`
