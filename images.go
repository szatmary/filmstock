package filmstock

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Wikipedia asks API clients to send a descriptive User-Agent with contact info.
const userAgent = "filmstock/0.1 (movie database; +https://github.com/szatmary/filmstock)"

var imgClient = &http.Client{Timeout: 6 * time.Second}

// FetchPersonImage queries the MediaWiki pageimages API for a person's lead
// thumbnail, live. Returns "" if the article has no image or the lookup fails.
// Nothing is stored — repeat loads are served from the browser/HTTP cache.
func FetchPersonImage(name string) string {
	api := "https://en.wikipedia.org/w/api.php?action=query&format=json&redirects=1" +
		"&prop=pageimages&piprop=thumbnail&pithumbsize=250&titles=" + url.QueryEscape(name)
	req, err := http.NewRequest("GET", api, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := imgClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var data struct {
		Query struct {
			Pages map[string]struct {
				Thumbnail struct {
					Source string `json:"source"`
				} `json:"thumbnail"`
			} `json:"pages"`
		} `json:"query"`
	}
	if json.NewDecoder(resp.Body).Decode(&data) != nil {
		return ""
	}
	for _, p := range data.Query.Pages {
		if p.Thumbnail.Source != "" {
			return stripQuery(p.Thumbnail.Source)
		}
	}
	return ""
}

func stripQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
