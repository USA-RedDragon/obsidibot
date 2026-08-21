// Package pprof serves the runtime profiling endpoints on a listener of their
// own.
//
// It is a SEPARATE LISTENER, and disabled by default, because net/http/pprof
// registers onto http.DefaultServeMux as an import side effect. Anything that
// served DefaultServeMux would publish heap and goroutine dumps — and
// /debug/pprof/cmdline, which prints the process arguments — wherever that
// server was reachable. Here the handlers are wired onto a private mux and the
// operator has to ask for the port.
package pprof

import (
	"context"
	"net/http"
	nethttppprof "net/http/pprof"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/server"
)

// shutdownGrace bounds an in-flight profile once shutdown has started. A CPU
// profile runs 30 seconds by default, so this deliberately does not wait for
// one: a rolling restart should not be held up by a profile nobody is reading.
const shutdownGrace = 5 * time.Second

// Serve runs the pprof endpoints until ctx is cancelled.
func Serve(ctx context.Context, bind string, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", nethttppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", nethttppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", nethttppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", nethttppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", nethttppprof.Trace)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: server.DefaultReadHeaderTimeout,
		// No WriteTimeout: a CPU profile or a trace legitimately streams for
		// its full duration, and a deadline here would truncate it into an
		// unparseable file.
	}
	return server.Serve(ctx, "pprof", srv, bind, port, shutdownGrace)
}
