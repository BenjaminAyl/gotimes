package feeds

import (
	"context"
	"log"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/benaylward/gotimes/internal/store"
)

const (
	pollEvery    = 30 * time.Second // how often to look for due feeds
	refreshEvery = 30 * time.Minute // normal per-feed interval
	maxBackoff   = 24 * time.Hour
	batchSize    = 200
)

// Scheduler polls due feeds concurrently with a bounded worker pool.
type Scheduler struct {
	store   *store.Store
	fetcher *Fetcher
	workers int
	kick    chan struct{}
	runMu   sync.Mutex // one sweep at a time, so a manual refresh can't double-fetch
}

func NewScheduler(s *store.Store, f *Fetcher, workers int) *Scheduler {
	return &Scheduler{store: s, fetcher: f, workers: workers, kick: make(chan struct{}, 1)}
}

// Kick asks the scheduler to look for due feeds now (non-blocking).
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// RefreshNow fetches everything that's due and returns when done.
func (s *Scheduler) RefreshNow(ctx context.Context) { s.runOnce(ctx) }

func (s *Scheduler) runOnce(ctx context.Context) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	due, err := s.store.DueFeeds(ctx, time.Now(), batchSize)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("scheduler: list due feeds: %v", err)
		}
		return
	}
	if len(due) == 0 {
		return
	}
	var g errgroup.Group
	g.SetLimit(s.workers)
	for _, st := range due {
		g.Go(func() error {
			s.refresh(ctx, st)
			return nil
		})
	}
	g.Wait()
}

func (s *Scheduler) refresh(ctx context.Context, st store.FeedState) {
	res := s.fetcher.Fetch(ctx, st)
	if ctx.Err() != nil {
		return
	}
	if res.Err != nil {
		log.Printf("feed %d %s: %v", st.ID, st.URL, res.Err)
	}
	if err := s.store.SaveFetch(ctx, res, nextFetch(time.Now(), st.Failures, res.Err != nil)); err != nil {
		log.Printf("feed %d: save: %v", st.ID, err)
	}
}

// nextFetch is the normal interval on success and exponential backoff on failure.
func nextFetch(now time.Time, failures int, failed bool) time.Time {
	if !failed {
		return now.Add(refreshEvery)
	}
	d := refreshEvery
	for i := 0; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return now.Add(min(d, maxBackoff))
}
