package claim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		if os.Getenv("CHROMIUM_REQUIRED") != "" {
			t.Fatal("no Chromium available, and CHROMIUM_REQUIRED is set")
		}
		t.Skip("no Chromium available")
	}
	return browserBase
}

// keepAlive stops cdptest from tying the browser's lifetime to the first
// test: what it would clean up after that test is done when the whole
// package has run (see TestMain).
type keepAlive struct{ testing.TB }

var (
	browserCleanupMu sync.Mutex
	browserCleanups  []func()
)

func (k *keepAlive) Cleanup(f func()) {
	browserCleanupMu.Lock()
	browserCleanups = append(browserCleanups, f)
	browserCleanupMu.Unlock()
}

// TestMain shuts the shared browser down at the end, so that test runs do
// not leave browsers behind.
func TestMain(m *testing.M) {
	code := m.Run()
	browserCleanupMu.Lock()
	for i := len(browserCleanups) - 1; i >= 0; i-- {
		browserCleanups[i]()
	}
	browserCleanupMu.Unlock()
	os.Exit(code)
}

// patience stretches a wait when the tests run in a slow browser: the
// launcher's own, on a CI runner without a graphics card, where it keeps
// the processors busy and answers in fits and starts (see CDP_BASE in
// cdptest).
func patience(d time.Duration) time.Duration {
	if os.Getenv("CDP_BASE") != "" {
		return 3 * d
	}
	return d
}

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
	var logMu sync.Mutex
	opt := Options{
		CheckoutBases: []string{store.URL()}, LoginBase: store.URL(),
		DiagDir:    r.diag,
		UIWait:     patience(20 * time.Second),
		SettleWait: 50 * time.Millisecond,
		LoginGrace: 2 * time.Second,
		// Short waits keep the negative cases quick.
		CheckoutWait: patience(12 * time.Second),
		OrderWait:    patience(14 * time.Second),
		Logf: func(f string, a ...any) {
			t.Logf(f, a...)
			logMu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(f, a...))
			logMu.Unlock()
		},
	}
	if mod != nil {
		mod(&opt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), patience(90*time.Second)+opt.BrowserCheckWait)
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

