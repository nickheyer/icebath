package provider

import "net/url"

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }
