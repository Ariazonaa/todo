package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Without a read and idle timeout, keep-alive connections and trickling
// request bodies lived forever.
func TestHTTPServerHasTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("Timeouts: header=%v read=%v idle=%v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout %v beendete SSE-Streams und lange Exporte", srv.WriteTimeout)
	}
}

// startShortTimeoutServer: like newHTTPServer, but with ReadTimeout in the
// millisecond range, so the tests don't wait a full minute.
func startShortTimeoutServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.Config.ReadTimeout = 300 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func authedEventsServer(t *testing.T) (*server, *httptest.Server, *http.Request) {
	t.Helper()
	s := sessionServer(t)
	s.shutdown = make(chan struct{})
	mustCreateSession(t, s.db, "tok", time.Now().Add(time.Hour), 0, testUID)
	ts := startShortTimeoutServer(t, http.HandlerFunc(s.handleEvents))
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	return s, ts, req
}

// readEvent reads up to the next "data:" line or the end of the stream.
func readEvent(t *testing.T, r *bufio.Reader, within time.Duration) (string, error) {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil || strings.HasPrefix(line, "data:") {
				ch <- res{line, err}
				return
			}
		}
	}()
	select {
	case got := <-ch:
		return got.line, got.err
	case <-time.After(within):
		t.Fatalf("kein Event innerhalb von %v", within)
		return "", nil
	}
}

// Go cancels the request's context on ReadTimeout — an SSE stream used to
// end after the timeout unless the handler lifted the deadline.
func TestSSESurvivesReadTimeout(t *testing.T) {
	s, _, req := authedEventsServer(t)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(700 * time.Millisecond) // longer than ReadTimeout
	s.hub.notifyAll()
	line, err := readEvent(t, bufio.NewReader(resp.Body), 2*time.Second)
	if err != nil || !strings.HasPrefix(line, "data: changed") {
		t.Fatalf("Stream nach ReadTimeout: %q, %v", line, err)
	}
}

// After logging out, an open stream used to keep receiving change signals.
func TestSSEEndsWhenSessionIsGone(t *testing.T) {
	old := sseHeartbeat
	sseHeartbeat = 100 * time.Millisecond
	t.Cleanup(func() { sseHeartbeat = old })
	s, _, req := authedEventsServer(t)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := s.db.Exec("DELETE FROM sessions"); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvent(t, bufio.NewReader(resp.Body), 2*time.Second); err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatalf("Stream lief nach dem Abmelden weiter (err=%v)", err)
	}
}

// slowBody sends the body in two parts with a pause in between.
func slowBody(first, rest []byte, pause time.Duration) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		pw.Write(first)
		time.Sleep(pause)
		pw.Write(rest)
		pw.Close()
	}()
	return pr
}

// A large backup over a slow connection can take longer than ReadTimeout.
func TestImportSurvivesReadTimeout(t *testing.T) {
	s := testServer(t)
	ts := startShortTimeoutServer(t, asTestUser(s.handleImport))
	body := slowBody([]byte(`{"tasks":[`), []byte(`{"id":1,"title":"langsam","created_at":"z"}]}`), 700*time.Millisecond)
	resp, err := http.Post(ts.URL, "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("langsamer Import: %d %s", resp.StatusCode, b)
	}
}

func TestUploadSurvivesReadTimeout(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"mit Bild"}`)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks/{id}/attachments", asTestUser(s.handleAttachmentUpload))
	ts := startShortTimeoutServer(t, mux)
	img := pngBytes(t)
	body := slowBody(img[:10], img[10:], 700*time.Millisecond)
	resp, err := http.Post(ts.URL+"/api/tasks/"+fmt.Sprint(task.ID)+"/attachments", "image/png", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("langsamer Upload: %d %s", resp.StatusCode, b)
	}
}
