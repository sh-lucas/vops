// Test app for e2e tests: an http server that answers $MSG, plus a few subcommands.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 2 && os.Args[1] == "lookup" {
		addrs, err := net.LookupHost(os.Args[2])
		fmt.Println(addrs, err)
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "get" {
		resp, err := http.Get(os.Args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		b := make([]byte, 4096)
		n, _ := resp.Body.Read(b)
		fmt.Print(string(b[:n]))
		return
	}
	if len(os.Args) > 3 && os.Args[1] == "write" {
		if err := os.WriteFile(os.Args[2], []byte(os.Args[3]), 0o644); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "read" {
		b, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Print(string(b))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "exit" {
		os.Exit(3)
	}
	// READY_AFTER delays readiness: /health answers 503 until then. FAIL_AFTER: 500 from then on.
	start := time.Now()
	delay, _ := time.ParseDuration(os.Getenv("READY_AFTER"))
	fail, _ := time.ParseDuration(os.Getenv("FAIL_AFTER"))
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < delay || os.Getenv("NEVER_READY") != "" {
			w.WriteHeader(503)
			return
		}
		if fail > 0 && time.Since(start) > fail {
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv("MSG"))
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("listening on", port)
	http.ListenAndServe(":"+port, nil)
}
