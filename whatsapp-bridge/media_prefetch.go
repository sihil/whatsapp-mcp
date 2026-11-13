package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"go.mau.fi/whatsmeow"
)

// Media is downloaded into the cache as soon as it is stored, because the
// signed URLs expire after a few weeks and then only the phone can re-upload
// the file. One pass downloads every cached-able attachment whose URL is still
// valid, soonest expiry first, one at a time; storing new media nudges a pass,
// and a timer catches anything a nudge missed.

const (
	prefetchMaxAttempts = 3
	prefetchInterval    = 10 * time.Minute
	prefetchPause       = time.Second
)

var errNotConnected = errors.New("not connected to WhatsApp")

// mediaFetchFailuresSchema records failed downloads, so neither a restart nor
// a later pass asks WhatsApp again for a file it has said is gone. A record
// applies only to the URL it was made against: a new URL (history sync, or a
// re-upload from the phone) starts afresh.
const mediaFetchFailuresSchema = `
	CREATE TABLE IF NOT EXISTS media_fetch_failures (
		message_id TEXT NOT NULL,
		chat_jid TEXT NOT NULL,
		url TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		gone INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		last_attempt TIMESTAMP,
		PRIMARY KEY (message_id, chat_jid)
	)`

// mediaIsGone reports a download error that will not change on retry: the
// media servers no longer hold the file (or no longer honour its URL).
func mediaIsGone(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

// recordMediaFetch notes the outcome of a download attempt against url: a
// success clears any record, a failure counts an attempt and marks the file
// gone when the error says so.
func (store *MessageStore) recordMediaFetch(id, chatJID, mediaURL string, fetchErr error, at time.Time) error {
	if fetchErr == nil {
		_, err := store.db.Exec("DELETE FROM media_fetch_failures WHERE message_id = ? AND chat_jid = ?", id, chatJID)
		return err
	}
	gone := 0
	if mediaIsGone(fetchErr) {
		gone = 1
	}
	_, err := store.db.Exec(
		`INSERT INTO media_fetch_failures (message_id, chat_jid, url, attempts, gone, last_error, last_attempt)
		 VALUES (?, ?, ?, 1, ?, ?, ?)
		 ON CONFLICT(message_id, chat_jid) DO UPDATE SET
			attempts = CASE WHEN media_fetch_failures.url = excluded.url THEN media_fetch_failures.attempts + 1 ELSE 1 END,
			url = excluded.url, gone = excluded.gone, last_error = excluded.last_error,
			last_attempt = excluded.last_attempt`,
		id, chatJID, mediaURL, gone, fetchErr.Error(), at,
	)
	return err
}

// mediaGone reports whether WhatsApp has said the file behind url is gone.
func (store *MessageStore) mediaGone(id, chatJID, mediaURL string) (bool, error) {
	var gone int
	err := store.db.QueryRow(
		"SELECT gone FROM media_fetch_failures WHERE message_id = ? AND chat_jid = ? AND url = ?",
		id, chatJID, mediaURL,
	).Scan(&gone)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return gone == 1, err
}

type prefetchKey struct{ ID, ChatJID string }

type prefetchCandidate struct {
	prefetchKey
	expires time.Time // zero when the URL carries no expiry
}

// urlExpiry reads the oe parameter (hex Unix seconds) of a signed media URL.
func urlExpiry(mediaURL string) (time.Time, bool) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return time.Time{}, false
	}
	oe, err := strconv.ParseInt(u.Query().Get("oe"), 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(oe, 0), true
}

// prefetchCandidates lists media that is not cached yet and can still be
// downloaded: soonest expiry first, then URLs with no known expiry. A failed
// attachment waits an hour before its second attempt and four before its
// third; without the wait, every newly stored attachment's nudge would retry
// it at once.
func (store *MessageStore) prefetchCandidates(now time.Time) ([]prefetchCandidate, error) {
	rows, err := store.db.Query(
		`SELECT m.id, m.chat_jid, m.filename, m.url FROM messages m
		 LEFT JOIN media_fetch_failures f
			ON f.message_id = m.id AND f.chat_jid = m.chat_jid AND f.url = m.url
		 WHERE m.media_type <> '' AND m.deleted_at IS NULL AND m.url <> '' AND m.media_key IS NOT NULL
			AND (f.message_id IS NULL OR (f.gone = 0 AND f.attempts < ?
				AND julianday(f.last_attempt) <= julianday(?) - (CASE f.attempts WHEN 1 THEN 1 ELSE 4 END) / 24.0))`,
		prefetchMaxAttempts, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cands []prefetchCandidate
	for rows.Next() {
		var c prefetchCandidate
		var filename, mediaURL string
		if err := rows.Scan(&c.ID, &c.ChatJID, &filename, &mediaURL); err != nil {
			return nil, err
		}
		if expires, ok := urlExpiry(mediaURL); ok {
			if !expires.After(now) {
				continue
			}
			c.expires = expires
		}
		if _, err := os.Stat(mediaCachePath(c.ChatJID, c.ID, filename)); err == nil {
			continue
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].expires, cands[j].expires
		if a.IsZero() != b.IsZero() {
			return b.IsZero()
		}
		return a.Before(b)
	})
	return cands, nil
}

type prefetcher struct {
	store *MessageStore
	fetch func(id, chatJID string) error // records its outcome (see downloadMedia)
	pause time.Duration
	now   func() time.Time
}

func newPrefetcher(store *MessageStore, fetch func(id, chatJID string) error, pause time.Duration) *prefetcher {
	return &prefetcher{store: store, fetch: fetch, pause: pause, now: time.Now}
}

// runOnce downloads every current candidate, once each. Failures are recorded
// by the fetch, so a gone file or one that has failed prefetchMaxAttempts
// times drops out of later passes; a lost connection ends the pass.
func (p *prefetcher) runOnce(ctx context.Context) {
	cands, err := p.store.prefetchCandidates(p.now())
	if err != nil {
		fmt.Printf("Media prefetch: listing candidates failed: %v\n", err)
		return
	}
	fetched, failed := 0, 0
	for _, c := range cands {
		if ctx.Err() != nil {
			return
		}
		err := p.fetch(c.ID, c.ChatJID)
		if errors.Is(err, errNotConnected) {
			return
		}
		if err != nil {
			failed++
		} else {
			fetched++
		}
		if p.pause > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.pause):
			}
		}
	}
	if fetched > 0 || failed > 0 {
		fmt.Printf("Media prefetch: downloaded %d attachments, %d failed\n", fetched, failed)
	}
}

func (p *prefetcher) run(ctx context.Context) {
	ticker := time.NewTicker(prefetchInterval)
	defer ticker.Stop()
	for {
		p.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.store.mediaAdded:
		}
	}
}

// nudgeMediaAdded wakes the prefetcher without blocking; a pending nudge
// already covers any number of new attachments.
func (store *MessageStore) nudgeMediaAdded() {
	if store.mediaAdded == nil {
		return
	}
	select {
	case store.mediaAdded <- struct{}{}:
	default:
	}
}

// removeCachedMedia deletes a message's downloaded media, for a message
// deleted for everyone.
func removeCachedMedia(chatJID, messageID string) {
	dir := filepath.Dir(mediaCachePath(chatJID, messageID, "x"))
	prefix := filepath.Base(mediaCachePath(chatJID, messageID, ""))
	matches, _ := filepath.Glob(filepath.Join(dir, globEscape(prefix)+"*"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			fmt.Printf("Failed to remove cached media %s: %v\n", m, err)
		}
	}
}

func globEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '*', '?', '[', '\\':
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}
