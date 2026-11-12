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

// Endpoint is the public free-games feed.
const Endpoint = "https://store-site-backend-static-ipv4.ak.epicgames.com/freeGamesPromotions"

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
	// Mobile is true when the offer is only for Android/iOS and cannot be
	// claimed through the desktop checkout.
	Mobile bool `json:"mobile"`
}

// Key identifies an offer across runs.
func (o Offer) Key() string { return o.Namespace + ":" + o.OfferID }

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
	// NextChange is the earliest future moment at which the set of free
	// offers is expected to change (a promotion starting or ending). Zero if
	// the feed does not say.
	NextChange time.Time
}

type rawFeed struct {
	Data struct {
		Catalog struct {
			SearchStore struct {
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

// slugOf picks the store page slug for an element.
//
// Order matters: the catalog namespace mapping is the canonical product page;
// offer mappings carry the page of add-ons and editions; productSlug is the
// legacy field (often with a trailing "/home"); urlSlug is a last resort and
// is frequently an opaque id that is not a real page.
func slugOf(e rawElement) string {
	if e.OfferType != "ADD_ON" && e.OfferType != "DLC" {
		for _, m := range e.CatalogNs.Mappings {
			if m.PageType == "productHome" && m.PageSlug != "" {
				return m.PageSlug
			}
		}
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
	if s := strings.TrimSuffix(strings.TrimSpace(e.ProductSlug), "/home"); s != "" && s != "[]" {
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

// isMobileOnly reports whether the element is tagged for Android/iOS without
// any desktop category. Epic's mobile giveaways cannot be claimed through the
// desktop checkout.
func isMobileOnly(e rawElement) bool {
	mobile, desktop := false, false
	for _, c := range e.Categories {
		p := strings.ToLower(c.Path)
		switch {
		case strings.Contains(p, "android"), strings.Contains(p, "ios"), strings.Contains(p, "mobile"):
			mobile = true
		case p == "games", strings.HasPrefix(p, "games/"), p == "applications", p == "bundles", strings.HasPrefix(p, "bundles/"), p == "addons", strings.HasPrefix(p, "addons/"):
			desktop = true
		}
	}
	return mobile && !desktop
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
		Title:     strings.TrimSpace(e.Title),
		Namespace: e.Namespace,
		OfferID:   e.ID,
		OfferType: e.OfferType,
		Slug:      slugOf(e),
		PagePath:  "p",
		Seller:    e.Seller.Name,
		Start:     start,
		End:       end,
		Mobile:    isMobileOnly(e),
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
}

// Fetch downloads and parses the feed for a country (ISO 3166-1 alpha-2).
func (c *Client) Fetch(ctx context.Context, country, locale string, now time.Time) (Snapshot, error) {
	if country == "" {
		country = "US"
	}
	if locale == "" {
		locale = "en-US"
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = Endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return Snapshot{}, err
	}
	q := u.Query()
	q.Set("locale", locale)
	q.Set("country", strings.ToUpper(country))
	q.Set("allowCountries", strings.ToUpper(country))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Snapshot{}, err
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
		return Snapshot{}, fmt.Errorf("fetch free-games feed: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read free-games feed: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("free-games feed returned HTTP %d", res.StatusCode)
	}
	if len(body) == 0 {
		return Snapshot{}, errors.New("free-games feed returned an empty body")
	}
	return Parse(body, now)
}
