// hello is a tiny app to deploy on the platform. It listens on $PORT.
//
//	GET /?work=200     sleep 200 ms (drives concurrency for autoscaling)
//	GET /?cpu=50       burn 50 ms of CPU in computePrimes (shows in profiles)
//	GET /?call=URL     call another service by name inside a stack
//
// With PPROF=1 it also serves Go's profiler at /debug/pprof, which xu can
// collect when the app's profiling path is set to /debug/pprof.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	host, _ := os.Hostname()
	client := &http.Client{Timeout: 5 * time.Second}
	mux := http.NewServeMux()
	if os.Getenv("PPROF") == "1" {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ms, err := strconv.Atoi(r.URL.Query().Get("work")); err == nil && ms > 0 {
			time.Sleep(time.Duration(min(ms, 10_000)) * time.Millisecond)
		}
		found := 0
		if ms, err := strconv.Atoi(r.URL.Query().Get("cpu")); err == nil && ms > 0 {
			found = computePrimes(time.Duration(min(ms, 2000)) * time.Millisecond)
		}
		fmt.Fprintf(w, "hello from %s (pid %d) %s", host, os.Getpid(), os.Getenv("GREETING"))
		if found > 0 {
			fmt.Fprintf(w, " (found %d primes)", found)
		}
		fmt.Fprintln(w)
		// ?call=http://api:8080/ shows one service reaching another by name
		// inside a stack. Demo only: don't ship an open fetcher like this.
		if target := r.URL.Query().Get("call"); target != "" {
			resp, err := client.Get(target)
			if err != nil {
				fmt.Fprintf(w, "  call %s failed: %v\n", target, err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			fmt.Fprintf(w, "  called %s -> %s %s", target, resp.Status, body)
		}
	})
	log.Printf("hello listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// computePrimes keeps a CPU busy for d by trial division.
func computePrimes(d time.Duration) int {
	found, n := 0, 2
	for end := time.Now().Add(d); time.Now().Before(end); n++ {
		if isPrime(n) {
			found++
		}
	}
	return found
}

func isPrime(n int) bool {
	for i := 2; i*i <= n; i++ {
		if n%i == 0 {
			return false
		}
	}
	return n > 1
}
