package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/config"
)

// healthCommand probes this instance's own readiness endpoint.
//
// It exists because the container image has to run a `HEALTHCHECK` and the image
// deliberately does not carry `curl` or `wget`: packaging.md section 7 requires "only
// the binary, the runner agent where applicable, the Git executable and its runtime
// dependencies, and CA certificates", and a package manager cache is explicitly out.
// So the probe is the binary itself rather than an added package.
//
// The default endpoint is `/api/health` — what the packaging document names for the
// container health check — and `--ready` switches to `/api/ready`, which is what a
// scheduler or load balancer should gate on, because it reports whether the database
// is reachable rather than whether the process is alive.
//
// The exit status is the whole interface: 0 when the endpoint answered 200, 1 when it
// answered anything else or could not be reached. A container runtime reads nothing
// else, and neither should a script.
func healthCommand(ctx context.Context, args []string, streams stdio) error {
	flags := flag.NewFlagSet("health", flag.ContinueOnError)
	flags.SetOutput(streams.err)
	url := flags.String("url", "", "base URL to probe; defaults to the configured listener")
	ready := flags.Bool("ready", false, "probe /api/ready instead of /api/health")
	timeout := flags.Duration("timeout", 5*time.Second, "how long to wait for an answer")
	flags.Usage = func() {
		fmt.Fprintln(streams.err, "usage: gitnest health [--ready] [--url http://host:port] [--timeout 5s]")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "Probes this instance's own HTTP endpoint and exits 0 when it answers 200.")
		fmt.Fprintln(streams.err, "--ready probes /api/ready, which reports database availability and is what a")
		fmt.Fprintln(streams.err, "scheduler should gate on. The default probes /api/health.")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("health takes no positional arguments, got %q", flags.Arg(0))
	}
	if *timeout <= 0 {
		return errors.New("the timeout must be positive")
	}

	base := strings.TrimRight(strings.TrimSpace(*url), "/")
	if base == "" {
		// The listener address comes from configuration, so the probe follows a
		// deployment that bound somewhere else without an operator restating it.
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		base = defaultProbeURL(cfg)
	}

	path := "/api/health"
	if *ready {
		path = "/api/ready"
	}

	probeCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("build the probe request: %w", err)
	}
	// The probe is discarded, so the response body must be closed without being read
	// beyond a bounded amount: an endpoint that streams would otherwise be able to hold
	// the probe open.
	response, err := (&http.Client{Timeout: *timeout}).Do(request)
	if err != nil {
		return fmt.Errorf("probe %s: %w", base+path, err)
	}
	defer func() { _ = response.Body.Close() }()
	// One line of the body is read, so a container log shows whatever the endpoint said
	// when it refused — which is the difference between "unhealthy" and knowing why.
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))

	if response.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if detail != "" {
			return fmt.Errorf("probe %s returned %d: %s", base+path, response.StatusCode, detail)
		}
		return fmt.Errorf("probe %s returned %d", base+path, response.StatusCode)
	}
	fmt.Fprintf(streams.out, "%s is healthy\n", base+path)
	return nil
}

// defaultProbeURL builds the probe URL from the configured listener.
//
// A listener bound to every interface is probed through the loopback address: a
// wildcard address is not connectable, and a probe that failed to parse `0.0.0.0` would
// report an instance unhealthy because of its bind address rather than its state. The
// scheme is always HTTP, because the configured listener speaks plain HTTP and TLS is
// the reverse proxy's job — which is what packaging.md section 10 says.
func defaultProbeURL(cfg config.Config) string {
	host := cfg.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		// An IPv6 literal needs brackets to be a valid host in a URL.
		host = "[" + host + "]"
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(cfg.Port))
}
