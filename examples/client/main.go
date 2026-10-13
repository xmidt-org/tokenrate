// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Command client makes a burst of requests to the example server and prints
// what came back, one line per request, so the limiter's answers can be
// seen: the status, Retry-After when it is refused, and any Capability
// Warning.
//
//	go run ./examples/client -key key-abc -n 7
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/xmidt-org/tokenrate"
)

func main() {
	url := flag.String("url", "http://localhost:8080/", "URL to request")
	key := flag.String("key", "key-abc", "API key to send")
	n := flag.Int("n", 7, "number of requests")
	flag.Parse()

	for i := 1; i <= *n; i++ {
		req, err := http.NewRequest(http.MethodGet, *url, nil)
		if err != nil {
			log.Fatal(err)
		}

		req.Header.Set("X-Api-Key", *key)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Fatal(err)
		}

		resp.Body.Close()

		line := fmt.Sprintf("%2d  %s", i, resp.Status)
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			line += "  Retry-After: " + retry + "s"
		}

		for _, warning := range resp.Header.Values(tokenrate.DefaultWarningHeader) {
			line += "  warning: " + warning
		}

		fmt.Println(line)
	}
}