// whatWasSeen logs, for a scenario that went wrong, how often the checkout
// was asked for and what the program last saw of the page.
func whatWasSeen(t *testing.T, r run) {
	t.Helper()
	t.Logf("the checkout was requested %d time(s)", r.store.loads())
	found, _ := filepath.Glob(filepath.Join(r.diag, "*", "summary.json"))
	for _, f := range found {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc struct {
			Page struct {
				URL, Title, Ready, Text string
				BrowserCheck, App       bool
			} `json:"page"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			if len(doc.Page.Text) > 300 {
				doc.Page.Text = doc.Page.Text[:300]
			}
			t.Logf("last seen: %+v", doc.Page)
		}
	}
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
	// May become possible during the week, so it is looked at again later
	// rather than written off.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), MissingBase: true}, nil)
	wantOutcome(t, r, Later, 0)
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
	// The order goes to an address this program does not know as an order
	// address, so success is read from the page ("It's all yours").
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, GraphQLDown: true, SubmitPath: "/api/v9/acquire"}, nil)
	wantOutcome(t, r, Claimed, 1)
	if !strings.Contains(r.res.Detail, "confirmation") {
		t.Errorf("detail = %q", r.res.Detail)
	}
	// At its usual address, Epic's own reply settles it.
	r = runScenario(t, scenario{SignedIn: true, NewLayout: true, GraphQLDown: true}, nil)
	wantOutcome(t, r, Claimed, 1)
	if !strings.Contains(r.res.Detail, "confirmed by Epic") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestNewCheckoutLayoutInAnotherLanguage(t *testing.T) {
	// No English sentence and no known label: Epic's order preview gives
	// the order a total of zero, the only price on the page is zero, and
	// the page has a single button that is not a way out.
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, Real2026: true, German: true, NewPrice: "0,00 €", NoFreeText: true}, nil)
	wantOutcome(t, r, Claimed, 1)
	// A title that ends in a number, right before a price that starts with
	// one: two things, not one amount.
	r = runScenario(t, scenario{SignedIn: true, NewLayout: true, Real2026: true, German: true, NewTitle: "Dead Island 2", NewPrice: "0,00 €", NoFreeText: true}, nil)
	wantOutcome(t, r, Claimed, 1)
	// A zero price on the page alone, with nothing else saying that the
	// order is free, is not enough to press anything.
	r = runScenario(t, scenario{SignedIn: true, NewLayout: true, German: true, NewPrice: "0,00 €", NoFreeText: true}, nil)
	wantOutcome(t, r, Failed, 0)
	if n := r.store.hitCount("place"); n != 0 {
		t.Errorf("the button was pressed %d time(s)", n)
	}
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
	// It rehearses the approach (pointer onto the button, last look) but
	// presses nothing.
	if !strings.Contains(r.res.Detail, "last look before a press passed") {
		t.Errorf("detail = %q", r.res.Detail)
	}
	if n := r.store.hitCount("place"); n != 0 {
		t.Errorf("the dry run pressed the button %d time(s)", n)
	}
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
	if ok, msg := w.confirmation(); ok || msg != "You already own this item. (errors.x)" || !w.ownedHint() {
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

// ---- windows the launcher opens by itself, and the store-page route

// browserCall runs one browser-level DevTools command.
func browserCall(t *testing.T, base, method string, params any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v, err := cdp.Version(ctx, base)
	if err != nil {
		t.Errorf("browser version: %v", err)
		return
	}
	c, err := cdp.Dial(ctx, v.WebSocketDebuggerURL)
	if err != nil {
		t.Errorf("browser connection: %v", err)
		return
	}
	defer c.Close()
	if err := c.Call(ctx, method, params, nil); err != nil {
		t.Errorf("%s: %v", method, err)
	}
}

// needTabs skips a test that has the browser open tabs on request. The
// launcher's own browser (see CDP_BASE in cdptest) does not do that for an
// outsider.
func needTabs(t *testing.T) {
	t.Helper()
	if os.Getenv("CDP_BASE") != "" {
		t.Skip("needs a browser that opens tabs on request")
	}
}

func pageIDs(base string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := map[string]bool{}
	targets, err := cdp.Targets(ctx, base)
	if err != nil {
		// An empty answer would make callers close or keep the wrong tabs.
		panic("the test browser did not list its pages: " + err.Error())
	}
	for _, tg := range targets {
		if tg.Type == "page" {
			out[tg.ID] = true
		}
	}
	return out
}

// closeNewTabs removes the tabs a test opened, so later tests start from
// the single page again.
func closeNewTabs(t *testing.T, base string, before map[string]bool) {
	for id := range pageIDs(base) {
		if !before[id] {
			browserCall(t, base, "Target.closeTarget", map[string]any{"targetId": id})
		}
	}
}

func TestFollowsACheckoutWindowTheLauncherOpens(t *testing.T) {
	// The launcher answers a checkout address by opening its own checkout
	// window instead of loading it in place: here, the direct address shows
	// an error, and a new window with the working checkout appears.
	base := sharedBrowser(t)
	needTabs(t)
	before := pageIDs(base)
	defer closeNewTabs(t, base, before)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true})
	var once sync.Once
	store.sc.OnDirect = func(offers string) {
		once.Do(func() {
			go browserCall(t, base, "Target.createTarget", map[string]any{"url": store.URL() + "/purchase?offers=" + offers + "&via=store"})
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{store.URL()}, LoginBase: store.URL(), SettleWait: 50 * time.Millisecond, CheckoutWait: 20 * time.Second, OrderWait: 14 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Claimed {
		t.Fatalf("result = %v", res)
	}
	if store.orderCount() != 1 {
		t.Fatalf("orders = %d", store.orderCount())
	}
	// Afterwards the session is back on its first page, ready for the next offer.
	if s.page != s.home.page || len(s.tabs) != 1 {
		t.Fatalf("session did not return to its first page (%d tabs)", len(s.tabs))
	}
}

func storePageOptions(t *testing.T, base string, store *mockStore) Options {
	return Options{
		CheckoutBases: []string{store.URL()}, LoginBase: store.URL(),
		SettleWait: 50 * time.Millisecond, CheckoutWait: 20 * time.Second, OrderWait: 14 * time.Second,
		StorePageWait: 10 * time.Second,
		Logf:          t.Logf,
		OpenProduct: func(ctx context.Context, o feed.Offer, try int) error {
			if try == 0 {
				return errors.New("the launcher did not understand the first link form")
			}
			browserCall(t, base, "Target.createTarget", map[string]any{"url": store.URL() + "/store/product/" + o.Slug})
			return nil
		},
	}
}

func TestClaimsThroughTheStorePage(t *testing.T) {
	// Neither the direct checkout nor the sign-in hand-over works; the
	// launcher's own route does: store page (behind a "Continue" notice),
	// "Get", and a checkout window the launcher opens.
	base := sharedBrowser(t)
	needTabs(t)
	before := pageIDs(base)
	defer closeNewTabs(t, base, before)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true, ProductGate: true})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := Open(ctx, base, storePageOptions(t, base, store))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Claimed {
		t.Fatalf("result = %v", res)
	}
	if store.orderCount() != 1 {
		t.Fatalf("orders = %d", store.orderCount())
	}
}

func TestStorePagePaidOrderIsStillRefused(t *testing.T) {
	// The store-page route ends in the same checkout checks: a total above
	// zero is never ordered.
	base := sharedBrowser(t)
	needTabs(t)
	before := pageIDs(base)
	defer closeNewTabs(t, base, before)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$19.99", PreviewTotal: 1999, ConfirmTotal: intp(1999), DirectBlocked: true})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := Open(ctx, base, storePageOptions(t, base, store))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if res := s.Claim(ctx, testOffer()); res.Outcome != NotFree {
		t.Fatalf("result = %v", res)
	}
	if store.orderCount() != 0 {
		t.Fatalf("orders = %d", store.orderCount())
	}
}

func TestStorePageStates(t *testing.T) {
	base := sharedBrowser(t)
	cases := []struct {
		label  string
		want   Outcome
		detail string
	}{
		{"In Library", Owned, "in the library"},
		{"Buy Now", Failed, "Buy Now"},
		{"$19.99", Failed, "$19.99"},
		{"Requires Base Game", Later, "base game"},
		{"Coming Soon", Later, "right now"},
	}
	for _, c := range cases {
		needTabs(t)
		before := pageIDs(base)
		// "In Library" is only believed when the store confirms it for
		// this very offer.
		store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true, ProductLabel: c.label, Owned: c.want == Owned})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		s, err := Open(ctx, base, storePageOptions(t, base, store))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		res := s.Claim(ctx, testOffer())
		s.Close()
		cancel()
		closeNewTabs(t, base, before)
		if res.Outcome != c.want || !strings.Contains(res.Detail, c.detail) {
			t.Errorf("button %q: result = %v, want %v mentioning %q", c.label, res, c.want, c.detail)
		}
		if store.orderCount() != 0 {
			t.Errorf("button %q: orders = %d", c.label, store.orderCount())
		}
	}
}

func TestIsCheckoutURL(t *testing.T) {
	bases := []string{"https://launcher.store.epicgames.com", "https://www.epicgames.com/store", "https://store.epicgames.com", "http://127.0.0.1:5555"}
	for u, want := range map[string]bool{
		"https://launcher.store.epicgames.com/purchase?offers=1-a-b": true,
		"https://store.epicgames.com/checkout?purchaseToken=x":       true,
		"https://www.epicgames.com/store/purchase?offers=1-a-b":      true,
		"https://payment-website-pci.ol.epicgames.com/purchase?x=1":  true,
		"http://127.0.0.1:5555/purchase?offers=1-a-b":                true,
		"https://launcher.store.epicgames.com/store":                 false,
		"https://store.epicgames.com/en-US/p/flotsam":                false,
		"https://evil.example/purchase?offers=1-a-b":                 false,
		"http://127.0.0.1:6666/purchase":                             false,
		"about:blank":                                                false,
	} {
		if got := isCheckoutURL(u, bases); got != want {
			t.Errorf("isCheckoutURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// ---- Epic's browser check ("One more step")

func TestBrowserCheckIsWaitedOut(t *testing.T) {
	// The check passes by itself after a few seconds; nothing is clicked
	// meanwhile, and the claim then goes ahead.
	if os.Getenv("CDP_BASE") != "" {
		// The launcher hands a page's own reloads to the system's browser
		// when the address is not one of Epic's (the stand-in store's is
		// not), so the stand-in check never "passes" in its own browser:
		// the checkout was asked for two or three times in ninety seconds,
		// and a Microsoft Edge was left running on the test machine.
		t.Skip("the launcher's browser does not follow a reload to an address that is not Epic's")
	}
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), ChallengeFor: 4 * time.Second}, func(o *Options) {
		o.CheckoutWait = 3 * time.Second // shorter than the check lasts
		o.BrowserCheckWait = patience(30 * time.Second)
	})
	wantOutcome(t, r, Claimed, 1)
	if r.res.Outcome != Claimed {
		whatWasSeen(t, r)
	}
}

func TestBrowserCheckThatNeverPassesBacksOff(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), ChallengeFor: time.Hour}, func(o *Options) {
		o.CheckoutWait = 2 * time.Second
		o.BrowserCheckWait = 4 * time.Second
	})
	wantOutcome(t, r, Captcha, 0)
	if !strings.Contains(r.res.Detail, "browser check") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestSaysFreeDoesNotOutweighAPrice(t *testing.T) {
	// The page carries Epic's "This is free" sentence, yet also shows an
	// amount above zero: that is not proof of a zero total.
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, NewPrice: "$4.99"}, nil)
	wantOutcome(t, r, Failed, 0)
}

func TestCoveredButtonIsNotClicked(t *testing.T) {
	// Something lies on top of the order button: a mouse click would land
	// on the cover, so none is made.
	base := sharedBrowser(t)
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hit" {
			mu.Lock()
			hits = append(hits, r.URL.Query().Get("what"))
			mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>x</title><body style="margin:0"><div id="purchase-app">
<div class="payment-price payment-price--total">Total $0.00</div>
<button class="payment-btn payment-order-confirm__btn" style="position:absolute;left:100px;top:100px;width:200px;height:40px" onclick="fetch('/hit?what=button')">Place Order</button>
<div id="cover" style="position:absolute;left:0;top:0;width:600px;height:400px;background:#000;opacity:0.9" onclick="fetch('/hit?what=cover')"></div>
</div></body>`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{srv.URL}, LoginBase: srv.URL, SettleWait: 50 * time.Millisecond, CheckoutWait: 4 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Failed {
		t.Fatalf("result = %v", res)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 0 {
		t.Fatalf("something was clicked: %v", hits)
	}
}

// ---- regression tests for the money-safety review

func TestLeftoverPageCannotOrderAfterTheClaimEnds(t *testing.T) {
	// The order is refused at the network (it states a price). The page
	// then keeps trying by itself. Nothing may get out once the claim has
	// returned and nobody is looking any more.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), RetryOnError: true}, nil)
	wantOutcome(t, r, NotFree, 0)
	time.Sleep(4 * time.Second)
	if n := r.store.orderCount(); n != 0 {
		t.Fatalf("%d order(s) reached the store after the claim had ended: %s", n, r.store.lastOrder())
	}
}

func TestRequestsWithoutAClaimInProgressCannotOrder(t *testing.T) {
	// The same rule, at its root: with no claim in progress, a supervised
	// page cannot get an order out, whatever the order says.
	base := sharedBrowser(t)
	store := newMockStore(t, scenario{SignedIn: true})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, base, Options{CheckoutBases: []string{store.URL()}, LoginBase: store.URL(), SettleWait: 50 * time.Millisecond, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.page.Navigate(ctx, store.URL()+"/"); err != nil {
		t.Fatal(err)
	}
	var out string
	js := `(async function () {
  var res = [];
  var tries = [['/purchase/confirm-order', '{"totalAmount":0}'], ['/checkout/api/complete', '{}'], ['/api/v9/acquire', '{"totalPrice":499}']];
  for (var i = 0; i < tries.length; i++) {
    try { await fetch(tries[i][0], { method: 'POST', headers: { 'content-type': 'application/json' }, body: tries[i][1] }); res.push('sent'); }
    catch (e) { res.push('blocked'); }
  }
  return res.join(',');
})()`
	if err := s.page.Eval(ctx, js, &out); err != nil {
		t.Fatal(err)
	}
	if out != "blocked,blocked,blocked" || store.orderCount() != 0 {
		t.Fatalf("requests = %s, orders at the store = %d", out, store.orderCount())
	}
}

func TestOnlyTheCheckoutsOwnTotalCounts(t *testing.T) {
	cases := []struct {
		name string
		sc   scenario
	}{
		// A cart badge in the page header, which happens to carry "total" in
		// its class name and shows a bare zero.
		{"header badge", scenario{SignedIn: true, NewLayout: true, NewPrice: "$19.99", NoFreeText: true,
			ExtraHTML: `<span class="nav-cart-total">0</span>`}},
		// A zero under a key named "total" in the data the page was built from.
		{"page data", scenario{SignedIn: true, NewLayout: true, NewPrice: "$19.99", NoFreeText: true,
			ExtraHTML: `<script>window.__epic_web_purchase_dataPreload = { wishlist: { total: 0 }, cart: { totalPrice: 0 } };</script>`}},
		// A currency this program has no list entry for, next to Epic's
		// "This is free" sentence.
		{"prefixed dollar", scenario{SignedIn: true, NewLayout: true, NewPrice: "CA$19.99"}},
		{"unlisted currency", scenario{SignedIn: true, NewLayout: true, NewPrice: "19.99 XYZ"}},
		// "Free shipping" somewhere on a page that charges.
		{"free word elsewhere", scenario{SignedIn: true, NewLayout: true, NewPrice: "$19.99", NoFreeText: true,
			ExtraHTML: `<div class="order-total-note">Free returns</div><div class="grand-total">$19.99</div>`}},
	}
	for _, c := range cases {
		r := runScenario(t, c.sc, func(o *Options) { o.CheckoutWait = 5 * time.Second })
		if r.res.Outcome == Claimed || r.res.Outcome == Owned || r.res.Outcome == DryRun {
			t.Errorf("%s: outcome = %v", c.name, r.res)
		}
		time.Sleep(150 * time.Millisecond)
		if n := r.store.orderCount(); n != 0 {
			t.Errorf("%s: %d order(s) reached the store", c.name, n)
		}
	}
}

func TestSubtotalNextToAZeroTotalIsStillClaimed(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0),
		ExtraHTML: `<div class="payment-price payment-price--subtotal"><span>Subtotal</span> <span>$24.99</span></div>`}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestWordsInsideScriptsAreNotRead(t *testing.T) {
	// The page's scripts carry every message the checkout could ever show.
	// Only what is on screen counts: this offer is neither owned nor ordered
	// until the page says so.
	extra := `<script>var MESSAGES = { owned: "You already own this item.", thanks: "Thanks for your order!", region: "This product is unavailable in your region.", busy: "Please wait 24 hours before trying to redeem a free game again." };</script>`
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), GraphQLDown: true, ExtraHTML: extra}, nil)
	wantOutcome(t, r, Claimed, 1)
	r = runScenario(t, scenario{SignedIn: true, NewLayout: true, GraphQLDown: true, ExtraHTML: extra}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestHiddenPinPromptDoesNotBlock(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0),
		ExtraHTML: `<div class="payment-pin-code" style="display:none"><input class="payment-pin-code__input"></div>`}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestPaidEvidenceAfterThePressStopsTheOrder(t *testing.T) {
	const confirm = "https://payment-website-pci.ol.epicgames.com/purchase/confirm-order"
	const preview = "https://payment-website-pci.ol.epicgames.com/purchase/order-preview"
	// The page showed zero and the button was pressed; then a re-priced
	// preview arrives. No order may follow, whatever it says of itself.
	for _, body := range []string{`{"offers":["x"]}`, `{"totalAmount":0}`} {
		w := newOrderWatch(false)
		w.setPreClick(Free)
		w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":1999}}`))
		if w.onRequest("POST", confirm, body, true) || w.submitted() || w.blockedReason() == "" {
			t.Errorf("order %s was let out after a paid preview", body)
		}
	}
	// A zero preview does not undo a paid one.
	w := newOrderWatch(false)
	w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":1999}}`))
	w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":0}}`))
	if w.onRequest("POST", confirm, `{"totalAmount":0}`, true) {
		t.Error("a later zero preview must not clear an earlier paid one")
	}
	// An order whose content the browser will not show is not sent.
	w = newOrderWatch(false)
	w.setPreClick(Free)
	if w.onRequest("POST", confirm, "", true) {
		t.Error("an order that cannot be inspected must be refused")
	}
	// Order addresses are recognised by their path, in the forms seen so far.
	for u, want := range map[string]bool{
		"https://x/checkout/api/complete":  true,
		"https://x/purchase/api/confirm":   true,
		"https://x/checkout/submit":        true,
		"https://x/purchase/finalize":      true,
		"https://x/checkout/api/completed": false,
		"https://x/purchase":               false,
	} {
		if got := isOrderSubmit("POST", u); got != want {
			t.Errorf("isOrderSubmit(%q) = %v, want %v", u, got, want)
		}
	}
	if !isOrderSubmit("PATCH", "https://x/v1/orders") || !isOrderSubmit("PUT", "https://x/purchase/confirm-order") {
		t.Error("PUT and PATCH can place orders too")
	}
	// With no claim in progress, nothing that could order or that states a
	// price is let out.
	if !mayPlaceOrder("POST", confirm, `{"totalAmount":0}`) || !mayPlaceOrder("POST", "https://x/anything", `{"totalPrice":499}`) {
		t.Error("orders and priced requests must be held back when no claim is in progress")
	}
	if mayPlaceOrder("GET", confirm, "") || mayPlaceOrder("POST", "https://x/telemetry", `{"event":"view"}`) {
		t.Error("ordinary requests must pass")
	}
}

func TestPriceEvidenceRules(t *testing.T) {
	free := func(st pageState, w *orderWatch) Verdict { v, _ := priceEvidence(st, w); return v }
	if v := free(pageState{Totals: []string{"Total $0.00"}, Prices: []string{"$24.99", "-$24.99", "$0.00"}}, newOrderWatch(false)); v != Free {
		t.Errorf("zero total line: %v", v)
	}
	if v := free(pageState{Prices: []string{"$0.00"}, SaysFree: true}, newOrderWatch(false)); v != Free {
		t.Errorf("2026 layout: %v", v)
	}
	// Page data cannot prove a zero total...
	if v := free(pageState{Preload: `{"wishlist":{"total":0}}`, Prices: []string{"$19.99"}}, newOrderWatch(false)); v == Free {
		t.Errorf("page data alone: %v", v)
	}
	if v := free(pageState{Preload: `{"order":{"totalPrice":0}}`}, newOrderWatch(false)); v == Free {
		t.Errorf("page data with no amount on the page: %v", v)
	}
	// ...but it can speak against one.
	if v := free(pageState{Totals: []string{"Total $0.00"}, Prices: []string{"$0.00"}, Preload: `{"order":{"totalPrice":1999}}`}, newOrderWatch(false)); v != Paid {
		t.Errorf("paid page data: %v", v)
	}
	// "This is free" next to amounts none of which is zero proves nothing.
	if v := free(pageState{Prices: []string{"CA$19.99"}, SaysFree: true}, newOrderWatch(false)); v == Free {
		t.Errorf("free sentence next to a price: %v", v)
	}
	if v := free(pageState{SaysFree: true}, newOrderWatch(false)); v == Free {
		t.Errorf("free sentence with no amount: %v", v)
	}
	// A zero preview does not make a page that shows only a price free.
	w := newOrderWatch(false)
	w.onResponse("POST", "https://x/purchase/order-preview", 200, []byte(`{"orderResponse":{"totalPrice":0}}`))
	if v := free(pageState{Prices: []string{"$19.99"}}, w); v == Free {
		t.Errorf("zero preview against a page showing only a price: %v", v)
	}
	// Anything paid on the network wins.
	w = newOrderWatch(false)
	w.onResponse("POST", "https://x/purchase/order-preview", 200, []byte(`{"orderResponse":{"totalPrice":499}}`))
	if v := free(pageState{Totals: []string{"Total $0.00"}, Prices: []string{"$0.00"}, SaysFree: true}, w); v != Paid {
		t.Errorf("paid preview: %v", v)
	}
}

func TestServiceWorkerCannotCarryAnOrderOut(t *testing.T) {
	// The page's requests are relayed by a service worker. An order that
	// states a price must still not reach the store.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), ServiceWorker: true}, nil)
	if r.res.Outcome != NotFree && r.res.Outcome != Failed {
		t.Errorf("outcome = %v", r.res)
	}
	time.Sleep(500 * time.Millisecond)
	if n := r.store.orderCount(); n != 0 {
		t.Fatalf("%d order(s) reached the store through the service worker", n)
	}
}

func TestFrameFromAnotherSiteCannotCarryAnOrderOut(t *testing.T) {
	// The order is sent by a frame from another site, which runs in a
	// process of its own.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), FrameOrder: true}, nil)
	if r.res.Outcome != NotFree && r.res.Outcome != Failed {
		t.Errorf("outcome = %v", r.res)
	}
	time.Sleep(500 * time.Millisecond)
	if n := r.store.orderCount(); n != 0 {
		t.Fatalf("%d order(s) reached the store through the frame", n)
	}
	// A free order sent the same way goes through.
	r = runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), FrameOrder: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestStorePageLibraryTileIsNotTheOffersState(t *testing.T) {
	// The page offers "Get" for this game and, further down, shows another
	// edition that is "In Library". The offer is not owned.
	base := sharedBrowser(t)
	needTabs(t)
	before := pageIDs(base)
	defer closeNewTabs(t, base, before)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true,
		ProductExtra: `<section><h2>Editions</h2><div class="edition"><span>Deluxe Edition</span> <button style="width:160px;height:40px" disabled>In Library</button></div><p>You already own this item.</p></section>`})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := Open(ctx, base, storePageOptions(t, base, store))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Claimed {
		t.Fatalf("result = %v", res)
	}
	if store.orderCount() != 1 {
		t.Fatalf("orders = %d", store.orderCount())
	}
}

