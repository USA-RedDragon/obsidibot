// Package server runs an http.Server until its context is cancelled.
//
// obsidibot has four listeners with deliberately different exposure —
// interactions, ingest, metrics and pprof — and every one of them needs the
// same listen, serve and drain behaviour. This is that behaviour, once.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// DefaultReadHeaderTimeout bounds how long a client may take to send its
// headers. Every listener wants it; none of them wants a slow-loris.
const DefaultReadHeaderTimeout = 5 * time.Second

// Listen opens listeners for bind:port.
//
// A wildcard bind takes one listener per family rather than relying on a single
// dual-stack socket: Go sets IPV6_V6ONLY on a "tcp6" listener, so the two can
// share a port, and IPv4 reachability stops depending on the host's
// net.ipv6.bindv6only setting.
//
// Both families are attempted, but only one has to succeed. A host with a stack
// disabled still gets a working service on the other, with a warning naming
// what was lost. Note that this treats a port already taken on one family the
// same as a missing stack, so it can degrade to half-reachable instead of
// reporting the conflict.
func Listen(ctx context.Context, bind string, port int) ([]net.Listener, error) {
	var lc net.ListenConfig
	service := strconv.Itoa(port)

	if bind != "" && bind != "*" {
		// An explicit address already picks its own family, and there is no
		// second one to fall back to.
		addr := net.JoinHostPort(bind, service)
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		return []net.Listener{ln}, nil
	}

	families := []struct{ network, host string }{
		{"tcp4", "0.0.0.0"},
		{"tcp6", "::"},
	}
	listeners := make([]net.Listener, 0, len(families))
	errs := make([]error, 0, len(families))

	for _, family := range families {
		ln, err := lc.Listen(ctx, family.network, net.JoinHostPort(family.host, service))
		if err != nil {
			slog.WarnContext(ctx, "could not listen on this address family, continuing without it",
				"network", family.network, "port", port, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", family.network, err))
			continue
		}
		listeners = append(listeners, ln)
	}

	if len(listeners) == 0 {
		return nil, fmt.Errorf("listen on port %d: %w", port, errors.Join(errs...))
	}
	return listeners, nil
}

// Serve runs server on bind:port until ctx is cancelled, then drains in-flight
// requests for grace before returning.
//
// name appears in the log lines and in any error, so an operator reading
// "listening" or a startup failure can tell which of the four listeners it was.
func Serve(ctx context.Context, name string, server *http.Server, bind string, port int, grace time.Duration) error {
	listeners, err := Listen(ctx, bind, port)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	// Buffered for every listener so each goroutine can exit even when
	// shutdown wins the race below.
	serveErr := make(chan error, len(listeners))
	for _, ln := range listeners {
		slog.InfoContext(ctx, "listening", "listener", name, "addr", ln.Addr().String(), "network", ln.Addr().Network())
		go func(ln net.Listener) {
			err := server.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			serveErr <- err
		}(ln)
	}

	select {
	case err := <-serveErr:
		// Failing to open a family is tolerated above, but a listener that
		// dies after it started accepting is unexpected, so give up entirely
		// and let the supervisor restart us. Close rather than leaking the
		// sibling listener, which would otherwise keep accepting into a
		// process that is on its way out.
		if cerr := server.Close(); cerr != nil {
			slog.DebugContext(ctx, "close after serve error", "listener", name, "error", cerr)
		}
		if err != nil {
			return fmt.Errorf("%s: serve: %w", name, err)
		}
		return nil
	case <-ctx.Done():
	}

	slog.InfoContext(ctx, "shutting down", "listener", name, "grace", grace)

	// WithoutCancel keeps ctx's values but drops its cancellation: ctx has
	// already fired, and the point of this context is to give open requests
	// time to land rather than to cut them off.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	if err := server.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("%s: graceful shutdown: %w", name, err)
	}
	// Collect from every listener so none is left mid-flight when we return.
	for range listeners {
		if err := <-serveErr; err != nil {
			return fmt.Errorf("%s: serve: %w", name, err)
		}
	}

	slog.InfoContext(ctx, "stopped", "listener", name)
	return nil
}
