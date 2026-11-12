package claim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp/cdptest"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
)

var (
	browserOnce sync.Once
	browserBase string
)

// sharedBrowser starts one Chromium for the whole package.
func sharedBrowser(t *testing.T) string {
	t.Helper()
	browserOnce.Do(func() { browserBase = cdptest.Start(&keepAlive{TB: t}) })
	if browserBase == "" {
		t.Skip("no Chromium available")
	}
	return browserBase
}

// keepAlive stops cdptest from tying the browser's lifetime to the first test.
type keepAlive struct{ testing.TB }

func (k *keepAlive) Cleanup(func()) {}

func testOffer() feed.Offer {
	return feed.Offer{
		Title: "Flotsam", Namespace: "ns", OfferID: "id", Slug: "flotsam",
		OriginalPrice: 2499, DiscountPrice: 0,
		Start: time.Now().Add(-time.Hour), End: time.Now().Add(24 * time.Hour),
	}
}

type run struct {
	res   Result
	store *mockStore
	logs  []string
	diag  string
}

func runScenario(t *testing.T, sc scenario, mod func(*Options)) run {
	t.Helper()
	base := sharedBrowser(t)
	store := newMockStore(t, sc)
	r := run{store: store, diag: t.TempDir()}
	opt := Options{
		CheckoutBases: []string{store.URL()}, LoginBase: store.URL(),
		DiagDir:    r.diag,
		UIWait:     20 * time.Second,
		SettleWait: 50 * time.Millisecond,
		LoginGrace: 2 * time.Second,
		// Short waits keep the negative cases quick.
		CheckoutWait: 12 * time.Second,
		OrderWait:    14 * time.Second,
		Logf: func(f string, a ...any) {
			t.Logf(f, a...)
		},
	}
	if mod != nil {
		mod(&opt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := Open(ctx, base, opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	r.res = s.Claim(ctx, testOffer())
	t.Logf("result: %v", r.res)
	return r
}

func wantOutcome(t *testing.T, r run, want Outcome, orders int) {
	t.Helper()
	if r.res.Outcome != want {
		t.Errorf("outcome = %v, want %v", r.res, want)
	}
	// Give a stray request a moment to arrive before counting.
	time.Sleep(150 * time.Millisecond)
	if got := r.store.orderCount(); got != orders {
		t.Errorf("orders that reached the store = %d, want %d (last: %s)", got, orders, r.store.lastOrder())
	}
}

func TestClaimsFreeOffer(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, Claimed, 1)
	if !strings.Contains(r.store.lastOrder(), `"totalAmount":0`) {
		t.Errorf("order body = %s", r.store.lastOrder())
	}
	if entries, _ := os.ReadDir(r.diag); len(entries) != 0 {
		t.Errorf("a successful claim should leave no diagnostics, found %d", len(entries))
	}
}

func TestWaitsForTheButtonToBeReady(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), SlowMs: 1800}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestConsentDialogForEUAccounts(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "0,00 €", ConfirmTotal: intp(0), EU: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestNonEnglishCheckout(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "0,00 €", ConfirmTotal: intp(0), EU: true, German: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestAlreadyOwnedIsNotOrderedAgain(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), Owned: true}, nil)
	wantOutcome(t, r, Owned, 0)
}

func TestOwnedMessageFromCheckout(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), OwnedMessage: true, GraphQLDown: true}, nil)
	wantOutcome(t, r, Owned, 0)
}

func TestOwnedMessageContradictedByStoreIsRetriedLater(t *testing.T) {
	// The page claims "already own" but the store's own answer is "no":
	// must not be recorded as owned (that would stop all further attempts).
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), OwnedMessage: true}, nil)
	wantOutcome(t, r, Failed, 0)
}

func TestMissingBaseGame(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), MissingBase: true}, nil)
	wantOutcome(t, r, Unavailable, 0)
}

func TestRegionLocked(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), Region: true}, nil)
	wantOutcome(t, r, Unavailable, 0)
}

func TestParentalPinIsNotHandled(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), Pin: true}, nil)
	wantOutcome(t, r, Unavailable, 0)
}

// ---- the price guarantees

func TestPaidTotalOnPageIsNeverOrdered(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$19.99", PreviewTotal: 1999, ConfirmTotal: intp(1999)}, nil)
	wantOutcome(t, r, NotFree, 0)
}