func TestCheckoutWindowForAnotherOfferIsNotUsed(t *testing.T) {
	// "Get" opens a checkout for something other than this giveaway (a
	// bundle or an edition the page promotes). It is left alone.
	base := sharedBrowser(t)
	needTabs(t)
	before := pageIDs(base)
	defer closeNewTabs(t, base, before)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true, ProductOpens: "1-ns-other"})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := Open(ctx, base, storePageOptions(t, base, store))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome == Claimed || res.Outcome == Owned {
		t.Fatalf("result = %v", res)
	}
	time.Sleep(300 * time.Millisecond)
	if store.orderCount() != 0 {
		t.Fatalf("orders = %d", store.orderCount())
	}
}

// ---- regression tests for the session-lifecycle review

func TestNoClickOnceTheClaimIsCalledOff(t *testing.T) {
	// The user starts the launcher (or the program is told to stop) at the
	// very moment the checkout has been found ready. Nothing is pressed,
	// by mouse or by script, and the attempt is not counted as a failure
	// of the checkout.
	for _, sc := range []scenario{
		{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)},
		{SignedIn: true, NewLayout: true},
	} {
		base := sharedBrowser(t)
		store := newMockStore(t, sc)
		ctx, cancel := context.WithCancel(context.Background())
		opt := Options{CheckoutBases: []string{store.URL()}, LoginBase: store.URL(), SettleWait: 50 * time.Millisecond, CheckoutWait: 12 * time.Second, OrderWait: 14 * time.Second}
		opt.Logf = func(f string, a ...any) {
			t.Logf(f, a...)
			if strings.Contains(f, "checkout ready") {
				cancel()
			}
		}
		s, err := Open(ctx, base, opt)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		res := s.Claim(ctx, testOffer())
		s.Close()
		cancel()
		time.Sleep(300 * time.Millisecond)
		if res.Outcome != Failed || res.Detail != "interrupted" {
			t.Errorf("result = %v", res)
		}
		if n := store.hitCount("place"); n != 0 {
			t.Errorf("the order button was pressed %d time(s) after the claim was called off", n)
		}
		if n := store.orderCount(); n != 0 {
			t.Errorf("%d order(s) reached the store", n)
		}
	}
}

