// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Command server is a small HTTP service limited by tokenrate.
//
// It stands in for a real service: a table of API keys plays the part of
// credential validation, and the capabilities each key carries play the part
// of the Token's.  The important part is the order of the middleware: the
// limiter runs only after the key has been validated, so the principal it
// limits by is never the client's to choose.
//
// Run it, then run the client against it:
//
//	go run ./examples/server
//	go run ./examples/client -key key-abc -n 7
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/xmidt-org/tokenrate"
)

// identity is what credential validation establishes about a request.
type identity struct {
	principal    string
	capabilities []string
}

// keys stands in for the credential store.  A real service would verify a
// JWT or look up a session instead.
var keys = map[string]identity{
	"key-abc": {"abc", []string{"prefix:rate:5/1m"}},
	"key-def": {"def", nil},                                               // no rate: Unrestricted
	"key-ghi": {"ghi", []string{"prefix:rate:3/1m", "prefix:rate:10/0s"}}, // one malformed
}

type identityKey struct{}

// authenticate validates the request's credentials and records the identity
// in the context.  Everything after it can trust the identity.
func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := keys[r.Header.Get("X-Api-Key")]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	flag.Parse()

	limiter, err := tokenrate.New(tokenrate.WithPrefixes("prefix:rate:"))
	if err != nil {
		log.Fatal(err)
	}

	hello := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Context().Value(identityKey{}).(identity)
		fmt.Fprintf(w, "hello, %s\n", id.principal)
	})

	limited := tokenrate.Middleware{
		Limiter: limiter,
		Extract: func(r *http.Request) (string, []string) {
			id := r.Context().Value(identityKey{}).(identity)
			return id.principal, id.capabilities
		},
	}.Wrap(hello)

	server := &http.Server{
		Addr:              *addr,
		Handler:           authenticate(limited), // validate credentials first, then limit
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("listening on %s", *addr)
	log.Fatal(server.ListenAndServe())
}
