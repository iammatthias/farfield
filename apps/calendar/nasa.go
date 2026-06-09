package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// calendarStart is the first day Farfield's public calendar records. NASA APOD
// goes back to 1995, but this app intentionally starts on Jan 1 2026 and then
// records forward from there.
const calendarStart = "2026-01-01"

// apodBase is NASA's APOD JSON endpoint. It is queried by single date or by
// start_date/end_date range; thumbs=true asks for a still on video days.
const apodBase = "https://api.nasa.gov/planetary/apod"

// apodResponse is one APOD record. Error responses carry code/msg instead.
type apodResponse struct {
	Date         string `json:"date"`
	Title        string `json:"title"`
	Explanation  string `json:"explanation"`
	URL          string `json:"url"`
	HDURL        string `json:"hdurl"`
	ThumbnailURL string `json:"thumbnail_url"`
	MediaType    string `json:"media_type"`
	Copyright    string `json:"copyright"`
	Code         int    `json:"code"`
	Msg          string `json:"msg"`
}

// apodToPhoto converts one APOD record into a calendar Photo.
func apodToPhoto(a apodResponse) Photo {
	media := a.MediaType
	if media == "" {
		media = "image"
	}
	image, thumb := a.URL, a.ThumbnailURL
	if media == "image" {
		// hdurl is the full-resolution still; url is a sensible thumbnail.
		if a.HDURL != "" {
			image = a.HDURL
		}
		if thumb == "" {
			thumb = a.URL
		}
	}
	p := Photo{
		Source:      sourceNASA,
		Date:        a.Date,
		Title:       strings.TrimSpace(a.Title),
		Explanation: strings.TrimSpace(a.Explanation),
		ImageURL:    image,
		ThumbURL:    thumb,
		MediaType:   media,
		Credit:      strings.TrimSpace(a.Copyright),
		SourceURL:   apodPageURL(a.Date),
		FetchedAt:   store.NowRFC3339(),
	}
	return p
}

// apodPageURL is the human-readable apod.nasa.gov page for a given date.
func apodPageURL(date string) string {
	t, err := time.Parse(dateLayout, date)
	if err != nil {
		return "https://apod.nasa.gov/apod/"
	}
	return "https://apod.nasa.gov/apod/ap" + t.Format("060102") + ".html"
}

// parseAPOD decodes an APOD response body. The endpoint returns a single
// object for a date query and an array for a range query; this handles both
// and surfaces the API's own error envelope as a Go error.
func parseAPOD(data []byte) ([]apodResponse, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("empty APOD response")
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []apodResponse
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, fmt.Errorf("decoding APOD array: %w", err)
		}
		return arr, nil
	}
	var one apodResponse
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, fmt.Errorf("decoding APOD object: %w", err)
	}
	if one.Code != 0 || one.Msg != "" {
		return nil, fmt.Errorf("APOD error %d: %s", one.Code, one.Msg)
	}
	if one.Date == "" {
		return nil, fmt.Errorf("APOD response has no date")
	}
	return []apodResponse{one}, nil
}

// nasaDay fetches a single day's APOD record. Concurrent cache misses for the
// same day are deduplicated into one upstream call.
func (f *fetcher) nasaDay(ctx context.Context, date string) (*Photo, error) {
	q := url.Values{}
	q.Set("api_key", f.nasaKey)
	q.Set("date", date)
	q.Set("thumbs", "true")
	records, err := f.flights.do("day:"+date, func() ([]apodResponse, error) {
		return f.nasaGet(ctx, q)
	})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("APOD returned no record for %s", date)
	}
	p := apodToPhoto(records[0])
	p.Date = date // trust the request date over any drift in the response
	return &p, nil
}

// nasaRange fetches every APOD record in the inclusive [start, end] range in a
// single request — the efficient way to warm an archive page. Concurrent
// misses for the same range are deduplicated into one upstream call.
func (f *fetcher) nasaRange(ctx context.Context, start, end string) ([]Photo, error) {
	q := url.Values{}
	q.Set("api_key", f.nasaKey)
	q.Set("start_date", start)
	q.Set("end_date", end)
	q.Set("thumbs", "true")
	records, err := f.flights.do("range:"+start+".."+end, func() ([]apodResponse, error) {
		return f.nasaGet(ctx, q)
	})
	if err != nil {
		return nil, err
	}
	out := make([]Photo, 0, len(records))
	for _, r := range records {
		if r.Date == "" {
			continue
		}
		out = append(out, apodToPhoto(r))
	}
	return out, nil
}

// nasaGet performs one APOD request and parses the body.
func (f *fetcher) nasaGet(ctx context.Context, q url.Values) ([]apodResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apodBase+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("APOD rate limited (HTTP 429) — set NASA_API_KEY")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("APOD HTTP %d", resp.StatusCode)
	}
	return parseAPOD(body)
}