func TestLeavePagePromptDoesNotHoldTheSessionUp(t *testing.T) {
	// A checkout that asks "Leave this page?" must not stall the clean-up
	// after a claim (or the next offer's checkout).
	started := time.Now()
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0),
		ExtraHTML: `<script>window.addEventListener('beforeunload', function (e) { e.preventDefault(); e.returnValue = 'Leave?'; });</script>`}, nil)
	wantOutcome(t, r, Claimed, 1)
	if took := time.Since(started); took > patience(9*time.Second) {
		t.Errorf("the session took %v; the leave-page prompt held it up", took.Round(time.Second))
	}
}

func TestOrderButtonIsPressedOnce(t *testing.T) {
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, Claimed, 1)
	if n := r.store.hitCount("place"); n != 1 {
		t.Errorf("the order button was pressed %d times", n)
	}
}

func TestStorePageWithItsCheckoutInsideThePage(t *testing.T) {
	// The web store's way: "Get" shows the checkout in a frame inside the
	// store page. Its address is opened as a page of its own and handled
	// like any other checkout.
	base := sharedBrowser(t)
	needTabs(t)
	before := pageIDs(base)
	defer closeNewTabs(t, base, before)
	store := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true, ProductInPage: true})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := Open(ctx, base, storePageOptions(t, base, store))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := s.Claim(ctx, testOffer())
	if res.Outcome != Claimed {
		t.Fatalf("result = %v", res)
	}
	if n := store.orderCount(); n != 1 {
		t.Fatalf("orders = %d", n)
	}
	if n := store.hitCount("place"); n != 1 {
		t.Fatalf("the order button was pressed %d times", n)
	}
	s.Close() // (each part with a session of its own, one at a time)

	// The same with a price: nothing is ordered.
	before2 := pageIDs(base)
	defer closeNewTabs(t, base, before2)
	paid := newMockStore(t, scenario{SignedIn: true, DomTotal: "$19.99", PreviewTotal: 1999, ConfirmTotal: intp(1999), DirectBlocked: true, ProductInPage: true})
	s2, err := Open(ctx, base, storePageOptions(t, base, paid))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if res := s2.Claim(ctx, testOffer()); res.Outcome != NotFree {
		t.Fatalf("paid: result = %v", res)
	}
	if n := paid.orderCount(); n != 0 {
		t.Fatalf("paid: orders = %d", n)
	}
	s2.Close()

	// A frame that is not this offer's checkout is left alone.
	before3 := pageIDs(base)
	defer closeNewTabs(t, base, before3)
	other := newMockStore(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), DirectBlocked: true, ProductInPage: true, ProductOpens: "1-ns-other"})
	s3, err := Open(ctx, base, storePageOptions(t, base, other))
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if res := s3.Claim(ctx, testOffer()); res.Outcome != Failed || !strings.Contains(res.Detail, "not recognisably") {
		t.Fatalf("other offer: result = %v", res)
	}
	if n := other.orderCount(); n != 0 {
		t.Fatalf("other offer: orders = %d", n)
	}
}

