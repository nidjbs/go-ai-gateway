// Package sample holds a small Go file with several reviewable defects.
package sample

import (
	"fmt"
	"io"
	"net/http"
)

// FetchPage returns the body of the given URL.
func FetchPage(url string) string {
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return "" // swallows the error, caller sees an empty string
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Println("bad status:", resp.StatusCode)
		return ""
	}

	data, _ := io.ReadAll(resp.Body)
	return string(data)
}
