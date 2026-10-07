package geocode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearch_IdentifiesRoundsAndCaches(t *testing.T) {
	var calls atomic.Int32
	var ua, q atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		ua.Store(r.Header.Get("User-Agent"))
		q.Store(r.URL.Query().Get("q"))
		_, _ = w.Write([]byte(`[{"display_name": "Denver, Colorado, United States", "lat": "39.7392364", "lon": "-104.984862"}, {"display_name": "Bad", "lat": "x", "lon": "1"}]`))
	}))
	t.Cleanup(server.Close)
	c := NewWithBaseURL(server.URL)

	places, err := c.Search(context.Background(), "denver")
	if err != nil {
		t.Fatal(err)
	}
	if len(places) != 1 || places[0].Lat != 39.74 || places[0].Lon != -104.98 {
		t.Errorf("places = %+v, want one place rounded to 2 decimals", places)
	}
	if ua.Load() != userAgent || q.Load() != "denver" {
		t.Errorf("request UA %q q %q", ua.Load(), q.Load())
	}
	if _, err := c.Search(context.Background(), "denver"); err != nil || calls.Load() != 1 {
		t.Errorf("repeat lookup: calls %d err %v, want 1 call (cached)", calls.Load(), err)
	}
}

func TestSearch_SpacesRequestsASecondApart(t *testing.T) {
	var times []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		times = append(times, time.Now())
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	c := NewWithBaseURL(server.URL)

	_, _ = c.Search(context.Background(), "a place")
	_, _ = c.Search(context.Background(), "another place")
	if len(times) != 2 || times[1].Sub(times[0]) < minInterval-50*time.Millisecond {
		t.Errorf("requests %v apart, want at least %v", times[1].Sub(times[0]), minInterval)
	}
}

func TestSearch_FailureCarriesNoQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	_, err := NewWithBaseURL(server.URL).Search(context.Background(), "secret street")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got := err.Error(); got != "place lookup unavailable: HTTP 429" {
		t.Errorf("err = %q, want no query or URL in it", got)
	}
}