// ---- what Epic's real checkout was seen to do on a signed-in launcher (November 2026)

func TestRealFreeCheckoutFlowOf2026(t *testing.T) {
	// The page: "Checkout", the game, "$0.00", "This is free. Add it to
	// your library to get started.", one button "Add to library". Behind
	// it: an order preview and the order itself under /v2/purchase/.
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, Real2026: true, GraphQLDown: true}, nil)
	wantOutcome(t, r, Claimed, 1)
	if !strings.Contains(r.res.Detail, "confirmed by Epic") {
		t.Errorf("detail = %q", r.res.Detail)
	}
	if !strings.Contains(r.store.lastOrder(), `"totalAmount":0`) {
		t.Errorf("order = %s", r.store.lastOrder())
	}
}

func TestPurchaseLimitAnswerMeansAlreadyOwned(t *testing.T) {
	// A free-to-play game the account has already: the checkout looks like
	// any other, and only the order itself is answered with HTTP 404 and
	// "purchase_limit_error" (the page then says "Sorry, you already own
	// this item, or it's still being processed."). That settles it: it is
	// not a failure to be tried again.
	r := runScenario(t, scenario{SignedIn: true, NewLayout: true, Real2026: true, GraphQLDown: true,
		ConfirmStatus: 404,
		ConfirmRaw:    `{"errorCode":"errors.com.epicgames.ecommerce.orderprocessor.purchase_limit_error","errorMessage":"Sorry, you already own this item, or it's still being processed.","numericErrorCode":20203,"correlationId":"x"}`}, nil)
	wantOutcome(t, r, Owned, 1)
	if !strings.Contains(r.res.Detail, "already") {
		t.Errorf("detail = %q", r.res.Detail)
	}
}

