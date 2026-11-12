// Package feed reads Epic's public "free games promotions" feed and turns it
// into the list of offers that are free to claim right now.
//
// The feed is a static, unauthenticated CDN document. It lists a rotating set
// of store offers; only some of them are free at any moment, so every entry
// has to be checked against its promotion window and its price.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Endpoint is the public free-games feed; FallbackEndpoint serves the same
// document from a second host.
const (
	Endpoint         = "https://store-site-backend-static-ipv4.ak.epicgames.com/freeGamesPromotions"
	FallbackEndpoint = "https://store-site-backend-static.ak.epicgames.com/freeGamesPromotions"
)

// Offer is one store offer that is free for a limited time.
type Offer struct {
	Title     string    `json:"title"`
	Namespace string    `json:"namespace"`
	OfferID   string    `json:"offerId"`
	OfferType string    `json:"offerType"` // BASE_GAME, ADD_ON, BUNDLE, DLC, EDITION, ...
	Slug      string    `json:"slug"`      // store page slug; may be empty
	PagePath  string    `json:"pagePath"`  // "p" or "bundles"
	ItemIDs   []string  `json:"itemIds"`
	Seller    string    `json:"seller"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	// Prices are in the currency's minor units (cents) as reported by the feed.
	OriginalPrice int64  `json:"originalPrice"`
	DiscountPrice int64  `json:"discountPrice"`
	Currency      string `json:"currency"`
	// Placeholder is true for the "mystery game" slots of Epic's holiday
	// events: the feed lists them under a stand-in namespace and id, so the
	// real offer has to be looked up by its page (Slug) before it can be
	// ordered. Slug is empty until the game has been revealed.
	Placeholder bool `json:"placeholder"`
}

// Key identifies one giveaway across runs. The start of the promotion is
// part of it because Epic gives the same game away again in later years.
func (o Offer) Key() string {
	return o.Namespace + ":" + o.OfferID + "@" + o.Start.UTC().Format("2006-01-02T15:04Z")
}

// PurchaseToken is the "1-<namespace>-<offerId>" form the checkout expects.
func (o Offer) PurchaseToken() string { return "1-" + o.Namespace + "-" + o.OfferID }

// ProductURL is the human-facing store page, or "" when the slug is unknown.
func (o Offer) ProductURL(locale string) string {
	if o.Slug == "" {
		return ""
	}
	if locale == "" {
		locale = "en-US"
	}
	path := o.PagePath
	if path == "" {
		path = "p"
	}
	return "https://store.epicgames.com/" + locale + "/" + path + "/" + o.Slug
}

// Snapshot is the result of reading the feed once.
type Snapshot struct {
	// FreeNow are offers whose free promotion is active at the snapshot time
	// and whose reported price is zero.
	FreeNow []Offer
	// Upcoming are offers with a 100%-off promotion that has not started yet.
	Upcoming []Offer
	// Stale is true when the document still lists as "upcoming" (or at full
	// price) a giveaway whose start time has passed: Epic's servers have
	// not caught up yet, and the list is worth reading again soon.
	Stale bool
	// NextChange is the earliest future moment at which the set of free
	// offers is expected to change (a promotion starting or ending). Zero if
	// the feed does not say.
	NextChange time.Time
	// ClockSkew is how far Epic's clock is ahead of this PC's (negative:
	// behind) when the difference is large enough to matter. What is free
	// is then judged by Epic's time, not the PC's. Zero otherwise.
	ClockSkew time.Duration
	// Country is the country the list was read for. It differs from the
	// one asked for when Epic would not answer for that one.
	Country string
}

type rawFeed struct {
	Errors []struct {
		Message         string `json:"message"`
		Status          int    `json:"status"`
		ServiceResponse string `json:"serviceResponse"`
	} `json:"errors"`
	Data *struct {
		Catalog *struct {
			SearchStore *struct {
				Elements []rawElement `json:"elements"`
			} `json:"searchStore"`
		} `json:"Catalog"`
	} `json:"data"`
}

type rawMapping struct {
	PageSlug string `json:"pageSlug"`
	PageType string `json:"pageType"`
}

type rawPromo struct {
	StartDate       string `json:"startDate"`
	EndDate         string `json:"endDate"`
	DiscountSetting struct {
		DiscountType       string   `json:"discountType"`
		DiscountPercentage *float64 `json:"discountPercentage"`
	} `json:"discountSetting"`
}

type rawPromoGroup struct {
	PromotionalOffers []rawPromo `json:"promotionalOffers"`
}

type rawElement struct {
	Title       string `json:"title"`
	ID          string `json:"id"`
	Namespace   string `json:"namespace"`
	OfferType   string `json:"offerType"`
	Status      string `json:"status"`
	CodeOnly    bool   `json:"isCodeRedemptionOnly"`
	ProductSlug string `json:"productSlug"`
	URLSlug     string `json:"urlSlug"`
	Seller      struct {
		Name string `json:"name"`
	} `json:"seller"`
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
	Categories []struct {
		Path string `json:"path"`
	} `json:"categories"`
	CatalogNs struct {
		Mappings []rawMapping `json:"mappings"`
	} `json:"catalogNs"`
	OfferMappings []rawMapping `json:"offerMappings"`
	Price         *struct {
		TotalPrice struct {
			DiscountPrice *float64 `json:"discountPrice"`
			OriginalPrice *float64 `json:"originalPrice"`
			CurrencyCode  string   `json:"currencyCode"`
		} `json:"totalPrice"`
	} `json:"price"`
	Promotions *struct {
		PromotionalOffers         []rawPromoGroup `json:"promotionalOffers"`
		UpcomingPromotionalOffers []rawPromoGroup `json:"upcomingPromotionalOffers"`
	} `json:"promotions"`
}

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// isPlaceholder recognises the holiday "mystery game" slots.
func isPlaceholder(e rawElement) bool {
	if e.CodeOnly || e.OfferType == "OTHERS" {
		return true
	}
	for _, c := range e.Categories {
		if c.Path == "freegames/vaulted" {
			return true
		}
	}
	return false
}

// legacySlug cleans the old productSlug field ("name/home", "[]", "a/b").
func legacySlug(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" {
		return ""
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// slugOf picks the store page slug for an element.
//
// Order matters: the offer's own mapping is the page of exactly this offer
// (for add-ons and editions the catalog mapping is the parent game's page);
// the catalog mapping is the product's home page; productSlug is the legacy
// field; urlSlug is a last resort and is frequently an opaque id or an
// unrelated internal name, not a real page.
func slugOf(e rawElement) string {
	if isPlaceholder(e) {
		// Only the legacy field carries the revealed game's page.
		return legacySlug(e.ProductSlug)
	}
	for _, m := range e.OfferMappings {
		if m.PageSlug != "" {
			return m.PageSlug
		}
	}
	for _, m := range e.CatalogNs.Mappings {
		if m.PageType == "productHome" && m.PageSlug != "" {
			return m.PageSlug
		}
	}
	if s := legacySlug(e.ProductSlug); s != "" {
		return s
	}
	if s := strings.TrimSpace(e.URLSlug); s != "" && !hexID.MatchString(s) {
		return s
	}
	return ""
}

func isBundle(e rawElement) bool {
	if e.OfferType == "BUNDLE" {
		return true
	}
	for _, c := range e.Categories {
		if c.Path == "bundles" || strings.HasPrefix(c.Path, "bundles/") {
			return true
		}
	}
	return false
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func isFreePromo(p rawPromo) bool {
	return p.DiscountSetting.DiscountPercentage != nil && *p.DiscountSetting.DiscountPercentage == 0
}

func offerFrom(e rawElement, start, end time.Time) Offer {
	o := Offer{
		Title:       strings.TrimSpace(e.Title),
		Namespace:   e.Namespace,
		OfferID:     e.ID,
		OfferType:   e.OfferType,
		Slug:        slugOf(e),
		PagePath:    "p",
		Seller:      e.Seller.Name,
		Start:       start,
		End:         end,
		Placeholder: isPlaceholder(e),
		// Unknown prices must never look free.
		OriginalPrice: -1,
		DiscountPrice: -1,
	}
	if isBundle(e) {
		o.PagePath = "bundles"
	}
	for _, it := range e.Items {
		if it.ID != "" {
			o.ItemIDs = append(o.ItemIDs, it.ID)
		}
	}
	if e.Price != nil {
		if v := e.Price.TotalPrice.OriginalPrice; v != nil {
			o.OriginalPrice = int64(*v)
		}
		if v := e.Price.TotalPrice.DiscountPrice; v != nil {
			o.DiscountPrice = int64(*v)
		}
		o.Currency = e.Price.TotalPrice.CurrencyCode
	}
	if o.Title == "" {
		o.Title = o.Slug
	}
	return o
}

// Parse extracts the free offers from a feed document as of the given time.
func Parse(data []byte, now time.Time) (Snapshot, error) {
	var raw rawFeed
	if err := json.Unmarshal(data, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("decode free-games feed: %w", err)
	}
	// Epic reports problems (an unknown country code, a backend failure)
	// with HTTP 200 and an empty catalogue. That is not "nothing is free".
	if raw.Data == nil || raw.Data.Catalog == nil || raw.Data.Catalog.SearchStore == nil {
		msg := "no catalogue in the reply"
		refused := false
		if len(raw.Errors) > 0 {
			e := raw.Errors[0]
			if e.Message != "" {
				msg = e.Message
				if len(msg) > 200 {
					msg = msg[:200]
				}
			}
			// Epic's answer to a country it does not know: a refusal that
			// asking again will not change. Anything else of this shape is
			// a failure on Epic's side that may well pass.
			refused = e.Status == 400 || strings.Contains(e.ServiceResponse, "invalid_country_code") || strings.Contains(e.Message, "status code 400")
		}
		if refused {
			return Snapshot{}, permanentError{fmt.Errorf("free-games feed: %s (is the country code right?)", msg)}
		}
		return Snapshot{}, fmt.Errorf("free-games feed: %s", msg)
	}
	// Epic always has something on offer. A list with nothing in it at all
	// is a hiccup on their side, not news.
	if len(raw.Data.Catalog.SearchStore.Elements) == 0 {
		return Snapshot{}, errors.New("free-games feed: the list came back empty")
	}
	now = now.UTC()
	var snap Snapshot
	seenNow := map[string]bool{}
	seenUp := map[string]bool{}
	noteChange := func(t time.Time) {
		if t.After(now) && (snap.NextChange.IsZero() || t.Before(snap.NextChange)) {
			snap.NextChange = t
		}
	}

	for _, e := range raw.Data.Catalog.SearchStore.Elements {
		if e.ID == "" || e.Namespace == "" || e.Promotions == nil {
			continue
		}
		for _, g := range e.Promotions.PromotionalOffers {
			for _, p := range g.PromotionalOffers {
				if !isFreePromo(p) {
					continue
				}
				start, okS := parseTime(p.StartDate)
				end, okE := parseTime(p.EndDate)
				if !okS || !okE || now.Before(start) || !now.Before(end) {
					continue
				}
				noteChange(end)
				o := offerFrom(e, start, end)
				// The promotion says "100% off"; the price must agree. A
				// stale or inconsistent feed entry is skipped, never bought.
				if o.DiscountPrice != 0 {
					snap.Stale = true
					continue
				}
				// An unrevealed mystery game has nothing to claim yet.
				if o.Placeholder && o.Slug == "" {
					continue
				}
				if !seenNow[o.Key()] {
					seenNow[o.Key()] = true
					snap.FreeNow = append(snap.FreeNow, o)
				}
			}
		}
		for _, g := range e.Promotions.UpcomingPromotionalOffers {
			for _, p := range g.PromotionalOffers {
				if !isFreePromo(p) {
					continue
				}
				start, okS := parseTime(p.StartDate)
				end, okE := parseTime(p.EndDate)
				if !okS || !okE || !now.Before(end) {
					continue
				}
				if !start.After(now) {
					snap.Stale = true // started, yet still listed as upcoming
					continue
				}
				noteChange(start)
				o := offerFrom(e, start, end)
				if !seenUp[o.Key()] && !seenNow[o.Key()] {
					seenUp[o.Key()] = true
					snap.Upcoming = append(snap.Upcoming, o)
				}
			}
		}
	}
	sort.SliceStable(snap.FreeNow, func(i, j int) bool { return snap.FreeNow[i].End.Before(snap.FreeNow[j].End) })
	sort.SliceStable(snap.Upcoming, func(i, j int) bool { return snap.Upcoming[i].Start.Before(snap.Upcoming[j].Start) })
	return snap, nil
}

// Client fetches the feed.
type Client struct {
	HTTP     *http.Client
	Endpoint string // overridable for tests
	// UserAgent is sent with requests. Keep it generic.
	UserAgent string
	// Rounds is how many times the hosts are gone through before giving
	// up (default 3), and Pause the wait before the second round (default
	// 2 s; it triples for each further one).
	Rounds int
	Pause  time.Duration
}

// maxSkew is how far the PC's clock may differ from Epic's before Epic's
// is used instead. (An answer served from a cache can carry a time that is
// a few minutes old, so small differences mean nothing.)
const maxSkew = 10 * time.Minute

// permanentError marks an answer that asking again will not change (the
// server understood the request and refused it).
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// serverTime reads the time of the answer from its headers: when it was
// generated, plus how long a cache has been holding it.
func serverTime(h http.Header) time.Time {
	t, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	if age, err := time.ParseDuration(strings.TrimSpace(h.Get("Age")) + "s"); err == nil && age > 0 && age < 24*time.Hour {
		t = t.Add(age)
	}
	return t
}

// normalCountry maps what people type to the ISO code Epic expects.
func normalCountry(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	switch c {
	case "":
		return "US"
	case "UK":
		return "GB"
	}
	return c
}

func (c *Client) fetchFrom(ctx context.Context, endpoint, country, locale string) ([]byte, time.Time, error) {
	body, at, err := c.fetchOnce(ctx, endpoint, country, locale)
	return body, at, err
}

func (c *Client) fetchOnce(ctx context.Context, endpoint, country, locale string) ([]byte, time.Time, error) {
	var none time.Time
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, none, err
	}
	q := u.Query()
	q.Set("locale", locale)
	q.Set("country", country)
	q.Set("allowCountries", country)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, none, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, none, fmt.Errorf("fetch free-games feed: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, none, fmt.Errorf("read free-games feed: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		err := fmt.Errorf("free-games feed returned HTTP %d", res.StatusCode)
		// "Too many requests" and timeouts pass; other refusals do not.
		if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != http.StatusRequestTimeout && res.StatusCode != http.StatusTooEarly && res.StatusCode != http.StatusTooManyRequests {
			return nil, none, permanentError{err}
		}
		return nil, none, err
	}
	if len(body) == 0 {
		return nil, none, errors.New("free-games feed returned an empty body")
	}
	return body, serverTime(res.Header), nil
}

// Fetch downloads and parses the feed for a country (ISO 3166-1 alpha-2).
//
// It does not give up easily: both of Epic's hosts are tried, several times
// over with a pause in between (a PC that has just woken up is often not
// online yet, and Epic's servers have their moments), and if Epic refuses
// to answer for the country asked for, the list for the United States is
// read instead. Whatever list comes back only says what to try: nothing is
// ordered unless Epic's checkout itself shows a price of zero.
func (c *Client) Fetch(ctx context.Context, country, locale string, now time.Time) (Snapshot, error) {
	country = normalCountry(country)
	if locale == "" {
		locale = "en-US"
	}
	snap, err := c.fetchCountry(ctx, country, locale, now)
	if err == nil || country == "US" || ctx.Err() != nil {
		return snap, err
	}
	var refused permanentError
	if !errors.As(err, &refused) {
		return snap, err
	}
	if alt, altErr := c.fetchCountry(ctx, "US", locale, now); altErr == nil {
		return alt, nil
	}
	return snap, err
}

func (c *Client) fetchCountry(ctx context.Context, country, locale string, now time.Time) (Snapshot, error) {
	endpoints := []string{Endpoint, FallbackEndpoint}
	if c.Endpoint != "" {
		endpoints = []string{c.Endpoint}
	}
	rounds, pause := c.Rounds, c.Pause
	if rounds <= 0 {
		rounds = 3
	}
	if pause <= 0 {
		pause = 2 * time.Second
	}
	var firstErr error
	for round := 0; round < rounds; round++ {
		if round > 0 {
			t := time.NewTimer(pause)
			select {
			case <-ctx.Done():
				t.Stop()
				return Snapshot{}, firstErr
			case <-t.C:
			}
			pause *= 3
		}
		refusals := 0
		for _, ep := range endpoints {
			body, at, err := c.fetchFrom(ctx, ep, country, locale)
			if err == nil {
				// Judge by Epic's clock when the PC's is far off.
				when, skew := now, time.Duration(0)
				if d := at.Sub(now); !at.IsZero() && (d > maxSkew || d < -maxSkew) {
					when, skew = at, d
				}
				var snap Snapshot
				if snap, err = Parse(body, when); err == nil {
					snap.ClockSkew, snap.Country = skew, country
					return snap, nil
				}
			}
			if firstErr == nil {
				firstErr = err
			}
			var refused permanentError
			if errors.As(err, &refused) {
				refusals++
			}
			if ctx.Err() != nil {
				return Snapshot{}, firstErr
			}
		}
		if refusals == len(endpoints) {
			break // asking again would get the same answer
		}
	}
	return Snapshot{}, firstErr
}
