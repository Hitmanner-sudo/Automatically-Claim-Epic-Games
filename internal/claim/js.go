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
  function text(el) { return ((el && (el.innerText || el.textContent)) || '').replace(/\s+/g, ' ').trim(); }
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
  out.browserCheck = /just a moment|verify you are human|checking your browser|attention required/i.test(document.title + ' ' + body.slice(0, 400));

  var app = document.querySelector('#purchase-app');
  out.app = !!app;
  var msg = app && app.querySelector('.message');
  out.appError = msg ? text(msg).slice(0, 200) : '';

  var scope = app || document;
  var buttons = [];
  var all = scope.querySelectorAll('button, [role="button"], input[type="submit"]');
  for (var i = 0; i < all.length; i++) if (shown(all[i], 8)) buttons.push(all[i]);

  function pick(test) { for (var i = 0; i < buttons.length; i++) if (test(buttons[i])) return buttons[i]; return null; }
  var consent = pick(function (b) { return /payment-confirm__btn/.test(cls(b)) && !/secondary|cancel/i.test(cls(b)); }) ||
    pick(function (b) { return /^(i accept|i agree|accept)$/i.test(text(b)); });
  var place = pick(function (b) { return /payment-order-confirm__btn/.test(cls(b)); }) ||
    pick(function (b) { return /^place order$/i.test(text(b)); });
  if (!place && app) {
    // Unknown language or renamed classes: accept the checkout's single
    // primary button, but only when there is exactly one candidate.
    var prim = [];
    for (var j = 0; j < buttons.length; j++) {
      if (buttons[j] !== consent && /btn--primary|payment-btn--primary/.test(cls(buttons[j]))) prim.push(buttons[j]);
    }
    if (prim.length === 1) place = prim[0];
  }
  out.place = place ? button(place) : null;
  out.consent = consent && consent !== place ? button(consent) : null;

  // A CAPTCHA only counts when its challenge is actually on screen: the
  // checkout always carries invisible hCaptcha frames.
  out.captcha = false;
  var frames = document.querySelectorAll('iframe');
  for (var k = 0; k < frames.length; k++) {
    var f = frames[k];
    var id = (f.src || '') + ' ' + (f.id || '') + ' ' + (f.title || '') + ' ' + (f.parentElement ? (f.parentElement.id || '') + ' ' + cls(f.parentElement) : '');
    if (/hcaptcha|h_captcha|recaptcha|arkoselabs|funcaptcha|talon/i.test(id) && shown(f, 120)) out.captcha = true;
  }
  if (/one more step|please complete (a|the) security check|select all (images|squares)/i.test(body)) out.captcha = true;

  out.pin = !!scope.querySelector('.payment-pin-code, [class*="pin-code"]');

  out.errors = [];
  var errs = scope.querySelectorAll('.payment__errors, [class*="payment-alert--ERROR"], [class*="alert--error"], [class*="alert--ERROR"], [role="alert"]');
  for (var e = 0; e < errs.length && out.errors.length < 5; e++) {
    var t = text(errs[e]);
    if (t && shown(errs[e], 2)) out.errors.push(t.slice(0, 300));
  }

  // What the buyer is shown as the total.
  out.totals = [];
  var tot = scope.querySelectorAll('[class*="total" i]');
  for (var m = 0; m < tot.length && out.totals.length < 12; m++) {
    if (!shown(tot[m], 2)) continue;
    if (tot[m].querySelector('[class*="total" i]')) continue; // keep the innermost only
    var tt = text(tot[m]);
    if (tt && tt.length < 120) out.totals.push(tt);
  }

  var pre = null;
  try { pre = window.__epic_web_purchase_dataPreload; } catch (err) {}
  try { out.preload = pre ? JSON.stringify(pre).slice(0, 400000) : ''; } catch (err2) { out.preload = ''; }

  out.success = /thanks? for your (order|purchase)|thank you for (buying|your order|your purchase)|order (is )?(complete|confirmed)/i.test(body);
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
	Consent      *btn     `json:"consent"`
	Captcha      bool     `json:"captcha"`
	Pin          bool     `json:"pin"`
	Errors       []string `json:"errors"`
	Totals       []string `json:"totals"`
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
}

func (b *btn) clickable() bool { return b != nil && !b.Disabled && !b.Loading && b.W >= 8 && b.H >= 8 }

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

// clickJS is the fallback for when synthetic mouse input is unavailable.
const clickJS = `(function (x, y) {
  var el = document.elementFromPoint(x, y);
  while (el && !(el.tagName === 'BUTTON' || el.getAttribute('role') === 'button' || el.tagName === 'INPUT')) el = el.parentElement;
  if (!el) return false;
  el.click();
  return true;
})`
