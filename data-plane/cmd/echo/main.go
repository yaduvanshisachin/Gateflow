// Command echo is a tiny upstream for trying the gateway by hand. It replies
// with exactly what it received, so you can see what the gateway forwarded.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	name := flag.String("name", "echo", "instance name included in every reply")
	delay := flag.Duration("delay", 0, "artificial response delay, for testing gateway timeouts")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if *delay > 0 {
			time.Sleep(*delay)
		}
		seen := map[string]string{}
		for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Request-Id", "User-Agent"} {
			if v := r.Header.Get(k); v != "" {
				seen[k] = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"upstream": *name,
			"method":   r.Method,
			"host":     r.Host,
			"path":     r.URL.Path,
			"query":    r.URL.RawQuery,
			"headers":  seen,
		})
	})

	log.Printf("echo %q listening on %s", *name, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
