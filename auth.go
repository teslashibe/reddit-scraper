package redditscraper

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
)

// WithToken sets a pre-existing Reddit session token on the client.
// Get this from your browser's cookies (token_v2 value from reddit.com).
//
// How to get it:
//  1. Log in to reddit.com in your browser
//  2. Open DevTools → Application → Cookies → reddit.com
//  3. Copy the value of "token_v2"
//  4. Pass it here
func (c *client) WithToken(tokenV2 string) {
	jar, _ := cookiejar.New(nil)
	redditURL, _ := url.Parse(c.baseURL)
	jar.SetCookies(redditURL, []*http.Cookie{
		{Name: "token_v2", Value: tokenV2, Domain: ".reddit.com", Path: "/"},
	})
	c.httpClient.Jar = jar
	c.authenticated = true
}

// IsAuthenticated returns whether the client has a valid session.
func (c *client) IsAuthenticated() bool {
	return c.authenticated
}