func TestPaidPreviewAloneBlocks(t *testing.T) {
	// The page shows no total at all; Epic's preview says it costs money.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "", PreviewTotal: 1999, ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, NotFree, 0)
}

func TestOrderRequestWithPaidTotalIsStoppedAtTheNetwork(t *testing.T) {
	// Everything visible says free, yet the order the page tries to send
	// states a price. The request must never leave the browser.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", PreviewTotal: 0, ConfirmTotal: intp(1999)}, nil)
	wantOutcome(t, r, NotFree, 0)
}

func TestPaidFormPostIsStoppedAtTheNetwork(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), FormPost: true}, nil)
	wantOutcome(t, r, NotFree, 0)
}

func TestUnverifiablePriceIsNotOrdered(t *testing.T) {
	// No total on the page, no preview, no total in the order: nothing
	// confirms the price is zero, so nothing may be sent.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "", NoPreview: true, ConfirmTotal: nil}, nil)
	wantOutcome(t, r, Failed, 0)
	if !strings.Contains(r.res.Detail, "could not be confirmed") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestOrderWithoutStatedTotalNeedsEvidenceFromThePage(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", NoPreview: true, ConfirmTotal: nil}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestNoButtonIsPressedWithoutEvidence(t *testing.T) {
	// The order this page would send states a zero total, but nothing on
	// the page itself confirms the price beforehand: the button is not
	// pressed at all.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "", NoPreview: true, ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, Failed, 0)
	if !strings.Contains(r.res.Detail, "could not be confirmed") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

// ---- Epic's 2026 free checkout ("Add to library" / "It's all yours")

func TestNewCheckoutLayout(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestNewCheckoutLayoutWithoutOwnershipQuery(t *testing.T) {
	// Success is then read from the page ("It's all yours").
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, GraphQLDown: true}, nil)
	wantOutcome(t, r, Claimed, 1)
	if !strings.Contains(r.res.Detail, "confirmation") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestNewCheckoutLayoutInAnotherLanguage(t *testing.T) {
	// No English sentence and no known label: the only price on the page
	// is zero, and the page has a single button that is not a way out.
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, German: true, NewPrice: "0,00 €", NoFreeText: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestUnrecognisedCheckoutIsReportedAsSuch(t *testing.T) {
	// The old layout in a language whose labels are unknown, with the class
	// names gone: two buttons, neither recognisable. It must be reported as
	// an unrecognised checkout, not as "signed out", and nothing is pressed.
	base := sharedBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>x</title><body><div id="purchase-app"><p>0,00 zł</p>
<button style="width:200px;height:40px">Zamów teraz</button> <button style="width:200px;height:40px">Dodaj kod</button></div></body>`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{srv.URL}, LoginBase: srv.URL, SettleWait: 50 * time.Millisecond, CheckoutWait: 5 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Failed || !strings.Contains(res.Detail, "not recognised") || !strings.Contains(res.Detail, "Dodaj kod") {
		t.Fatalf("result = %v", res)
	}
}

func TestNewCheckoutLayoutShowingAPriceIsNotOrdered(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, NewPrice: "$19.99", NoFreeText: true}, nil)
	wantOutcome(t, r, Failed, 0)
	if !strings.Contains(r.res.Detail, "could not be confirmed") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestNewCheckoutLayoutWithNoPriceAtAllIsNotOrdered(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, NewPrice: "Included", NoFreeText: true}, nil)
	wantOutcome(t, r, Failed, 0)
}

func TestThrottledAccount(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, Throttled: true}, nil)
	wantOutcome(t, r, Throttled, 0)
}

func TestDryRunPlacesNoOrder(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)}, func(o *Options) { o.DryRun = true })
	wantOutcome(t, r, DryRun, 0)
}

func TestFeedPriceMustBeZero(t *testing.T) {
	base := sharedBrowser(t)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{store.URL()}, LoginBase: store.URL(), SettleWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	o := testOffer()
	o.DiscountPrice = 1999
	if res := s.Claim(ctx, o); res.Outcome != NotFree {
		t.Errorf("paid offer: %v", res)
	}
	o = testOffer()
	o.DiscountPrice = -1 // unknown
	if res := s.Claim(ctx, o); res.Outcome != NotFree {
		t.Errorf("offer with unknown price: %v", res)
	}
	o = testOffer()
	o.End = time.Now().Add(30 * time.Second)
	if res := s.Claim(ctx, o); res.Outcome != Failed {
		t.Errorf("offer about to end: %v", res)
	}
	if store.orderCount() != 0 {
		t.Errorf("orders = %d", store.orderCount())
	}
}

// ---- captcha, sign-in, errors

func TestCaptchaAfterPressIsLeftAlone(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), Captcha: true}, nil)
	wantOutcome(t, r, Captcha, 0)
}

func TestCaptchaBeforePressIsLeftAlone(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), CaptchaFirst: true}, nil)
	wantOutcome(t, r, Captcha, 0)
}

func TestSignedOutEndsAtSignInPage(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: false, HopWorks: false, DomTotal: "$0.00", ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, NotSignedIn, 0)
	// A failed attempt leaves evidence behind.
	entries, _ := os.ReadDir(r.diag)
	if len(entries) != 1 {
		t.Fatalf("diagnostics folders = %d", len(entries))
	}
	for _, f := range []string{"summary.json", "screenshot.jpg", "page.html", "targets.json"} {
		if st, err := os.Stat(filepath.Join(r.diag, entries[0].Name(), f)); err != nil || st.Size() == 0 {
			t.Errorf("diagnostic file %s missing: %v", f, err)
		}
	}
}

func TestSignInHandOverRecoversWebSession(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: false, HopWorks: true, DomTotal: "$0.00", ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestRejectedOrder(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), ConfirmError: "Sorry, something went wrong with your order."}, nil)
	wantOutcome(t, r, Failed, 1)
	if !strings.Contains(r.res.Detail, "something went wrong") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestRejectedBecauseAlreadyOwned(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), GraphQLDown: true, ConfirmError: "You already own this item."}, nil)
	wantOutcome(t, r, Owned, 1)
}

func TestWorksWithoutOwnershipQuery(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), GraphQLDown: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

// ---- choosing the launcher page, sign-in detection

func TestSurveyTargets(t *testing.T) {
	store := []string{"https://launcher.store.epicgames.com", "https://www.epicgames.com/store", "https://store.epicgames.com"}
	ws := "ws://x"
	login := cdp.Target{Type: "page", URL: "https://www.epicgames.com/id/login?loginId=abc", WebSocketDebuggerURL: ws}
	storePage := cdp.Target{Type: "page", URL: "https://store.epicgames.com/en-US/", WebSocketDebuggerURL: ws}
	other := cdp.Target{Type: "page", URL: "https://launcher-website-prod.ak.epicgames.com/x", WebSocketDebuggerURL: ws}
	blank := cdp.Target{Type: "page", URL: "about:blank", WebSocketDebuggerURL: ws}
	captcha := cdp.Target{Type: "iframe", URL: "https://newassets.hcaptcha.com/captcha/v1/x/static/hcaptcha.html#frame=challenge", WebSocketDebuggerURL: ws}
	busy := cdp.Target{Type: "page", URL: "https://store.epicgames.com/en-US/"} // another debugger attached

	sv := surveyTargets([]cdp.Target{login, captcha}, store)
	if !sv.Login || sv.BestScore != -1 || sv.Pages != 1 {
		t.Errorf("signed-out launcher: %+v", sv)
	}
	sv = surveyTargets([]cdp.Target{blank, other, storePage, captcha}, store)
	if sv.Login || sv.Best.URL != storePage.URL || sv.BestScore != 3 {
		t.Errorf("store page should win: %+v", sv)
	}
	sv = surveyTargets([]cdp.Target{blank, other}, store)
	if sv.Best.URL != other.URL || sv.BestScore != 2 {
		t.Errorf("epic page should beat blank: %+v", sv)
	}
	sv = surveyTargets([]cdp.Target{blank}, store)
	if sv.BestScore != 0 {
		t.Errorf("blank page: %+v", sv)
	}
	sv = surveyTargets([]cdp.Target{busy}, store)
	if sv.BestScore != -1 {
		t.Errorf("a page without a debugger URL is unusable: %+v", sv)
	}
	sv = surveyTargets(nil, store)
	if sv.BestScore != -1 || sv.Login {
		t.Errorf("no targets: %+v", sv)
	}

	for u, want := range map[string]bool{
		"https://www.epicgames.com/id/login?loginId=1": true,
		"https://www.epicgames.com/id/login/mfa":       true,
		"https://epicgames.com/id/register":            true,
		"https://store.epicgames.com/en-US/p/flotsam":  false,
		"https://www.epicgames.com/identity":           false,
		"https://evil.example/id/login":                false,
		"https://notepicgames.com/id/login":            false,
		"::bad":                                        false,
	} {
		if got := isLoginURL(u); got != want {
			t.Errorf("isLoginURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestOpenReportsSignedOutLauncher(t *testing.T) {
	// A fake DevTools endpoint whose only page is Epic's sign-in page, as the
	// launcher shows when nobody is signed in.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"1","type":"page","title":"Sign in to Your Epic Games account | Epic Games","url":"https://www.epicgames.com/id/login?loginId=x","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/1"},
		{"id":"2","type":"iframe","title":"hcaptcha","url":"https://newassets.hcaptcha.com/x#frame=challenge","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/2"}]`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Open(ctx, srv.URL, Options{LoginGrace: 600 * time.Millisecond, UIWait: 10 * time.Second})
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v, want ErrNotSignedIn", err)
	}
}

func TestOpenTimesOutWithoutInterface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Open(ctx, srv.URL, Options{UIWait: 700 * time.Millisecond})
	if err == nil || errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

func TestParseOwnership(t *testing.T) {
	cases := map[string]ownership{
		`{"data":{"Entitlements":{"cartOffersValidation":{"conflictingOffers":null,"missingPrerequisites":null,"fullyOwnedOffers":null,"unablePartiallyUpgradeOffers":null}}}}`: ownNo,
		`{"data":{"Entitlements":{"cartOffersValidation":{"fullyOwnedOffers":[{"namespace":"n","offerId":"o"}]}}}}`:                                                             ownYes,
		`{"data":{"Entitlements":{"cartOffersValidation":{"missingPrerequisites":[{"namespace":"n","offerId":"o"}]}}}}`:                                                         ownBlocked,
		`{"data":{"Entitlements":{"cartOffersValidation":{"conflictingOffers":[{}]}}}}`:                                                                                         ownBlocked,
		`{"errors":[{"message":"PersistedQueryNotFound"}]}`:                                                                                                                     ownUnknown,
		`{"data":{"Entitlements":null}}`: ownUnknown,
		`not json`:                       ownUnknown,
	}
	for body, want := range cases {
		if got := parseOwnership([]byte(body)); got != want {
			t.Errorf("parseOwnership(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestOrderWatchRules(t *testing.T) {
	const confirm = "https://payment-website-pci.ol.epicgames.com/purchase/confirm-order"
	const preview = "https://payment-website-pci.ol.epicgames.com/purchase/order-preview"

	// reads are never held up
	w := newOrderWatch(false)
	if !w.onRequest("GET", confirm, "", false) {
		t.Error("GET must pass")
	}
	// a preview request carries no total and is not an order
	if !w.onRequest("POST", preview, `{"offers":["x"]}`, true) {
		t.Error("preview request must pass")
	}
	// an order with no stated total and no prior evidence is refused
	if w.onRequest("POST", confirm, `{"offers":["x"]}`, true) || w.blockedReason() == "" || w.submitted() {
		t.Error("unverifiable order must be refused")
	}

	// with evidence from the page it passes
	w = newOrderWatch(false)
	w.setPreClick(Free)
	if !w.onRequest("POST", confirm, `{"offers":["x"]}`, true) || !w.submitted() {
		t.Error("order backed by page evidence must pass")
	}

	// with evidence from the preview it passes
	w = newOrderWatch(false)
	w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":0}}`))
	if w.previewVerdict() != Free || !w.onRequest("POST", confirm, `{"offers":["x"]}`, true) {
		t.Error("order backed by preview evidence must pass")
	}

	// a stated price always loses, whatever the page said
	w = newOrderWatch(false)
	w.setPreClick(Free)
	if w.onRequest("POST", confirm, `{"totalAmount":499}`, true) || !w.sawPaid() {
		t.Error("paid order must be refused")
	}
	// ...and once refused, nothing further is let through
	if w.onRequest("POST", confirm, `{"totalAmount":0}`, true) {
		t.Error("after a refusal no order may pass")
	}

	// a paid preview poisons the claim
	w = newOrderWatch(false)
	w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":1999}}`))
	if w.previewVerdict() != Paid || !w.sawPaid() {
		t.Error("paid preview must be recorded")
	}

	// dry run refuses every order
	w = newOrderWatch(true)
	w.setPreClick(Free)
	if w.onRequest("POST", confirm, `{"totalAmount":0}`, true) {
		t.Error("dry run must refuse orders")
	}

	// replies
	w = newOrderWatch(false)
	w.onResponse("POST", confirm, 200, []byte(`{"confirmation":"A1","error":null}`))
	if ok, msg := w.confirmation(); !ok || msg != "" {
		t.Errorf("accepted order: %v %q", ok, msg)
	}
	w = newOrderWatch(false)
	w.onResponse("POST", confirm, 200, []byte(`{"error":"errors.x","message":"You already own this item."}`))
	if ok, msg := w.confirmation(); ok || msg != "You already own this item." || !w.ownedHint() {
		t.Errorf("rejected order: %v %q", ok, msg)
	}
	w = newOrderWatch(false)
	w.onResponse("POST", confirm, 500, []byte(`oops`))
	if ok, msg := w.confirmation(); ok || msg != "HTTP 500" {
		t.Errorf("server error: %v %q", ok, msg)
	}
	// words inside an unrelated file are not statements about the account
	w = newOrderWatch(false)
	w.onResponse("GET", "https://static.example/order-strings.json", 200, []byte(`{"msg":"You already own this item."}`))
	if w.ownedHint() {
		t.Error("a translation file must not mark the offer as owned")
	}

	for u, want := range map[string]bool{
		"https://x/purchase/confirm-order":                     true,
		"https://x/api/shared/accounts/1/orders/quickPurchase": true,
		"https://x/v1/orders":                                  true,
		"https://x/placeOrder":                                 true,
		"https://x/purchase/order-preview":                     false,
		"https://x/orders/123/status":                          false,
		"https://x/telemetry?event=confirm-order":              false,
	} {
		if got := isOrderSubmit("POST", u); got != want {
			t.Errorf("isOrderSubmit(%q) = %v, want %v", u, got, want)
		}
	}
	if isOrderSubmit("GET", "https://x/purchase/confirm-order") {
		t.Error("GET is never an order")
	}
}

func TestStoreSiteRecognition(t *testing.T) {
	bases := []string{"https://launcher.store.epicgames.com", "https://www.epicgames.com/store", "https://store.epicgames.com"}
	ws := "ws://x"
	launcherStore := cdp.Target{Type: "page", URL: "https://launcher.store.epicgames.com/store", WebSocketDebuggerURL: ws}
	account := cdp.Target{Type: "page", URL: "https://www.epicgames.com/store/whatever", WebSocketDebuggerURL: ws}
	other := cdp.Target{Type: "page", URL: "https://egs-download-manager.store.on.epicgames.com/home", WebSocketDebuggerURL: ws}
	sv := surveyTargets([]cdp.Target{account, other, launcherStore}, bases)
	if sv.Best.URL != launcherStore.URL || sv.BestScore != 3 {
		t.Errorf("the launcher's own store page should win: %+v", sv)
	}
	if targetScore(account, bases) != 2 {
		t.Errorf("www.epicgames.com is not a store site just because a checkout path lives there")
	}
	withMock := append(append([]string{}, bases...), "http://127.0.0.1:5555")
	for u, want := range map[string]string{
		"https://launcher.store.epicgames.com/purchase?offers=1": "https://launcher.store.epicgames.com",
		"https://store.epicgames.com/en-US/p/x":                  "https://store.epicgames.com",
		"https://www.epicgames.com/store/purchase":               "",
		"https://evilstore.epicgames.com.example/x":              "",
		"https://notstore.epicgames.com/x":                       "",
		"https://xstore.epicgames.com/x":                         "",
		"file:///C:/x.html":                                      "",
		"http://127.0.0.1:5555/purchase":                         "http://127.0.0.1:5555",
		"http://127.0.0.1:5556/purchase":                         "",
	} {
		if got := storeOrigin(u, withMock); got != want {
			t.Errorf("storeOrigin(%q) = %q, want %q", u, got, want)
		}
	}
}

// ---- several checkout addresses

func TestFallsThroughToAWorkingCheckoutAddress(t *testing.T) {
	base := sharedBrowser(t)
	// First address: a site with no checkout at all. Second: signed out
	// (error page). Third: works.
	nothing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<!doctype html><title>404</title><body><h1>Page not found</h1></body>"))
	}))
	defer nothing.Close()
	signedOut := newMockStore(t, scenario{SignedIn: false, DomTotal: "$0.00", ConfirmTotal: intp(0)})
	working := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{
		CheckoutBases: []string{nothing.URL, signedOut.URL(), working.URL()},
		LoginBase:     working.URL(),
		SettleWait:    50 * time.Millisecond,
		CheckoutWait:  30 * time.Second,
		OrderWait:     14 * time.Second,
		Logf:          t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Claimed {
		t.Fatalf("result = %v", res)
	}
	if signedOut.orderCount() != 0 || working.orderCount() != 1 {
		t.Errorf("orders: signed-out store %d, working store %d", signedOut.orderCount(), working.orderCount())
	}
}

func TestDefinitiveAnswerStopsTheSearch(t *testing.T) {
	base := sharedBrowser(t)
	// The first address answers "already owned"; the second must not be tried.
	first := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), Owned: true})
	second := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{first.URL(), second.URL()}, LoginBase: first.URL(), SettleWait: 50 * time.Millisecond, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if res := s.Claim(ctx, testOffer()); res.Outcome != Owned {
		t.Fatalf("result = %v", res)
	}
	if first.orderCount() != 0 || second.orderCount() != 0 {
		t.Errorf("orders: %d, %d", first.orderCount(), second.orderCount())
	}
}

