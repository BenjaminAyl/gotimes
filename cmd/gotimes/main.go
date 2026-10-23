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
	"syscall"
	"time"

	"github.com/benaylward/gotimes/internal/db"
	"github.com/benaylward/gotimes/internal/feeds"
	"github.com/benaylward/gotimes/internal/server"
	"github.com/benaylward/gotimes/internal/store"
	"github.com/benaylward/gotimes/web"
)

const fetchWorkers = 8

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on the local server and exit")
	flag.Parse()

	addr := env("ADDR", ":4444")
	if *healthcheck {
		os.Exit(probe(addr))
	}

	if err := run(addr); err != nil {
		log.Fatal(err)
	}
}

func run(addr string) error {
	dataDir := env("DATA_DIR", "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	conn, err := db.Open(filepath.Join(dataDir, "gotimes.db"))
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		return err
	}

	st := store.New(conn)
	// ALLOW_PRIVATE_FETCH lets the fetcher reach private addresses (tests only).
	fetcher := feeds.NewFetcher(os.Getenv("ALLOW_PRIVATE_FETCH") == "1")
	sched := feeds.NewScheduler(st, fetcher, fetchWorkers)

	srv, err := server.New(server.Config{DevEmail: os.Getenv("DEV_EMAIL")}, st, fetcher, sched, web.FS)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()

	log.Printf("listening on %s", addr)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

func probe(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
