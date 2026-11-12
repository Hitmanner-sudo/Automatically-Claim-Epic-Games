package claim

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
)

// productJS reports the state of a game's store page: its main button and
// whatever stands between the visitor and the checkout. It only reads.
const productJS = `(function () {
  function text(el) { return ((el && (typeof el.innerText === 'string' ? el.innerText : el.textContent)) || '').replace(/\s+/g, ' ').trim(); }
  function shown(el, min) {
    if (!el) return false;
    var r = el.getBoundingClientRect();
    if (r.width < (min || 4) || r.height < (min || 4)) return false;
    var cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none' || parseFloat(cs.opacity) === 0) return false;
    return r.bottom > 0 && r.right > 0 && r.top < (innerHeight || 1e9) && r.left < (innerWidth || 1e9);
  }
  function button(el) {
    var r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height, text: text(el).slice(0, 60),
      disabled: !!el.disabled || el.getAttribute('aria-disabled') === 'true', loading: /^loading/i.test(text(el)) };
  }
  var out = { url: location.href, title: document.title, ready: document.readyState, buttons: [] };
  var body = document.body ? text(document.body) : '';
  out.login = /(^|\.)epicgames\.com$/.test(location.hostname) && /^\/id\/(login|register)/.test(location.pathname || '') ||
    !!document.querySelector('form input[type="password"], input#email[name="email"]');
  out.captcha = false;
  var cf = false;
  try { cf = typeof window._cf_chl_opt !== 'undefined'; } catch (errCf) {}
  out.browserCheck = cf || !!document.querySelector('.cf_challenge_container, h1[data-t="challengeTitle"], #challenge-form') ||
    /just a moment|one more step|complete (a|the) security check/i.test(document.title + ' ' + body.slice(0, 400));
  var frames = document.querySelectorAll('iframe');
  for (var k = 0; k < frames.length; k++) {
    var f = frames[k];
    var id = (f.src || '') + ' ' + (f.id || '') + ' ' + (f.title || '');
    if (/hcaptcha|h_captcha|recaptcha|arkoselabs|funcaptcha|talon/i.test(id) && shown(f, 120)) out.captcha = true;
  }
  var all = document.querySelectorAll('button, [role="button"]');
  var buttons = [];
  for (var i = 0; i < all.length; i++) if (shown(all[i], 8)) buttons.push(all[i]);
  for (var b = 0; b < buttons.length && out.buttons.length < 15; b++) { var t = text(buttons[b]).slice(0, 40); if (t) out.buttons.push(t); }
  function find(re) { for (var i = 0; i < buttons.length; i++) if (re.test(text(buttons[i]))) return buttons[i]; return null; }

  var cta = document.querySelector('button[data-testid="purchase-cta-button"]');
  if (cta && !shown(cta, 8)) cta = null;
  if (!cta) cta = find(/^(get|free|claim)$/i);
  out.cta = cta ? button(cta) : null;
  var label = cta ? text(cta) : '';
  // Read from the page's main button only: other buttons on the page
  // belong to other editions and add-ons.
  out.owned = /^(in library|owned)$/i.test(label);
  out.needsBase = /requires base game/i.test(label);
  out.notYet = /^(unavailable|coming soon)$/i.test(label);
  out.region = /unavailable in your platform or region/i.test(body);
  var gate = find(/^continue$/i);
  out.gate = gate ? button(gate) : null;
  var yes = find(/^yes, buy now$/i);
  out.confirm = yes ? button(yes) : null;
  out.eula = /end user license agreement/i.test(body) && !!document.querySelector('input#agree');
  out.inPage = !!document.querySelector('iframe[title="Checkout"], iframe[src*="/checkout"], #webPurchaseContainer iframe');
  return out;
})()`

type productState struct {
	URL          string   `json:"url"`
	Title        string   `json:"title"`
	Ready        string   `json:"ready"`
	Buttons      []string `json:"buttons"`
	Login        bool     `json:"login"`
	Captcha      bool     `json:"captcha"`
	BrowserCheck bool     `json:"browserCheck"`
	CTA          *btn     `json:"cta"`
	Owned        bool     `json:"owned"`
	NeedsBase    bool     `json:"needsBase"`
	NotYet       bool     `json:"notYet"`
	Region       bool     `json:"region"`
	Gate         *btn     `json:"gate"`
	Confirm      *btn     `json:"confirm"`
	EULA         bool     `json:"eula"`
	InPage       bool     `json:"inPage"`
}

var (
	reGetLabel  = regexp.MustCompile(`(?i)^(get|free|claim)$`)
	rePaidLabel = regexp.MustCompile(`(?i)buy|purchase|pre-?order|cart|\d`)
)