// ---- holiday mystery games

func TestMysteryGameIsLookedUpByItsPage(t *testing.T) {
	base := sharedBrowser(t)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{store.URL()}, LoginBase: store.URL(), SettleWait: 50 * time.Millisecond, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	o := testOffer()
	o.Placeholder, o.Namespace, o.OfferID, o.Slug = true, "d5241c76f178492ea1540fce45616757", "slot-9", "revealed-game"
	if res := s.Claim(ctx, o); res.Outcome != Claimed {
		t.Fatalf("result = %v", res)
	}
	store.mu.Lock()
	got := store.lastOffers
	store.mu.Unlock()
	if got != "1-real-ns-real-id" {
		t.Fatalf("checkout was opened for %q; the stand-in identifiers must never be ordered", got)
	}

	// A slot whose page the store does not know is not ordered at all.
	before := store.orderCount()
	o.Slug = "unknown-page"
	if res := s.Claim(ctx, o); res.Outcome != Failed || !strings.Contains(res.Detail, "mystery game") {
		t.Fatalf("result = %v", res)
	}
	o.Slug = ""
	if res := s.Claim(ctx, o); res.Outcome != Failed {
		t.Fatalf("unrevealed slot: %v", res)
	}
	if store.orderCount() != before {
		t.Fatal("an unresolved mystery game must not be ordered")
	}
}

func TestParsePageMapping(t *testing.T) {
	ns, id, ok := parsePageMapping([]byte(`{"data":{"StorePageMapping":{"mapping":{"pageSlug":"x","mappings":{"offer":{"id":"o","namespace":"n"}}}}}}`))
	if !ok || ns != "n" || id != "o" {
		t.Fatalf("got %q %q %v", ns, id, ok)
	}
	for _, body := range []string{
		`{"data":{"StorePageMapping":{"mapping":null}}}`,
		`{"data":{"StorePageMapping":{"mapping":{"mappings":{"offer":null}}}}}`,
		`{"data":{"StorePageMapping":{"mapping":{"mappings":{"offer":{"id":"","namespace":"n"}}}}}}`,
		`{"errors":[{"message":"PersistedQueryNotFound"}]}`,
		`nope`,
	} {
		if _, _, ok := parsePageMapping([]byte(body)); ok {
			t.Errorf("should not resolve: %s", body)
		}
	}
}