func TestGoingThroughTheCheckoutAgainForAGameAlreadyOwned(t *testing.T) {
	// The whole path, proved on a game the account already has: Epic's
	// checkout looks like any other for it, and the order is answered with
	// "purchase_limit_error". Nothing changes on the account.
	owned := scenario{SignedIn: true, NewLayout: true, Real2026: true, Owned: true,
		ConfirmStatus: 404,
		ConfirmRaw:    `{"errorCode":"errors.com.epicgames.ecommerce.orderprocessor.purchase_limit_error","errorMessage":"Sorry, you already own this item, or it's still being processed.","numericErrorCode":20203,"correlationId":"x"}`}

	// Ordinarily it stops at "already in the library" and presses nothing.
	r := runScenario(t, owned, nil)
	wantOutcome(t, r, Owned, 0)
	if n := r.store.hitCount("place"); n != 0 {
		t.Fatalf("pressed %d time(s) for a game already owned", n)
	}

	// A rehearsal that goes on regardless: price check, pointer onto the
	// button, last look; nothing pressed.
	r = runScenario(t, owned, func(o *Options) { o.Again, o.DryRun = true, true })
	wantOutcome(t, r, DryRun, 0)
	if !strings.Contains(r.res.Detail, "last look before a press passed") || !strings.Contains(r.res.Detail, "already in the library") {
		t.Errorf("detail = %q", r.res.Detail)
	}
	if n := r.store.hitCount("place"); n != 0 {
		t.Fatalf("the rehearsal pressed %d time(s)", n)
	}

	// For real: the button is pressed once, the order goes out, Epic says
	// the account has it. That is "owned", never "claimed" (the library
	// listed the game before the press, so it proves nothing here).
	r = runScenario(t, owned, func(o *Options) { o.Again = true })
	wantOutcome(t, r, Owned, 1)
	if n := r.store.hitCount("place"); n != 1 {
		t.Fatalf("pressed %d time(s), want 1", n)
	}
	if !strings.Contains(r.res.Detail, "already") {
		t.Errorf("detail = %q", r.res.Detail)
	}
	logged := false
	for _, l := range r.logs {
		logged = logged || strings.Contains(l, "Epic is expected to refuse the order")
	}
	if !logged {
		t.Error("the log does not say why a button was pressed for a game already owned")
	}

	// A price is a price, owned or not: nothing is pressed.
	paid := owned
	paid.NewPrice, paid.NoFreeText, paid.PreviewTotal = "$19.99", true, 1999
	r = runScenario(t, paid, func(o *Options) { o.Again = true })
	wantOutcome(t, r, NotFree, 0)
	if n := r.store.hitCount("place"); n != 0 {
		t.Fatalf("pressed %d time(s) although the checkout showed a price", n)
	}

	// And if Epic takes the order after all (the library was wrong), that
	// is a claim like any other.
	accepted := owned
	accepted.ConfirmStatus, accepted.ConfirmRaw = 0, ""
	r = runScenario(t, accepted, func(o *Options) { o.Again = true })
	wantOutcome(t, r, Claimed, 1)

	// The price rules are what they always are, owned or not, again or
	// not. A total on the page: nothing is pressed.
	limit := owned.ConfirmRaw
	r = runScenario(t, scenario{SignedIn: true, Owned: true, DomTotal: "$19.99", NoPreview: true, ConfirmTotal: intp(1999)}, func(o *Options) { o.Again = true })
	wantOutcome(t, r, NotFree, 0)
	if n := r.store.hitCount("place"); n != 0 {
		t.Fatalf("pressed %d time(s) although the page showed a total", n)
	}
	logged = false
	for _, l := range r.logs {
		logged = logged || strings.Contains(l, "going on all the same")
	}
	if !logged {
		t.Error("this case was meant to get past the ownership question and stop at the price")
	}
	// A total in the order itself: it is stopped at the network.
	r = runScenario(t, scenario{SignedIn: true, Owned: true, DomTotal: "$0.00", ConfirmTotal: intp(1999)}, func(o *Options) { o.Again = true })
	wantRefusedAtTheNetwork(t, r)

	// The checkout itself says "you already own this" next to its order
	// button. Without Again that ends it; with Again the button is pressed
	// and it is Epic's answer to the order that is reported, not the words
	// that were on the page all along.
	ownedText := scenario{SignedIn: true, Owned: true, OwnedMessage: true, DomTotal: "$0.00", ConfirmTotal: intp(0), SlowMs: 1500, ConfirmStatus: 404, ConfirmRaw: limit}
	r = runScenario(t, ownedText, nil)
	wantOutcome(t, r, Owned, 0)
	r = runScenario(t, ownedText, func(o *Options) { o.Again = true })
	wantOutcome(t, r, Owned, 1)
	if !strings.Contains(r.res.Detail, "Epic says the account has this already") || r.store.hitCount("place") != 1 {
		t.Errorf("owned text on the page: %v, %d press(es)", r.res, r.store.hitCount("place"))
	}
	// ...and if Epic takes that order, it is a claim, whatever the page
	// said before.
	ownedText.ConfirmStatus, ownedText.ConfirmRaw = 0, ""
	r = runScenario(t, ownedText, func(o *Options) { o.Again = true })
	wantOutcome(t, r, Claimed, 1)
	// A run that ends before a press says so.
	r = runScenario(t, scenario{SignedIn: true, Owned: true, OwnedMessage: true, DomTotal: "$0.00", ConfirmTotal: intp(0), SlowMs: 60000}, func(o *Options) { o.Again = true; o.CheckoutWait = 4 * time.Second })
	if r.res.Outcome == Claimed || r.store.hitCount("place") != 0 {
		t.Errorf("button never ready: %v, %d press(es)", r.res, r.store.hitCount("place"))
	}
}

func TestTheLibraryOnlyConfirmsAClaimIfItDidNotListTheGameBefore(t *testing.T) {
	// The question "does the account have this?" got no answer before the
	// press (the store's server hiccuped). The account did have it. After
	// the press the library says "yes", as it would have all along: that
	// is not a claim. Epic's answer to the order decides.
	limit := `{"errorCode":"errors.com.epicgames.ecommerce.orderprocessor.purchase_limit_error","errorMessage":"Sorry, you already own this item, or it's still being processed.","numericErrorCode":20203,"correlationId":"x"}`
	sc := scenario{SignedIn: true, NewLayout: true, Real2026: true, Owned: true, OwnershipFails: 1, ConfirmStatus: 404, ConfirmRaw: limit}
	for _, again := range []bool{false, true} {
		r := runScenario(t, sc, func(o *Options) { o.Again = again })
		wantOutcome(t, r, Owned, 1)
		if !strings.Contains(r.res.Detail, "already") {
			t.Errorf("again=%v: %v", again, r.res)
		}
	}
	// With the answer "no" before the press, the library's "yes" after it
	// is a claim, as before.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0)}, nil)
	wantOutcome(t, r, Claimed, 1)
}

// ---- what runs next to the checkout is supervised from its first moment

// wantRefusedAtTheNetwork: the button was pressed once, the order that
// followed stated a price and was stopped before it left the browser.
func wantRefusedAtTheNetwork(t *testing.T, r run) {
	t.Helper()
	time.Sleep(700 * time.Millisecond)
	if n := r.store.orderCount(); n != 0 {
		t.Fatalf("%d order(s) reached the store: %s", n, r.store.lastOrder())
	}
	if r.res.Outcome != NotFree || !strings.Contains(r.res.Detail, "refused to send") {
		t.Errorf("outcome = %v, want the order refused at the network", r.res)
	}
	if n := r.store.hitCount("place"); n != 1 {
		t.Errorf("the order button was pressed %d time(s), want 1", n)
	}
}