// findStorePage looks for the launcher page showing the offer's store page
// and makes it the current page.
func (s *Session) findStorePage(ctx context.Context, o feed.Offer, wait time.Duration) bool {
	needle := "/" + strings.ToLower(o.Slug)
	deadline := time.Now().Add(wait)
	for {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		targets, err := cdp.Targets(tctx, s.base)
		cancel()
		if err == nil {
			for _, t := range targets {
				if t.Type != "page" || !strings.Contains(strings.ToLower(t.URL), needle) || isCheckoutURL(t.URL, s.opt.CheckoutBases) {
					continue
				}
				if at := s.attached(t.ID); at != nil {
					s.page = at.page
					return true
				}
				if t.WebSocketDebuggerURL == "" {
					continue
				}
				if nt, err := s.attach(ctx, t); err == nil {
					s.page = nt.page
					return true
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		if sleep(ctx, time.Second) != nil {
			return false
		}
	}
}

// viaStorePage claims the way a person does: the launcher shows the game's
// store page, "Get" is pressed there, the launcher opens its own checkout
// window, and the order is placed in it (with the same price checks as
// always).
func (s *Session) viaStorePage(ctx context.Context, o feed.Offer, w *orderWatch) (Result, pageState) {
	var last pageState
	known := s.targetIDs(ctx)

	found := false
	for try := 0; try < 2 && !found && ctx.Err() == nil; try++ {
		if err := s.opt.OpenProduct(ctx, o, try); err != nil {
			s.opt.Logf("%s: could not ask the launcher for the store page: %v", o.Title, err)
			continue
		}
		found = s.findStorePage(ctx, o, 30*time.Second)
	}
	if !found {
		return Result{Failed, "the launcher did not show the game's store page"}, last
	}

	deadline := time.Now().Add(s.opt.StorePageWait)
	var pressedAt, lastGate time.Time
	presses := 0
	var ps productState
	for {
		if ctx.Err() != nil {
			return Result{Failed, "interrupted"}, last
		}
		if s.adoptCheckout(ctx, o, known) {
			break
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.page.Eval(cctx, productJS, &ps)
		cancel()
		if err == nil {
			last = pageState{URL: ps.URL, Title: ps.Title, Ready: ps.Ready, Buttons: ps.Buttons}
			switch {
			case ps.Login:
				return Result{NotSignedIn, "the store page asks to sign in"}, last
			case ps.Captcha:
				return Result{Captcha, "Epic asked for a human check on the store page"}, last
			case ps.BrowserCheck:
				// Passes by itself, or the deadline below ends the wait.
				if sleep(ctx, 2*time.Second) != nil {
					return Result{Failed, "interrupted"}, last
				}
				if time.Now().After(deadline) {
					return Result{Captcha, "Epic's site kept showing its browser check instead of the store page"}, last
				}
				continue
			case ps.Owned:
				// The page may be that of the main game while the giveaway
				// is an add-on or an edition of it, so the store is asked
				// about this very offer. Without its answer, the button is
				// only believed for a game or bundle, whose page is its own.
				switch s.ownership(ctx, pageState{URL: ps.URL}, o) {
				case ownYes:
					return Result{Owned, "the store page shows it is in the library"}, last
				case ownUnknown:
					if o.OfferType == "" || o.OfferType == "BASE_GAME" || o.OfferType == "BUNDLE" {
						return Result{Owned, "the store page shows it is in the library"}, last
					}
				}
				return Result{Failed, "the store page says \"In Library\", but that could not be confirmed for this very offer; will look again later"}, last
			case ps.NeedsBase:
				return Result{Later, "it needs a base game that is not in the library"}, last
			case ps.NotYet:
				return Result{Later, "the store page does not offer it right now"}, last
			case ps.Region:
				return Result{Unavailable, "the store page says it is unavailable in this region"}, last
			case ps.EULA:
				return Result{Failed, "the game asks you to accept its licence agreement first; get it once by hand in the launcher"}, last
			case ps.InPage:
				return Result{Failed, "the checkout opened inside the store page, a layout this program does not handle yet"}, last
			}
			switch {
			case ps.Confirm.clickable() && time.Since(lastGate) > 2*time.Second:
				// "This edition contains something you already have."
				lastGate = time.Now()
				_ = s.click(ctx, ps.Confirm)
			case ps.Gate.clickable() && time.Since(lastGate) > 2*time.Second:
				// Mature-content notice, or "device not supported".
				lastGate = time.Now()
				_ = s.click(ctx, ps.Gate)
			case ps.CTA.clickable():
				label := strings.TrimSpace(ps.CTA.Text)
				switch {
				case reGetLabel.MatchString(label):
					if presses == 0 || (presses < 3 && time.Since(pressedAt) > 20*time.Second) {
						presses++
						pressedAt = time.Now()
						if err := s.click(ctx, ps.CTA); err != nil {
							return Result{Failed, "could not press the store page's button: " + err.Error()}, last
						}
					}
				case rePaidLabel.MatchString(label):
					// Nothing is pressed; and since a store page can show a
					// different edition than the one given away, this is
					// not taken as proof that the giveaway itself costs money.
					return Result{Failed, fmt.Sprintf("the store page's button reads %q, not \"Get\"; nothing was pressed", label)}, last
				}
			}
		}
		if time.Now().After(deadline) {
			if presses == 0 {
				return Result{Failed, fmt.Sprintf("no \"Get\" button was found on the store page (buttons: %q)", ps.Buttons)}, last
			}
			return Result{Failed, "pressing \"Get\" on the store page did not open a checkout"}, last
		}
		if sleep(ctx, time.Second) != nil {
			return Result{Failed, "interrupted"}, last
		}
	}

	s.opt.Logf("%s: the launcher opened its checkout window from the store page", o.Title)
	res, last, _ := s.order(ctx, o, w, false, known)
	return res, last
}
