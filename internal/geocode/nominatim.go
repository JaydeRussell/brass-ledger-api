// Package geocode turns a typed place name into coordinates through
// OpenStreetMap's Nominatim, within its usage policy
// (https://operations.osmfoundation.org/policies/nominatim/): at most one
// request a second across the whole service, an identifying User-Agent,
// cached results, and lookups only on an explicit search, never as
// autocomplete. Requests go from this server, so users' IP addresses never
// reach OpenStreetMap.
package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://nominatim.openstreetmap.org"
	userAgent      = "BrassLedger/1.0 (+https://brass-ledger.app)"
	minInterval    = time.Second
	cacheTTL       = 7 * 24 * time.Hour
	maxCached      = 1024
	maxResults     = 5
)

// Place is one match for a typed place name.
type Place struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

type cached struct {
	places []Place
	at     time.Time
}

// Client looks places up on Nominatim.
type Client struct {
	http    *http.Client
	baseURL string

	// Serializes requests so they stay minInterval apart.
	gate sync.Mutex
	last time.Time

	mu    sync.Mutex
	cache map[string]cached
}

// New builds a Client against the public Nominatim service.
func New() *Client { return NewWithBaseURL(defaultBaseURL) }

// NewWithBaseURL builds a Client against another Nominatim, for tests.
func NewWithBaseURL(base string) *Client {
	return &Client{
		http:    &http.Client{Timeout: 10 * time.Second},
		baseURL: strings.TrimSuffix(base, "/"),
		cache:   map[string]cached{},
	}
}

// ErrUnavailable is returned when Nominatim can't be reached or refuses.
// It carries no request detail, since the query is the user's typed place.
var ErrUnavailable = errors.New("place lookup unavailable")

// Normalize is the form queries are cached under.
func Normalize(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// Search returns up to five places matching q, which must already be
// normalized. Coordinates are rounded to two decimals (about a kilometre).
func (c *Client) Search(ctx context.Context, q string) ([]Place, error) {
	c.mu.Lock()
	if e, ok := c.cache[q]; ok && time.Since(e.at) < cacheTTL {
		c.mu.Unlock()
		return e.places, nil
	}
	c.mu.Unlock()

	places, err := c.fetch(ctx, q)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if len(c.cache) >= maxCached {
		clear(c.cache)
	}
	c.cache[q] = cached{places: places, at: time.Now()}
	c.mu.Unlock()
	return places, nil
}

func (c *Client) wait(ctx context.Context) error {
	c.gate.Lock()
	defer c.gate.Unlock()
	if d := minInterval - time.Since(c.last); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()
	return nil
}

func (c *Client) fetch(ctx context.Context, q string) ([]Place, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("q", q)
	params.Set("format", "jsonv2")
	params.Set("limit", strconv.Itoa(maxResults))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/search?"+params.Encode(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, res.StatusCode)
	}

	var body []struct {
		DisplayName string `json:"display_name"`
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: unreadable response", ErrUnavailable)
	}
	places := make([]Place, 0, len(body))
	for _, b := range body {
		lat, errLat := strconv.ParseFloat(b.Lat, 64)
		lon, errLon := strconv.ParseFloat(b.Lon, 64)
		if errLat != nil || errLon != nil {
			continue
		}
		places = append(places, Place{Name: b.DisplayName, Lat: round2(lat), Lon: round2(lon)})
	}
	return places, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
