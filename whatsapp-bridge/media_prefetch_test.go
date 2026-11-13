package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

var prefetchNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// mediaURL builds a signed URL expiring at the given time.
func mediaURL(expires time.Time) string {
	return fmt.Sprintf("https://mmg.whatsapp.net/v/t62/x.enc?ccb=11-4&oh=01_sig&oe=%X&_nc_sid=5e03e0", expires.Unix())
}

func putMedia(t *testing.T, s *MessageStore, id, mediaURLValue string) {
	t.Helper()
	must(t, s.StoreChat(tChatGrp, "", t0))
	must(t, s.StoreMessage(id, tChatGrp, tPeer, "", t0, false, mediaInfo{
		Type: "image", Filename: id + ".jpg", URL: mediaURLValue,
		MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: 9,
	}))
}

func candidateIDs(t *testing.T, s *MessageStore) []string {
	t.Helper()
	cands, err := s.prefetchCandidates(prefetchNow)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestURLExpiry(t *testing.T) {
	want := time.Unix(0x6B150DBA, 0)
	got, ok := urlExpiry(signedURL)
	if !ok || !got.Equal(want) {
		t.Errorf("urlExpiry = %v %v, want %v", got, ok, want)
	}
	if _, ok := urlExpiry("https://mmg.whatsapp.net/v/x.enc"); ok {
		t.Error("a URL without oe reported an expiry")
	}
}

func TestPrefetchCandidatesSoonestExpiryFirst(t *testing.T) {
	s := newMutationStore(t)
	putMedia(t, s, "LATER", mediaURL(prefetchNow.Add(10*24*time.Hour)))
	putMedia(t, s, "SOON", mediaURL(prefetchNow.Add(time.Hour)))
	putMedia(t, s, "NOEXPIRY", "https://mmg.whatsapp.net/v/x.enc")
	got := candidateIDs(t, s)
	want := []string{"SOON", "LATER", "NOEXPIRY"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func TestPrefetchCandidatesSkipExpiredDeletedTextAndCached(t *testing.T) {
	s := newMutationStore(t)
	putMedia(t, s, "EXPIRED", mediaURL(prefetchNow.Add(-time.Hour)))
	putMedia(t, s, "DELETED", mediaURL(prefetchNow.Add(time.Hour)))
	if _, err := s.ApplyRevoke(tChatGrp, "DELETED", tPeer, nil, t0); err != nil {
		t.Fatal(err)
	}
	putMsg(t, s, "TEXT", tChatGrp, tPeer, "hello", "", false)
	putMedia(t, s, "CACHED", mediaURL(prefetchNow.Add(time.Hour)))
	cached := mediaCachePath(tChatGrp, "CACHED", "CACHED.jpg")
	must(t, os.MkdirAll(filepath.Dir(cached), 0700))
	must(t, os.WriteFile(cached, []byte("x"), 0600))
	putMedia(t, s, "WANTED", mediaURL(prefetchNow.Add(time.Hour)))

	if got := candidateIDs(t, s); fmt.Sprint(got) != "[WANTED]" {
		t.Errorf("candidates = %v, want [WANTED]", got)
	}
}

// fakeFetch records results the way downloadMedia does.
func fakeFetch(s *MessageStore, calls *[]string, errFor map[string]error) func(id, chat string) error {
	return func(id, chat string) error {
		*calls = append(*calls, id)
		var url string
		if err := s.db.QueryRow("SELECT url FROM messages WHERE id = ? AND chat_jid = ?", id, chat).Scan(&url); err != nil {
			panic(err)
		}
		err := errFor[id]
		if rerr := s.recordMediaFetch(id, chat, url, err, prefetchNow); rerr != nil {
			panic(rerr)
		}
		return err
	}
}

func count(calls []string, id string) int {
	n := 0
	for _, c := range calls {
		if c == id {
			n++
		}
	}
	return n
}

func TestGoneMediaIsNeverRequestedAgain(t *testing.T) {
	s := newMutationStore(t)
	putMedia(t, s, "GONE", mediaURL(prefetchNow.Add(time.Hour)))
	var calls []string
	fetch := fakeFetch(s, &calls, map[string]error{"GONE": fmt.Errorf("wrapped: %w", whatsmeow.ErrMediaDownloadFailedWith410)})
	for i := 0; i < 3; i++ {
		// A fresh prefetcher each time stands in for a bridge restart.
		p := newPrefetcher(s, fetch, 0)
		p.now = func() time.Time { return prefetchNow }
		p.runOnce(context.Background())
	}
	if n := count(calls, "GONE"); n != 1 {
		t.Errorf("gone media requested %d times, want 1", n)
	}
	if gone, _ := s.mediaGone("GONE", tChatGrp, mediaURL(prefetchNow.Add(time.Hour))); !gone {
		t.Error("410 not recorded as gone")
	}
}

func TestGoneMarkDoesNotOutliveTheURL(t *testing.T) {
	s := newMutationStore(t)
	oldURL := mediaURL(prefetchNow.Add(time.Hour))
	putMedia(t, s, "M", oldURL)
	must(t, s.recordMediaFetch("M", tChatGrp, oldURL, whatsmeow.ErrMediaDownloadFailedWith410, prefetchNow))
	// History sync or a re-upload delivers a fresh URL.
	putMedia(t, s, "M", mediaURL(prefetchNow.Add(2*time.Hour)))
	if got := candidateIDs(t, s); fmt.Sprint(got) != "[M]" {
		t.Errorf("candidates = %v, want the re-delivered M", got)
	}
}

func TestOtherFailuresGetLimitedAttemptsAcrossRestarts(t *testing.T) {
	s := newMutationStore(t)
	putMedia(t, s, "FLAKY", mediaURL(prefetchNow.Add(30*24*time.Hour)))
	var calls []string
	fetch := fakeFetch(s, &calls, map[string]error{"FLAKY": errors.New("connection reset")})
	for i := 0; i < prefetchMaxAttempts+2; i++ {
		// A day apart, so every retry wait has passed.
		day := prefetchNow.Add(time.Duration(i) * 24 * time.Hour)
		p := newPrefetcher(s, fetch, 0)
		p.now = func() time.Time { return day }
		p.runOnce(context.Background())
	}
	if n := count(calls, "FLAKY"); n != prefetchMaxAttempts {
		t.Errorf("FLAKY attempted %d times, want %d", n, prefetchMaxAttempts)
	}
	if gone, _ := s.mediaGone("FLAKY", tChatGrp, mediaURL(prefetchNow.Add(30*24*time.Hour))); gone {
		t.Error("a transient failure was recorded as gone")
	}
}

func TestSuccessClearsRecordedFailures(t *testing.T) {
	s := newMutationStore(t)
	u := mediaURL(prefetchNow.Add(time.Hour))
	putMedia(t, s, "M", u)
	must(t, s.recordMediaFetch("M", tChatGrp, u, errors.New("timeout"), prefetchNow))
	must(t, s.recordMediaFetch("M", tChatGrp, u, nil, prefetchNow))
	var n int
	must(t, s.db.QueryRow("SELECT COUNT(*) FROM media_fetch_failures").Scan(&n))
	if n != 0 {
		t.Errorf("%d failure rows remain after a success", n)
	}
}

func TestPrefetchStopsWhileDisconnected(t *testing.T) {
	s := newMutationStore(t)
	putMedia(t, s, "A", mediaURL(prefetchNow.Add(time.Hour)))
	putMedia(t, s, "B", mediaURL(prefetchNow.Add(2*time.Hour)))
	calls := 0
	p := newPrefetcher(s, func(id, chat string) error {
		calls++
		return errNotConnected
	}, 0)
	p.now = func() time.Time { return prefetchNow }
	p.runOnce(context.Background())
	p.runOnce(context.Background())
	if calls != 2 {
		t.Errorf("fetch called %d times, want 1 per run", calls)
	}
	var n int
	must(t, s.db.QueryRow("SELECT COUNT(*) FROM media_fetch_failures").Scan(&n))
	if n != 0 {
		t.Error("a disconnection counted as a failed attempt")
	}
}

func TestStoringMediaNudgesThePrefetcher(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "TEXT", tChatGrp, tPeer, "hello", "", false)
	select {
	case <-s.mediaAdded:
		t.Fatal("a text message nudged the prefetcher")
	default:
	}
	putMedia(t, s, "IMG", mediaURL(prefetchNow.Add(time.Hour)))
	putMedia(t, s, "IMG2", mediaURL(prefetchNow.Add(time.Hour)))
	select {
	case <-s.mediaAdded:
	default:
		t.Fatal("storing media did not nudge the prefetcher")
	}
}

func TestRevokeRemovesCachedMedia(t *testing.T) {
	s := newMutationStore(t)
	putMedia(t, s, "GONE", mediaURL(prefetchNow.Add(time.Hour)))
	putMedia(t, s, "KEPT", mediaURL(prefetchNow.Add(time.Hour)))
	gone := mediaCachePath(tChatGrp, "GONE", "GONE.jpg")
	kept := mediaCachePath(tChatGrp, "KEPT", "KEPT.jpg")
	must(t, os.MkdirAll(filepath.Dir(gone), 0700))
	must(t, os.WriteFile(gone, []byte("x"), 0600))
	must(t, os.WriteFile(kept, []byte("x"), 0600))

	if applied, err := s.ApplyRevoke(tChatGrp, "GONE", tPeer, nil, t0); err != nil || !applied {
		t.Fatalf("ApplyRevoke = %v %v", applied, err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Error("revoked message's media is still cached")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Error("another message's media was removed")
	}
}

func TestFailedMediaWaitsBeforeRetrying(t *testing.T) {
	s := newMutationStore(t)
	u := mediaURL(prefetchNow.Add(48 * time.Hour))
	putMedia(t, s, "M", u)
	must(t, s.recordMediaFetch("M", tChatGrp, u, errors.New("status code 400"), prefetchNow))

	at := func(d time.Duration) string {
		cands, err := s.prefetchCandidates(prefetchNow.Add(d))
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(len(cands))
	}
	if got := at(30 * time.Minute); got != "0" {
		t.Error("retried within an hour of the first failure")
	}
	if got := at(61 * time.Minute); got != "1" {
		t.Error("not retried an hour after the first failure")
	}
	must(t, s.recordMediaFetch("M", tChatGrp, u, errors.New("status code 400"), prefetchNow.Add(61*time.Minute)))
	if got := at(3 * time.Hour); got != "0" {
		t.Error("retried within four hours of the second failure")
	}
	if got := at(61*time.Minute + 4*time.Hour + time.Minute); got != "1" {
		t.Error("not retried four hours after the second failure")
	}
}
