package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev" // set via -ldflags at build time

// maxWait caps the long-poll duration on /dequeue. It must stay below the
// server WriteTimeout or in-flight polls would be cut off mid-response.
const maxWait = 20 * time.Second

// --------------------------- configuration ----------------------------------

type config struct {
	addr             string
	tcpAddr          string
	tlsCert          string
	tlsKey           string
	maxBytes         int64
	maxMsgs          int
	maxMsgBytes      int64
	maxQueues        int
	maxWaiters       int64
	maxLease         time.Duration
	dataDir          string
	tokens           tokens
	logLevel         slog.Level
	snapshotFile     string
	snapshotInterval time.Duration
	pprof            bool
	healthcheck      bool
}

func parseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	var n int64
	var err error
	switch {
	case strings.HasSuffix(u, "G"):
		n, err = strconv.ParseInt(strings.TrimSuffix(u, "G"), 10, 64)
		n <<= 30
	case strings.HasSuffix(u, "M"):
		n, err = strconv.ParseInt(strings.TrimSuffix(u, "M"), 10, 64)
		n <<= 20
	case strings.HasSuffix(u, "K"):
		n, err = strconv.ParseInt(strings.TrimSuffix(u, "K"), 10, 64)
		n <<= 10
	default:
		n, err = strconv.ParseInt(u, 10, 64)
	}
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("size must not be negative: %s", s)
	}
	return n, nil
}

// envOr returns the flag value, falling back to the environment variable.
func envOr(flagVal, env string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(env)
}

