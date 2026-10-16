package state

import (
	"database/sql"
	"net/url"
)

func openDB(path, mode string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {mode}}
	u.RawQuery = q.Encode()
	return sql.Open("sqlite", u.String())
}
