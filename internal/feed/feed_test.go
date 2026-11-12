package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func loadReal(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/feed-2026-11-12-us.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func titles(os []Offer) []string {
	var out []string
	for _, o := range os {
		out = append(out, o.Title)
	}
	return out
}

func TestParseRealFeedDuringPromotion(t *testing.T) {
	snap, err := Parse(loadReal(t), mustTime(t, "2026-11-12T04:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(snap.FreeNow); len(got) != 2 || got[0] != "Idle Champions of the Forgotten Realms" || got[1] != "Flotsam" {
		t.Fatalf("free now = %v", got)
	}
	flotsam := snap.FreeNow[1]
	if flotsam.Namespace != "27e33143fc9e4de8bbf76782292933d4" || flotsam.OfferID != "3be0a0d8479946d78f60d90c9442a0b4" {
		t.Fatalf("ids = %+v", flotsam)
	}
	if flotsam.PurchaseToken() != "1-27e33143fc9e4de8bbf76782292933d4-3be0a0d8479946d78f60d90c9442a0b4" {
		t.Fatalf("purchase token = %s", flotsam.PurchaseToken())
	}
	// productSlug is null and urlSlug is an opaque id for this entry; the
	// catalog mapping is the real page.
	if got := flotsam.ProductURL("en-US"); got != "https://store.epicgames.com/en-US/p/flotsam-266287" {
		t.Fatalf("url = %s", got)
	}
	if flotsam.OriginalPrice != 2499 || flotsam.DiscountPrice != 0 || flotsam.Currency != "USD" {
		t.Fatalf("price = %+v", flotsam)
	}
	if len(flotsam.ItemIDs) != 1 || flotsam.ItemIDs[0] != "697883143d244f6c82e08782093f3a9e" {
		t.Fatalf("items = %v", flotsam.ItemIDs)
	}
	if got := titles(snap.Upcoming); len(got) != 1 || got[0] != "Knightica" {
		t.Fatalf("upcoming = %v", got)
	}
	// The running promotions end at the same instant the next one starts.
	if want := mustTime(t, "2026-11-12T16:00:00Z"); !snap.NextChange.Equal(want) {
		t.Fatalf("next change = %v, want %v", snap.NextChange, want)
	}
}

func TestParseRealFeedAfterPromotionEnds(t *testing.T) {
	// One minute after the window closes nothing in this (now stale) document
	// may be treated as free: the old promotions are over and the upcoming
	// one still carries its full price.
	snap, err := Parse(loadReal(t), mustTime(t, "2026-11-12T16:01:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.FreeNow) != 0 {
		t.Fatalf("free now = %v", titles(snap.FreeNow))
	}
}

func TestParseIgnoresPaidDiscountsAndBrokenEntries(t *testing.T) {
	doc := `{"data":{"Catalog":{"searchStore":{"elements":[
	 {"title":"Half Price","id":"o1","namespace":"n1","price":{"totalPrice":{"discountPrice":500,"originalPrice":1000}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountType":"PERCENTAGE","discountPercentage":50}}]}]}},
	 {"title":"Says Free But Costs Money","id":"o2","namespace":"n2","price":{"totalPrice":{"discountPrice":999,"originalPrice":999}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountType":"PERCENTAGE","discountPercentage":0}}]}]}},
	 {"title":"No Price Block","id":"o3","namespace":"n3",
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountType":"PERCENTAGE","discountPercentage":0}}]}]}},
	 {"title":"Null Promotions","id":"o4","namespace":"n4","price":{"totalPrice":{"discountPrice":0,"originalPrice":0}},"promotions":null},
	 {"title":"Missing Percentage","id":"o5","namespace":"n5","price":{"totalPrice":{"discountPrice":0,"originalPrice":100}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountType":"PERCENTAGE"}}]}]}},
	 {"title":"Bad Dates","id":"o6","namespace":"n6","price":{"totalPrice":{"discountPrice":0,"originalPrice":100}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"soon","endDate":"later","discountSetting":{"discountPercentage":0}}]}]}},
	 {"title":"","id":"","namespace":"n7"},
	 {"title":"Really Free","id":"o8","namespace":"n8","offerType":"BUNDLE","productSlug":"really-free/home","price":{"totalPrice":{"discountPrice":0,"originalPrice":100,"currencyCode":"EUR"}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountPercentage":0}}]}]}}
	]}}}}`
	snap, err := Parse([]byte(doc), mustTime(t, "2026-06-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(snap.FreeNow); len(got) != 1 || got[0] != "Really Free" {
		t.Fatalf("free now = %v", got)
	}
	if got := snap.FreeNow[0].ProductURL(""); got != "https://store.epicgames.com/en-US/bundles/really-free" {
		t.Fatalf("bundle url = %s", got)
	}
}

const freePromo = `"price":{"totalPrice":{"discountPrice":0,"originalPrice":1}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountPercentage":0}}]}]}`

func TestSlugSelection(t *testing.T) {
	doc := `{"data":{"Catalog":{"searchStore":{"elements":[
	 {"title":"AddOn","id":"a","namespace":"n","offerType":"ADD_ON","urlSlug":"x",
	  "catalogNs":{"mappings":[{"pageSlug":"base-game","pageType":"productHome"}]},
	  "offerMappings":[{"pageSlug":"base-game-addon-123","pageType":"offer"}],` + freePromo + `},
	 {"title":"Edition","id":"e","namespace":"n","offerType":"EDITION",
	  "catalogNs":{"mappings":[{"pageSlug":"godfall","pageType":"productHome"}]},
	  "offerMappings":[{"pageSlug":"godfall--challenger-edition","pageType":"productHome"}],` + freePromo + `},
	 {"title":"OpaqueOnly","id":"b","namespace":"n","urlSlug":"3f18c54a6e00424ba6e8f4422167cac7",` + freePromo + `},
	 {"title":"LegacyPath","id":"c","namespace":"n","productSlug":"some-game/standard-edition",` + freePromo + `}
	]}}}}`
	snap, err := Parse([]byte(doc), mustTime(t, "2026-06-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.FreeNow) != 4 {
		t.Fatalf("free now = %v", titles(snap.FreeNow))
	}
	by := map[string]Offer{}
	for _, o := range snap.FreeNow {
		by[o.Title] = o
	}
	if by["AddOn"].Slug != "base-game-addon-123" {
		t.Errorf("add-on slug = %q", by["AddOn"].Slug)
	}
	if by["Edition"].Slug != "godfall--challenger-edition" {
		t.Errorf("edition slug = %q (must be the edition's own page, not the parent game's)", by["Edition"].Slug)
	}
	if by["OpaqueOnly"].Slug != "" || by["OpaqueOnly"].ProductURL("") != "" {
		t.Errorf("opaque slug = %q", by["OpaqueOnly"].Slug)
	}
	if by["LegacyPath"].Slug != "some-game" {
		t.Errorf("legacy slug = %q", by["LegacyPath"].Slug)
	}
}

func TestMysteryGamePlaceholders(t *testing.T) {
	// Holiday "vault" slots: a stand-in namespace and id, productSlug "[]"
	// until the game is revealed.
	doc := `{"data":{"Catalog":{"searchStore":{"elements":[
	 {"title":"Mystery Game","id":"slot1","namespace":"d5241c76f178492ea1540fce45616757","offerType":"OTHERS","isCodeRedemptionOnly":true,
	  "productSlug":"[]","urlSlug":"mysterygame-10","categories":[{"path":"freegames/vaulted"},{"path":"freegames"}],
	  "price":{"totalPrice":{"discountPrice":0,"originalPrice":0}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountPercentage":0}}]}]}},
	 {"title":"Revealed Game","id":"slot2","namespace":"d5241c76f178492ea1540fce45616757","offerType":"OTHERS","isCodeRedemptionOnly":true,
	  "productSlug":"revealed-game","urlSlug":"mysterygame-9","categories":[{"path":"freegames/vaulted"}],
	  "price":{"totalPrice":{"discountPrice":0,"originalPrice":0}},
	  "promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-01-01T00:00:00.000Z","endDate":"2026-12-31T00:00:00.000Z","discountSetting":{"discountPercentage":0}}]}]}}
	]}}}}`
	snap, err := Parse([]byte(doc), mustTime(t, "2026-06-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(snap.FreeNow); len(got) != 1 || got[0] != "Revealed Game" {
		t.Fatalf("free now = %v (an unrevealed slot has nothing to claim)", got)
	}
	o := snap.FreeNow[0]
	if !o.Placeholder || o.Slug != "revealed-game" {
		t.Fatalf("offer = %+v", o)
	}
}

func TestErrorDocumentIsNotAnEmptyList(t *testing.T) {
	// What Epic answers (with HTTP 200) for an unknown country code.
	doc := `{"errors":[{"message":"CatalogQuery/searchStore: Request failed with status code 400","correlationId":"x"}],"data":{"Catalog":{"searchStore":null}}}`
	if _, err := Parse([]byte(doc), time.Now()); err == nil {
		t.Fatal("an error document must not be read as \"nothing is free\"")
	}
	if _, err := Parse([]byte(`{}`), time.Now()); err == nil {
		t.Fatal("a document without a catalogue is an error")
	}
	// A real, empty catalogue is fine.
	if snap, err := Parse([]byte(`{"data":{"Catalog":{"searchStore":{"elements":[]}}}}`), time.Now()); err != nil || len(snap.FreeNow) != 0 {
		t.Fatalf("empty catalogue: %v %v", snap, err)
	}
}

func TestStaleDocumentIsFlagged(t *testing.T) {
	// One minute after the changeover the edge still serves the old
	// document: the new giveaway has started but is listed as upcoming.
	snap, err := Parse(loadReal(t), mustTime(t, "2026-11-12T16:01:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Stale || len(snap.FreeNow) != 0 || len(snap.Upcoming) != 0 {
		t.Fatalf("snap = %+v", snap)
	}
	snap, _ = Parse(loadReal(t), mustTime(t, "2026-11-12T04:00:00Z"))
	if snap.Stale {
		t.Fatal("a current document is not stale")
	}
}

func TestKeyDistinguishesRepeatGiveaways(t *testing.T) {
	a := Offer{Namespace: "n", OfferID: "o", Start: mustTime(t, "2025-11-06T16:00:00Z")}
	b := Offer{Namespace: "n", OfferID: "o", Start: mustTime(t, "2026-11-05T16:00:00Z")}
	if a.Key() == b.Key() {
		t.Fatal("the same game given away in two different years must be two giveaways")
	}
	if a.Key() != "n:o@2025-11-06T16:00Z" {
		t.Fatalf("key = %s", a.Key())
	}
}

func TestFetchSendsCountryAndRejectsErrors(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Query().Get("country") == "ZZ" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadReal(t))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL}
	snap, err := c.Fetch(context.Background(), "de", "", mustTime(t, "2026-11-12T04:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.FreeNow) != 2 {
		t.Fatalf("free now = %v", titles(snap.FreeNow))
	}
	if gotQuery != "allowCountries=DE&country=DE&locale=en-US" {
		t.Fatalf("query = %s", gotQuery)
	}
	if _, err := c.Fetch(context.Background(), "uk", "", mustTime(t, "2026-11-12T04:00:00Z")); err != nil || gotQuery != "allowCountries=GB&country=GB&locale=en-US" {
		t.Fatalf("UK should be sent as GB: %v %s", err, gotQuery)
	}
	if _, err := c.Fetch(context.Background(), "ZZ", "", time.Now()); err == nil {
		t.Fatal("expected an error for HTTP 403")
	}
}