// loadConfig resolves settings with precedence: flag > environment > default.
func loadConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("zapq", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "HTTP listen address")
	tcpAddr := fs.String("tcp-addr", "", "binary TCP protocol listen address, e.g. :6789 (disabled when empty)")
	cert := fs.String("tls-cert", "", "TLS certificate file (optional)")
	key := fs.String("tls-key", "", "TLS key file (optional)")
	maxBytesFlag := fs.String("max-bytes", "", "per-queue max bytes, e.g. 2G, 512M (default 256M, env QUEUE_MAX_BYTES)")
	maxMsgsFlag := fs.Int("max-msgs", 0, "per-queue max messages (default 50000, env QUEUE_MAX_MSGS)")
	maxMsgBytesFlag := fs.String("max-msg-bytes", "", "per-message size cap, e.g. 128K, 1M (default 128K, env ZAPQ_MAX_MSG_BYTES)")
	maxQueuesFlag := fs.Int("max-queues", 64, "maximum number of named queues")
	maxWaitersFlag := fs.Int64("max-waiters", 1024, "maximum concurrent long-poll (wait=) requests")
	maxLeaseFlag := fs.Duration("max-lease", 5*time.Minute, "maximum lease duration for dequeue leases")
	dataDir := fs.String("data-dir", ".", "directory for persist/load operations and snapshots")
	apiToken := fs.String("api-token", "", "bearer token with full access (env ZAPQ_API_TOKEN)")
	producerToken := fs.String("producer-token", "", "bearer token limited to enqueue + metrics (env ZAPQ_PRODUCER_TOKEN)")
	consumerToken := fs.String("consumer-token", "", "bearer token limited to dequeue/ack + metrics (env ZAPQ_CONSUMER_TOKEN)")
	adminToken := fs.String("admin-token", "", "bearer token with full access incl. clear/persist/load (env ZAPQ_ADMIN_TOKEN)")
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, error (default info, env ZAPQ_LOG_LEVEL)")
	snapFile := fs.String("snapshot-file", "", "snapshot path; load on start, persist on shutdown (env ZAPQ_SNAPSHOT_FILE)")
	snapInterval := fs.Duration("snapshot-interval", 0, "persist snapshot periodically, e.g. 30s; requires --snapshot-file (env ZAPQ_SNAPSHOT_INTERVAL)")
	pprofFlag := fs.Bool("pprof", false, "expose /debug/pprof endpoints (admin access class)")
	healthFlag := fs.Bool("healthcheck", false, "probe the local server's /health and exit 0/1 (for container healthchecks)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg := config{
		addr:        *addr,
		tcpAddr:     *tcpAddr,
		tlsCert:     *cert,
		tlsKey:      *key,
		maxBytes:    256 << 20,
		maxMsgs:     50000,
		maxMsgBytes: 128 << 10,
		maxQueues:   *maxQueuesFlag,
		maxWaiters:  *maxWaitersFlag,
		maxLease:    *maxLeaseFlag,
		logLevel:    slog.LevelInfo,
		pprof:       *pprofFlag,
		healthcheck: *healthFlag,
		tokens: tokens{
			api:      envOr(*apiToken, "ZAPQ_API_TOKEN"),
			producer: envOr(*producerToken, "ZAPQ_PRODUCER_TOKEN"),
			consumer: envOr(*consumerToken, "ZAPQ_CONSUMER_TOKEN"),
			admin:    envOr(*adminToken, "ZAPQ_ADMIN_TOKEN"),
		},
	}

	sizeVal := func(flagVal, env string, dst *int64) error {
		v := envOr(flagVal, env)
		if v == "" {
			return nil
		}
		n, err := parseSize(v)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", env, err)
		}
		*dst = n
		return nil
	}
	if err := sizeVal(*maxBytesFlag, "QUEUE_MAX_BYTES", &cfg.maxBytes); err != nil {
		return config{}, err
	}
	if err := sizeVal(*maxMsgBytesFlag, "ZAPQ_MAX_MSG_BYTES", &cfg.maxMsgBytes); err != nil {
		return config{}, err
	}

	if *maxMsgsFlag != 0 {
		cfg.maxMsgs = *maxMsgsFlag
	} else if env := os.Getenv("QUEUE_MAX_MSGS"); env != "" {
		m, err := strconv.Atoi(env)
		if err != nil {
			return config{}, fmt.Errorf("invalid QUEUE_MAX_MSGS: %w", err)
		}
		cfg.maxMsgs = m
	}

	if lv := envOr(*logLevel, "ZAPQ_LOG_LEVEL"); lv != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(lv)); err != nil {
			return config{}, fmt.Errorf("invalid log level %q", lv)
		}
	}

	cfg.snapshotFile = envOr(*snapFile, "ZAPQ_SNAPSHOT_FILE")
	cfg.snapshotInterval = *snapInterval
	if cfg.snapshotInterval == 0 {
		if env := os.Getenv("ZAPQ_SNAPSHOT_INTERVAL"); env != "" {
			d, err := time.ParseDuration(env)
			if err != nil {
				return config{}, fmt.Errorf("invalid ZAPQ_SNAPSHOT_INTERVAL: %w", err)
			}
			cfg.snapshotInterval = d
		}
	}

	var err error
	cfg.dataDir, err = filepath.Abs(*dataDir)
	if err != nil {
		return config{}, fmt.Errorf("invalid data-dir: %w", err)
	}
	if cfg.snapshotFile != "" && !filepath.IsAbs(cfg.snapshotFile) {
		cfg.snapshotFile = filepath.Join(cfg.dataDir, cfg.snapshotFile)
	}

	switch {
	case cfg.maxBytes < 1:
		return config{}, errors.New("max-bytes must be at least 1")
	case cfg.maxMsgs < 1:
		return config{}, errors.New("max-msgs must be at least 1")
	case cfg.maxMsgBytes < 1:
		return config{}, errors.New("max-msg-bytes must be at least 1")
	case cfg.maxMsgBytes > cfg.maxBytes:
		return config{}, errors.New("max-msg-bytes cannot exceed max-bytes")
	case cfg.maxQueues < 1:
		return config{}, errors.New("max-queues must be at least 1")
	case cfg.maxWaiters < 1:
		return config{}, errors.New("max-waiters must be at least 1")
	case cfg.maxLease < time.Second:
		return config{}, errors.New("max-lease must be at least 1s")
	case cfg.snapshotInterval < 0:
		return config{}, errors.New("snapshot-interval must not be negative")
	case cfg.snapshotInterval > 0 && cfg.snapshotFile == "":
		return config{}, errors.New("snapshot-interval requires snapshot-file")
	case (cfg.tlsCert == "") != (cfg.tlsKey == ""):
		return config{}, errors.New("tls-cert and tls-key must be set together")
	}
	return cfg, nil
}

// --------------------------- healthcheck mode --------------------------------