func TestFrameCreatedAtTheClickCannotCarryAnOrderOut(t *testing.T) {
	// The frame does not exist until the button is pressed, belongs to
	// another site (a process of its own), and orders the moment it loads.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), LateFrame: true}, nil)
	wantRefusedAtTheNetwork(t, r)
	// A free order sent the same way goes through.
	r = runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), LateFrame: true}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestWorkersCannotCarryAnOrderOut(t *testing.T) {
	// A worker of the page's own, started at the click; and a worker
	// shared between pages, running since the page loaded, that is told to
	// order at the click.
	for _, kind := range []string{"dedicated", "shared"} {
		r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), ClickWorker: kind}, nil)
		t.Logf("%s worker: %v", kind, r.res)
		wantRefusedAtTheNetwork(t, r)
	}
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(0), ClickWorker: "dedicated"}, nil)
	wantOutcome(t, r, Claimed, 1)
}

func TestServiceWorkerToldToOrderCannotCarryItOut(t *testing.T) {
	// A service worker that does not control the page (so the page's own
	// requests do not pass through it) sends the order itself.
	r := runScenario(t, scenario{SignedIn: true, DomTotal: "$0.00", ConfirmTotal: intp(1999), SWOrder: true}, nil)
	wantRefusedAtTheNetwork(t, r)
}

func TestOrderWatchIsStrictAboutMethodsAndBodies(t *testing.T) {
	const confirm = "https://payment-website-pci.ol.epicgames.com/v2/purchase/confirm-order"
	const preview = "https://payment-website-pci.ol.epicgames.com/v2/purchase/order-preview"

	// Every way of writing a method that can change something counts.
	for _, m := range []string{"post", "Post", "PUT", "PATCH", "DELETE", "put"} {
		w := newOrderWatch(false)
		w.setPreClick(Free)
		if w.onRequest(m, confirm, `{"totalAmount":499}`, true) || !w.sawPaid() {
			t.Errorf("%s with a price must be refused", m)
		}
		w = newOrderWatch(false)
		if w.onRequest(m, confirm, `{"offers":["x"]}`, true) {
			t.Errorf("%s order without evidence must be refused", m)
		}
	}

	// However the price is written.
	for _, body := range []string{
		`{"total_amount":1999}`, `{"total-price":"19.99"}`, `{"TotalAmount":"$19.99"}`,
		`{"total":{"amount":1999,"currency":"USD"}}`, `{"order":{"grandTotal":{"value":"19,99 €"}}}`,
		`{"payload":"{\"totalAmount\":1999}"}`,
		`totalAmount=1999&offers=x`, `offers=x&order=%7B%22totalPrice%22%3A1999%7D`,
		"--b\r\nContent-Disposition: form-data; name=\"totalAmount\"\r\n\r\n1999\r\n--b--\r\n",
		`{"totalAmount":"19,99 zł"}`, `{"totalAmount":"19,99 Kč"}`, `{"totalAmount":"1 999 руб."}`, `{"totalAmount":"S/ 19.90"}`,
		`{"totalAmount":"1999円"}`, `{"totalAmount":"1,999원"}`, `{"totalAmount":"US$19.99"}`, `{"totalAmount":"R$ 19,99"}`,
		`order[totalAmount]=19.99`, `order.totalAmount=19.99&x=1`, "totalAmount=19.99&name=\x00",
		`eyJ0b3RhbEFtb3VudCI6MTkuOTl9Cg==`, "{\"a\":1}\n{\"totalAmount\":19.99}", "\ufeff{\"totalAmount\":19.99}",
		`{"total":{"amount":0,"items":[{"totalPrice":1999}]}}`,
	} {
		w := newOrderWatch(false)
		w.setPreClick(Free)
		if w.onRequest("POST", confirm, body, true) || !w.sawPaid() {
			t.Errorf("order %q must be refused as paid", body)
		}
	}
	// What is not an amount is not read as one.
	for _, body := range []string{`{"total":"3 items"}`, `{"totals":[1,2]}`, `{"subtotal":1999,"totalAmount":0}`, `totalAmount=0&offers=x`} {
		w := newOrderWatch(false)
		w.setPreClick(Free)
		if !w.onRequest("POST", confirm, body, true) {
			t.Errorf("order %q must pass", body)
		}
	}
	// An order whose content cannot be read is not sent.
	for _, body := range []string{`<order total="19.99"/>`, "\x1f\x8b\x08\x00binary=1", `a=1&<b c="d">=2`} {
		w := newOrderWatch(false)
		w.setPreClick(Free)
		if w.onRequest("POST", confirm, body, true) || !strings.Contains(w.blockedReason(), "could not be inspected") {
			t.Errorf("order %q: %q", body, w.blockedReason())
		}
	}
	w := newOrderWatch(false)
	w.setPreClick(Free)
	if w.onRequest("POST", confirm, "\x00\x01\x02binary", true) || !strings.Contains(w.blockedReason(), "could not be inspected") {
		t.Errorf("unreadable order: %q", w.blockedReason())
	}
	w = newOrderWatch(false)
	w.setPreClick(Free)
	if w.onRequest("POST", confirm, "", true) {
		t.Error("an order whose content the browser does not show must be refused")
	}

	// Once anything has spoken of a price, nothing that could change
	// anything is let out, wherever it is going and whatever it says.
	w = newOrderWatch(false)
	w.setPreClick(Free)
	w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":1999}}`))
	for _, u := range []string{confirm, "https://x.example/api/finish", "https://x.example/anything"} {
		if w.onRequest("POST", u, `{"totalAmount":0}`, true) {
			t.Errorf("after a paid preview, POST %s must be refused", u)
		}
	}
	if !w.onRequest("GET", "https://x.example/anything", "", false) {
		t.Error("reads still pass")
	}

	// After the press, whatever goes where orders are taken is an order.
	const finish = "https://payment-website-pci.ol.epicgames.com/v3/purchase/finish"
	w = newOrderWatch(false)
	if !w.onRequest("POST", finish, `{"x":1}`, true) || w.submitted() {
		t.Error("before the press, an unknown address is not taken for an order")
	}
	w.markPressed(Free)
	if !w.onRequest("POST", finish, `{"x":1}`, true) {
		t.Error("after the press, a readable request to a payment address that states no price passes")
	}
	if w.onRequest("POST", finish, "", true) || !strings.Contains(w.blockedReason(), "could not be inspected") {
		t.Error("after the press, a request to a payment address whose content the browser does not show is refused")
	}
	w = newOrderWatch(false)
	w.markPressed(Free)
	if w.onRequest("POST", finish, `{"amountDue":"19,99 zł"}`, true) || !w.sawPaid() {
		t.Error("after the press, a request to a payment address that states a price is refused")
	}
	for _, u := range []string{preview, "https://payment-website-pci.ol.epicgames.com/v2/purchase/get-earn-reward", "https://tracking.epicgames.com/track", "https://hcaptcha.com/getcaptcha/abc"} {
		w = newOrderWatch(false)
		w.markPressed(Free)
		if !w.onRequest("POST", u, `{"x":1}`, true) || w.submitted() {
			t.Errorf("POST %s after the press is not an order", u)
		}
	}
	w = newOrderWatch(true)
	w.markPressed(Free)
	if w.onRequest("POST", finish, `{"x":1}`, true) {
		t.Error("a dry run sends nothing that could be an order")
	}
	// With no claim in progress nothing of the kind gets out at all.
	for _, c := range []struct{ m, u, body string }{
		{"POST", finish, `{}`}, {"put", confirm, `{"totalAmount":0}`}, {"DELETE", "https://x/v1/orders", ``}, {"POST", "https://x/api", `total=5`},
	} {
		if !mayPlaceOrder(c.m, c.u, c.body) {
			t.Errorf("%s %s must not get out with no claim in progress", c.m, c.u)
		}
	}
	if mayPlaceOrder("GET", confirm, "") || mayPlaceOrder("POST", "https://x/graphql", `{"query":"x"}`) {
		t.Error("ordinary requests pass")
	}
}

