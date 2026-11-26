package mcp

import (
	"net/url"
	"regexp"
)

var endpointURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

// Redact returns err with the credentials an endpoint URL can carry, its
// user part and its query, removed from the message. Transport errors quote
// the URL they failed on, and these messages reach logs and the model. The
// error chain is kept.
func Redact(err error) error {
	if err == nil {
		return nil
	}
	text := endpointURL.ReplaceAllStringFunc(err.Error(), redactURL)
	if text == err.Error() {
		return err
	}
	return redacted{text: text, err: err}
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[url]"
	}
	query := u.RawQuery != ""
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	if query {
		return u.String() + "?[redacted]"
	}
	return u.String()
}

type redacted struct {
	text string
	err  error
}

func (r redacted) Error() string { return r.text }
func (r redacted) Unwrap() error { return r.err }