// runHealthcheck probes the local server, for HEALTHCHECK in images without
// a shell. Exit status is the only output contract.
func runHealthcheck(cfg config) int {
	addr := cfg.addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if cfg.tlsCert != "" {
		scheme = "https"
		// self-probe over loopback; the cert may be for an external name
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	resp, err := client.Get(scheme + "://" + addr + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: status", resp.StatusCode)
		return 1
	}
	return 0
}

// --------------------------- main -------------------------------------------

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "zapq:", err)
		os.Exit(2)
	}
	if cfg.healthcheck {
		os.Exit(runHealthcheck(cfg))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		logger.Error("cannot create data-dir", "dir", cfg.dataDir, "error", err.Error())
		os.Exit(1)
	}

	if gomem := os.Getenv("GOMEMLIMIT"); gomem == "" {
		logger.Warn("GOMEMLIMIT not set; consider setting it", "suggest_bytes", cfg.maxBytes*2)
	} else {
		logger.Info("GOMEMLIMIT detected", "value", gomem)
	}

	reg := newRegistry(cfg.maxBytes, cfg.maxMsgBytes, cfg.maxMsgs, cfg.maxQueues)

	if cfg.snapshotFile != "" {
		switch _, err := os.Stat(cfg.snapshotFile); {
		case err == nil:
			if err := reg.load(cfg.snapshotFile); err != nil {
				logger.Error("snapshot restore failed; remove or fix the file to start",
					"file", cfg.snapshotFile, "error", err.Error())
				os.Exit(1)
			}
			logger.Info("snapshot restored", "file", cfg.snapshotFile)
		case errors.Is(err, os.ErrNotExist):
			logger.Info("no snapshot found, starting empty", "file", cfg.snapshotFile)
		default:
			logger.Error("cannot stat snapshot file", "file", cfg.snapshotFile, "error", err.Error())
			os.Exit(1)
		}
	}

	s := &server{
		reg:        reg,
		tokens:     cfg.tokens,
		log:        logger,
		startTime:  time.Now(),
		dataDir:    cfg.dataDir,
		maxWait:    maxWait,
		maxLease:   cfg.maxLease,
		maxWaiters: cfg.maxWaiters,
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           s.routes(cfg.pprof),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bgWG sync.WaitGroup

	// lease reaper: requeue expired leases every second
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if n := reg.expireAll(time.Now()); n > 0 {
					logger.Debug("leases expired and requeued", "count", n)
				}
			case <-bgCtx.Done():
				return
			}
		}
	}()

	if cfg.snapshotInterval > 0 {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			t := time.NewTicker(cfg.snapshotInterval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := reg.persist(cfg.snapshotFile); err != nil {
						logger.Error("periodic snapshot failed", "file", cfg.snapshotFile, "error", err.Error())
					} else {
						logger.Debug("periodic snapshot written", "file", cfg.snapshotFile)
					}
				case <-bgCtx.Done():
					return
				}
			}
		}()
	}

	var tcpSrv *tcpServer
	if cfg.tcpAddr != "" {
		var tlsConf *tls.Config
		if cfg.tlsCert != "" {
			certPair, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
			if err != nil {
				logger.Error("cannot load TLS keypair for tcp listener", "error", err.Error())
				os.Exit(1)
			}
			tlsConf = &tls.Config{Certificates: []tls.Certificate{certPair}}
		}
		var err error
		tcpSrv, err = newTCPServer(s, cfg.tcpAddr, tlsConf)
		if err != nil {
			logger.Error("tcp listen failed", "addr", cfg.tcpAddr, "error", err.Error())
			os.Exit(1)
		}
		go tcpSrv.serve()
	}

	logger.Info("queue listening",
		"addr", cfg.addr,
		"tcp_addr", cfg.tcpAddr,
		"tls", cfg.tlsCert != "",
		"auth", cfg.tokens.enabled(),
		"max_bytes", cfg.maxBytes,
		"max_msgs", cfg.maxMsgs,
		"max_msg_bytes", cfg.maxMsgBytes,
		"max_queues", cfg.maxQueues,
		"data_dir", cfg.dataDir,
		"snapshot_file", cfg.snapshotFile,
		"version", version,
	)

	go func() {
		var err error
		if cfg.tlsCert != "" {
			err = srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("listen failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("shutdown signal received", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err.Error())
	}
	if tcpSrv != nil {
		tcpSrv.shutdown(10 * time.Second)
	}

	bgCancel()
	bgWG.Wait()
	if cfg.snapshotFile != "" {
		if err := reg.persist(cfg.snapshotFile); err != nil {
			logger.Error("final snapshot failed", "file", cfg.snapshotFile, "error", err.Error())
			os.Exit(1)
		}
		logger.Info("final snapshot written", "file", cfg.snapshotFile)
	}
	logger.Info("bye")
}