func TestOrderRepliesAreReadStrictly(t *testing.T) {
	const confirm = "https://payment-website-pci.ol.epicgames.com/v2/purchase/confirm-order"
	const preview = "https://payment-website-pci.ol.epicgames.com/v2/purchase/order-preview"
	reply := func(status int, body string) (bool, string, *orderWatch) {
		w := newOrderWatch(false)
		w.onResponse("POST", confirm, status, []byte(body))
		ok, msg := w.confirmation()
		return ok, msg, w
	}
	for _, body := range []string{`{"confirmation":"A1","error":null}`, `{"orderComplete":true,"orderPending":false,"orderId":"A1","errors":[]}`} {
		if ok, msg, w := reply(200, body); !ok || msg != "" || !strings.Contains(w.confirmationNote(), "reply fields:") {
			t.Errorf("accepted %s: %v %q %q", body, ok, msg, w.confirmationNote())
		}
	}
	// Not an error, but not an accepted order either: the library decides.
	for _, body := range []string{``, `ok`, `<html></html>`, `[]`, `{"orderPending":true}`, `{"orderComplete":false,"orderId":"A1"}`, `{"redirectUrl":"https://x/3ds"}`} {
		if ok, msg, _ := reply(200, body); ok || msg != "" {
			t.Errorf("reply %q: confirmed=%v error=%q, want neither", body, ok, msg)
		}
	}
	for _, body := range []string{
		`{"success":false}`, `{"status":"FAILED"}`, `{"errorCode":"errors.x.y"}`, `{"errors":[{"message":"no"}]}`,
		`{"errorMessage":"The order could not be completed"}`, `{"errorCode":1012,"errorMessage":"Payment declined"}`,
		`{"data":{"confirmOrder":{"errorMessage":"declined"}}}`, `{"error_message":"no"}`,
	} {
		if ok, msg, _ := reply(200, body); ok || msg == "" {
			t.Errorf("reply %q: confirmed=%v error=%q, want a rejection", body, ok, msg)
		}
	}
	// A flag of failure on something that merely travels inside an accepted
	// order's reply is not a rejection of the order.
	for _, body := range []string{
		`{"orderComplete":true,"orderPending":false,"orderError":null,"orderId":"A1","rewards":{"success":false}}`,
		`{"orderComplete":true,"orderId":"A1","earnRewards":{"status":"FAILED"}}`,
		`{"confirmation":"A1","threeDS":{"ok":false}}`,
	} {
		if ok, msg, _ := reply(200, body); !ok || msg != "" {
			t.Errorf("reply %q: confirmed=%v error=%q, want accepted", body, ok, msg)
		}
	}
	// The same reply always reads the same.
	for i := 0; i < 20; i++ {
		if _, msg, _ := reply(400, `{"message":"Nope.","errorCode":"errors.a","error":"errors.b"}`); msg != "Nope. (errors.a)" {
			t.Fatalf("message = %q", msg)
		}
	}
	// What Epic's order service answers for something the account has.
	_, msg, w := reply(404, `{"errorCode":"errors.com.epicgames.ecommerce.orderprocessor.purchase_limit_error","errorMessage":"Purchase limit reached","numericErrorCode":1}`)
	if oc, known := classifyError(msg); !w.ownedHint() || !known || oc != Owned {
		t.Errorf("purchase limit: %q owned=%v", msg, w.ownedHint())
	}
	// ...also when it is said in the message alone.
	if _, _, w := reply(404, `{"errorMessage":"Sorry, you already own this item."}`); !w.ownedHint() {
		t.Error("an error message saying it is owned must be recognised")
	}
	if ok, msg, _ := reply(200, `{"message":"Thank you!","orderId":"A1"}`); !ok || msg != "" {
		t.Errorf("a bare message is not an error: %v %q", ok, msg)
	}
	// Words that merely appear in a preview are not statements about the
	// account.
	w = newOrderWatch(false)
	w.onResponse("POST", preview, 200, []byte(`{"orderResponse":{"totalPrice":0},"message":"ok","offer":{"description":"If you already own the base game, this is unavailable in your region."}}`))
	if w.ownedHint() || w.regionHint() || w.previewVerdict() != Free {
		t.Errorf("preview with words: owned=%v region=%v verdict=%v", w.ownedHint(), w.regionHint(), w.previewVerdict())
	}
	// A zero under some other "total" is not the order's total.
	for body, want := range map[string]Verdict{
		`{"rewards":{"total":0}}`: Unknown,
		`{"orderResponse":{"totalPrice":0,"lineOffers":[{"totalPrice":0}]}}`:              Free,
		`{"orderResponse":{"totalPrice":0,"lineOffers":[{"totalPrice":1999}]}}`:           Paid,
		`{"orderResponse":{"currency":"USD"},"coupons":{"total":0}}`:                      Unknown,
		`{"orderResponse":{"totalPrice":"0.00"},"rewards":{"total":0}}`:                   Free,
		`{"orderResponse":{"totalPrice":0},"rewards":{"totalAmount":{"amount":"$5.00"}}}`: Paid,
		`{"orderResponse":{"totalPrice":{"amount":0,"totalPrice":1999}}}`:                 Paid,
		`{"orderResponse":{"totalPrice":0,"lineOffers":[{"totalPrice":"19,99 zł"}]}}`:     Paid,
	} {
		if v, facts := JudgePreview([]byte(body)); v != want {
			t.Errorf("JudgePreview(%s) = %v %v, want %v", body, v, facts, want)
		}
	}
}
