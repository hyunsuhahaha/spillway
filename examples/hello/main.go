package main

import (
	_ "embed"
	"log"
	"net/http"
)

//go:embed index.html
var page []byte

func main() {
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	http.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
