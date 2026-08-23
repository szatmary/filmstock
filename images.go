package filmstock

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UserAgent is sent on every request to the Wikimedia APIs. Their policy asks
// clients to identify themselves and to give a way to be contacted, so anyone
// running this at volume should set their own before making requests:
//
//	filmstock.UserAgent = "myapp/1.0 (https://example.com; me@example.com)"
//
// The default carries no personal contact on purpose. A library that shipped one
// would make every program importing it send a stranger's address to Wikimedia,
// and would put that address in every copy of the source.
var UserAgent = "filmstock/0.1 (+https://github.com/szatmary/filmstock)"

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
	req.Header.Set("User-Agent", UserAgent)
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
