// Command model-router is the high-performance Go backend for Model Router.
//
// It is a drop-in alternative to the Python backend: the same data/ layout, the same
// config.yaml, the same REST API, and byte-compatible trace records, so the two can take turns
// serving one deployment. Pick between them with the repository's ./run.sh, or run this binary
// directly.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/satomic/model-router/server-go/internal/aicredits"
	"github.com/satomic/model-router/server-go/internal/auth"
	"github.com/satomic/model-router/server-go/internal/authstore"
	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/release"
	"github.com/satomic/model-router/server-go/internal/server"
	"github.com/satomic/model-router/server-go/internal/traces"
	"github.com/satomic/model-router/server-go/internal/usagestats"
	"github.com/satomic/model-router/server-go/internal/version"
)

func main() {
	host := flag.String("host", envOr("MR_HOST", "0.0.0.0"), "address to listen on")
	port := flag.String("port", envOr("MR_PORT", "8000"), "port to listen on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe a running instance and exit 0 when it is healthy")
	resolve := flag.String("resolve", "", "resolve a hostname with this process's own resolver and exit")
	// Named after uvicorn's flag, because that is what the Python backend's documented
	// reverse-proxy command passes and the migration should be a rename rather than a rethink.
	forwarded := flag.String("forwarded-allow-ips", envOr("MR_FORWARDED_ALLOW_IPS", ""),
		"comma-separated proxy addresses allowed to set X-Forwarded-* ('*' trusts every peer; "+
			"default: loopback only)")
	flag.Parse()

	if *showVersion {
		log.SetFlags(0)
		log.Printf("model-router %s (go backend)", version.Version)
		return
	}

	if *healthcheck {
		// The container health check runs this same binary, so it does not depend on the image
		// carrying a tool to probe with.
		probe(*port)
		return
	}

	if *resolve != "" {
		// A shell in the container resolves through musl; this process resolves through Go's own
		// resolver, and when an upstream fails with "no such host" the two do not always agree.
		// This makes the answer that actually matters observable from inside the container.
		resolveAndReport(*resolve)
		return
	}

	log.SetFlags(log.LstdFlags)
	// stdout, not Go's default stderr: uvicorn writes the Python backend's log there, a log
	// collector that splits the two streams would file every ordinary request line as an error,
	// and `docker logs` shows both anyway.
	log.SetOutput(os.Stdout)

	// Before the seeding check, never after: this is what stops an upgrade from finding the new
	// data/config.yaml empty and quietly starting from the template with every credential gone.
	for _, moved := range config.MigrateLegacyLayout() {
		log.Printf("INFO mr: moved existing state under data/: %s", moved)
	}
	created, err := config.EnsureConfigFile()
	if err != nil {
		log.Fatalf("ERROR mr: %v", err)
	}
	if created {
		// Worth a line at INFO: on a fresh volume this is the difference between "my settings are
		// gone" and "this deployment started from the template", and the operator needs to know
		// the local-admin password change is waiting for them.
		log.Printf("INFO mr: created %s from %s -- sign in as the local administrator to configure it",
			config.ConfigPath, config.TemplatePath)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("ERROR mr: could not load the configuration: %v", err)
	}

	// Which peers may dictate the callback origin and the cookie's Secure flag. Set before the
	// first request can arrive.
	if *forwarded != "" {
		auth.SetTrustedProxies(strings.Split(*forwarded, ","))
	}

	// Every package that persists state under data/ is bound to the resolved directory here,
	// rather than each resolving it again -- one place decides where state lives.
	ghcache.Init(config.DataDir)
	usagestats.Init(config.DataDir)
	aicredits.Init(config.DataDir)
	release.Init(config.DataDir)

	traceStore := traces.New(filepath.Join(config.LogDir, "traces"), 500)
	store := authstore.New(config.DataDir)
	app := server.New(cfg, traceStore, store, filepath.Join(config.Root, "frontend", "dist"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.StartLoops(ctx)

	address := *host + ":" + *port
	httpServer := &http.Server{
		Addr:    address,
		Handler: app.Handler(),
		// No write deadline: a streamed answer is written for as long as the model keeps
		// generating, and a blanket timeout would cut long answers off mid-sentence. The read
		// header timeout still bounds a client that opens a connection and says nothing.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("INFO mr: model-router %s (go) listening on http://%s (X-Forwarded-* trusted from %s)",
			version.Version, address, auth.TrustedProxyDescription())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ERROR mr: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("INFO mr: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("WARNING mr: shutdown: %v", err)
	}
}

// resolveAndReport looks a hostname up the way the server does and prints what it found.
//
// Deliberately the plain net.LookupHost the HTTP client ends up calling, rather than a resolver
// configured specially for the occasion: a diagnostic that resolves differently from the thing
// being diagnosed is worse than none.
func resolveAndReport(host string) {
	log.SetFlags(0)
	// stdout, so `--resolve host | grep ...` works and the diagnostic script does not have to
	// redirect to see the answer.
	log.SetOutput(os.Stdout)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	started := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	elapsed := time.Since(started).Round(time.Millisecond)
	if err != nil {
		log.Printf("Go resolver: FAILED after %s: %v", elapsed, err)
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			log.Printf("  not found: %v   timeout: %v   server: %q",
				dnsErr.IsNotFound, dnsErr.IsTimeout, dnsErr.Server)
		}
		// Non-zero, so the diagnostic script can branch on it.
		os.Exit(1)
	}
	log.Printf("Go resolver: %s -> %s (in %s)", host, strings.Join(addrs, ", "), elapsed)
}

// probe asks a locally running instance for /healthz and exits non-zero unless it answers 200.
func probe(port string) {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		log.SetFlags(0)
		log.Printf("unhealthy: %v", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.SetFlags(0)
		log.Printf("unhealthy: /healthz returned %d", resp.StatusCode)
		os.Exit(1)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
