// Package queue implements a disk-backed outbound mail queue: each accepted
// submission gets spooled as plain files (RFC 822 bytes + JSON metadata) and
// retried with backoff until it's relayed or permanently fails.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sociolytik/mailserver/internal/metrics"
)

// Item is one queued (message, recipient) pair. A submission with multiple
// non-local recipients is split into one Item per recipient, since each may
// hit a different remote MTA on a different retry schedule.
type Item struct {
	ID          string    `json:"id"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Queue is an outbound relay queue rooted at Dir.
type Queue struct {
	Dir            string
	Hostname       string
	RetryIntervals []time.Duration
	MaxRetries     int
	Logger         *slog.Logger

	// OnPermanentFailure is called (if set) when a message is given up on,
	// either because the remote MTA rejected it outright (5xx) or retries
	// were exhausted. Typically used to bounce the message back to the
	// original sender's own mailbox.
	OnPermanentFailure func(item Item, data []byte, reason string)

	// SmartHost is called (if set) before every delivery attempt to check
	// whether outbound mail should go through a configured relay instead
	// of direct-to-MX. Queried live rather than cached, so an admin
	// enabling/disabling it takes effect on the next delivery attempt
	// without a restart.
	SmartHost func(ctx context.Context) (cfg *SmartHostConfig, enabled bool, err error)
}

func New(dir, hostname string, retryIntervals []time.Duration, maxRetries int, logger *slog.Logger) (*Queue, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating queue directory: %w", err)
	}
	return &Queue{
		Dir:            dir,
		Hostname:       hostname,
		RetryIntervals: retryIntervals,
		MaxRetries:     maxRetries,
		Logger:         logger,
	}, nil
}

// Enqueue persists a message for relay to a single recipient and returns its
// queue ID.
func (q *Queue) Enqueue(from, to string, data []byte) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(q.Dir, id+".eml"), data, 0600); err != nil {
		return "", fmt.Errorf("writing queued message: %w", err)
	}

	item := Item{ID: id, From: from, To: to, CreatedAt: time.Now(), NextAttempt: time.Now()}
	if err := q.save(item); err != nil {
		return "", err
	}
	return id, nil
}

func (q *Queue) save(item Item) error {
	buf, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(q.Dir, item.ID+".json"), buf, 0600)
}

func (q *Queue) load(id string) (Item, error) {
	buf, err := os.ReadFile(filepath.Join(q.Dir, id+".json"))
	if err != nil {
		return Item{}, err
	}
	var item Item
	err = json.Unmarshal(buf, &item)
	return item, err
}

func (q *Queue) remove(id string) {
	_ = os.Remove(filepath.Join(q.Dir, id+".eml"))
	_ = os.Remove(filepath.Join(q.Dir, id+".json"))
}

// Run processes due queue items every interval until ctx is canceled.
func (q *Queue) Run(ctx context.Context, interval time.Duration) {
	q.processAll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.processAll()
		}
	}
}

func (q *Queue) processAll() {
	entries, err := os.ReadDir(q.Dir)
	if err != nil {
		q.Logger.Error("reading queue directory", "error", err)
		return
	}

	depth := 0
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		depth++
		id := strings.TrimSuffix(e.Name(), ".json")

		item, err := q.load(id)
		if err != nil {
			q.Logger.Error("loading queue item", "id", id, "error", err)
			continue
		}
		if item.NextAttempt.After(now) {
			continue
		}
		q.attempt(item)
	}
	metrics.QueueDepth.Set(float64(depth))
}

func (q *Queue) attempt(item Item) {
	data, err := os.ReadFile(filepath.Join(q.Dir, item.ID+".eml"))
	if err != nil {
		q.Logger.Error("reading queued message body", "id", item.ID, "error", err)
		q.remove(item.ID)
		return
	}

	deliverErr := q.deliver(item.From, item.To, data)
	if deliverErr == nil {
		q.Logger.Info("relayed message", "id", item.ID, "to", item.To, "attempts", item.Attempts+1)
		metrics.RelayAttempts.WithLabelValues("success").Inc()
		q.remove(item.ID)
		return
	}

	item.Attempts++
	item.LastError = deliverErr.Error()

	if isPermanent(deliverErr) || item.Attempts > q.MaxRetries {
		q.Logger.Warn("giving up relaying message", "id", item.ID, "to", item.To, "attempts", item.Attempts, "error", deliverErr)
		metrics.RelayAttempts.WithLabelValues("perm_fail").Inc()
		if q.OnPermanentFailure != nil {
			q.OnPermanentFailure(item, data, deliverErr.Error())
		}
		q.remove(item.ID)
		return
	}
	metrics.RelayAttempts.WithLabelValues("temp_fail").Inc()

	delay := q.RetryIntervals[len(q.RetryIntervals)-1]
	if item.Attempts-1 < len(q.RetryIntervals) {
		delay = q.RetryIntervals[item.Attempts-1]
	}
	item.NextAttempt = time.Now().Add(delay)

	q.Logger.Warn("temporary failure relaying message, will retry",
		"id", item.ID, "to", item.To, "attempts", item.Attempts, "next_attempt", item.NextAttempt, "error", deliverErr)
	if err := q.save(item); err != nil {
		q.Logger.Error("saving queue item", "id", item.ID, "error", err)
	}
}

func generateID() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(buf[:])), nil
}
