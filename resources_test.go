package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Resources a logged-in account must not be able to exhaust for everyone:
// memory (1 GB container) and the single DB connection.

// maxWriteRecorder remembers the largest single write of a response.
type maxWriteRecorder struct {
	*httptest.ResponseRecorder
	max int
}

func (m *maxWriteRecorder) Write(b []byte) (int, error) {
	m.max = max(m.max, len(b))
	return m.ResponseRecorder.Write(b)
}

// bigNotes gives the admin n tasks whose notes are full of "<" (six bytes
// each in HTML-escaped JSON).
func bigNotes(t *testing.T, s *server, n int) string {
	t.Helper()
	note := strings.Repeat("<", maxNoteLen)
	list := defaultListID(t, s.db)
	tx, _ := s.db.Begin()
	for i := range n {
		if _, err := tx.Exec("INSERT INTO tasks (title, created_at, note, list_id) VALUES (?, '2026-01-01T00:00:00Z', ?, ?)", fmt.Sprint(i), note, list); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return note
}

// The task list used to be built as one JSON document in memory and
// written in one go: a member filling the owner's lists made every load of
// the app take hundreds of MB — and after the crash, every client reloads.
func TestTaskListIsWrittenInChunks(t *testing.T) {
	s := testServer(t)
	note := bigNotes(t, s, 300)
	rec := &maxWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.handleListTasks(rec, freshUser(httptest.NewRequest("GET", "/api/tasks", nil), testUID))
	var got struct {
		Tasks []Task `json:"tasks"`
		Today string `json:"today"`
		Lists []List `json:"lists"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Antwort kein JSON: %v", err)
	}
	if len(got.Tasks) != 300 || got.Tasks[0].Note != note || got.Today == "" || len(got.Lists) == 0 {
		t.Fatalf("Antwort unvollständig: %d Aufgaben, today %q, %d Listen", len(got.Tasks), got.Today, len(got.Lists))
	}
	if rec.max > 256<<10 {
		t.Fatalf("Antwort in einem Stück von %d KB geschrieben", rec.max>>10)
	}
}

// The same for the backup: tasks, links and stats sat in memory as one
// JSON document before the first byte went out.
func TestExportIsWrittenInChunks(t *testing.T) {
	s := testServer(t)
	note := bigNotes(t, s, 300)
	rec := &maxWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.handleExport(rec, freshUser(httptest.NewRequest("GET", "/api/export", nil), testUID))
	var got exportData
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Backup kein JSON: %v", err)
	}
	if len(got.Tasks) != 300 || got.Tasks[0].Note != note || len(got.Lists) == 0 {
		t.Fatalf("Backup unvollständig: %d Aufgaben, %d Listen", len(got.Tasks), len(got.Lists))
	}
	if rec.max > 256<<10 {
		t.Fatalf("Backup in einem Stück von %d KB geschrieben", rec.max>>10)
	}
	if rec := postImportRaw(t, testServer(t), testUID, rec.Body.String()); rec.Code != http.StatusNoContent {
		t.Fatalf("Backup lässt sich nicht importieren: %d %s", rec.Code, rec.Body)
	}
}

// stalledResponse runs h with a response nobody reads yet; drain lets it
// finish (at the latest when the test ends — the slots are package-wide).
// done closes once h has returned.
func stalledResponse(t *testing.T, h http.HandlerFunc, r *http.Request) (drain func(), done chan struct{}) {
	pr, pw := io.Pipe()
	done = make(chan struct{})
	go func() {
		h(&pipeWriter{h: http.Header{}, w: pw}, r)
		pw.Close()
		close(done)
	}()
	drain = func() { go io.Copy(io.Discard, pr) }
	t.Cleanup(drain)
	return drain, done
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: nie eingetreten", what)
		}
	}
}

// finished runs h and closes the returned channel once it's done.
func finished(h http.HandlerFunc, r *http.Request) chan struct{} {
	done := make(chan struct{})
	go func() {
		h(httptest.NewRecorder(), r)
		close(done)
	}()
	return done
}

func waitDone(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s hängt", what)
	}
}

// stillRunning: done hasn't closed within a short while.
func stillRunning(done chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-time.After(200 * time.Millisecond):
		return true
	}
}

// One person loads the list one request at a time: in parallel, their
// requests would each hold a copy of the whole list. Others don't wait for
// them.
func TestTaskListLoadsOneAtATimePerPerson(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	bigNotes(t, s, 50)
	get := func(uid int64) *http.Request { return freshUser(httptest.NewRequest("GET", "/api/tasks", nil), uid) }

	drain, first := stalledResponse(t, s.handleListTasks, get(testUID))
	waitFor(t, "erste Liste hält ihren Platz", func() bool { return len(taskLoads.total) == 1 })
	second := finished(s.handleListTasks, get(testUID))
	waitDone(t, finished(s.handleListTasks, get(bea)), "Beas Liste")
	if !stillRunning(second) {
		t.Fatal("zweite Liste derselben Person geladen, während die erste noch läuft")
	}
	drain()
	waitDone(t, first, "erste Liste")
	waitDone(t, second, "zweite Liste")
}

// waitSlots: per person, then overall — and a request whose client is gone
// stops waiting.
func TestWaitSlotsLimitPerPersonAndOverall(t *testing.T) {
	ws := newWaitSlots(1, 2)
	ctx := context.Background()
	rel1, ok1 := ws.acquire(ctx, 1)
	rel2, ok2 := ws.acquire(ctx, 2)
	if !ok1 || !ok2 {
		t.Fatal("zwei Personen sollten gleichzeitig dürfen")
	}
	gone, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := ws.acquire(gone, 3); ok {
		t.Fatal("dritte Person über der Gesamtgrenze")
	}
	if _, ok := ws.acquire(gone, 1); ok {
		t.Fatal("zweiter Platz für Person 1")
	}
	rel2()
	rel3, ok3 := ws.acquire(ctx, 3)
	if !ok3 {
		t.Fatal("frei gewordener Platz nicht vergeben")
	}
	rel1()
	rel3()
	if len(ws.users) != 0 {
		t.Fatalf("Einträge bleiben liegen: %d", len(ws.users))
	}
}

// Without a thumbnail (imported images), /thumb decodes the original —
// one at a time. Every waiting request used to hold its original (up to
// 15 MB) in memory, and kept waiting after its client was long gone.
func TestThumbWaitEndsWithClient(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Bild"}`)
	res, err := s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', ?)", task.ID, pngBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	decodeSlot <- struct{}{} // someone else is decoding
	defer func() { <-decodeSlot }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := freshUser(httptest.NewRequestWithContext(ctx, "GET", "/", nil), testUID)
	req.SetPathValue("task", fmt.Sprint(task.ID))
	req.SetPathValue("id", fmt.Sprint(id))
	waitDone(t, finished(s.handleAttachmentThumb, req), "Vorschau nach dem Abbruch")
}

// imageFixture: a task with an image of size bytes; returns the request
// path of the original.
func imageFixture(t *testing.T, s *server, uid int64, size int) string {
	t.Helper()
	task := mustCreateAs(t, s, uid, `{"title":"Bild"}`)
	data := make([]byte, size)
	rand.Read(data)
	res, err := s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/jpeg', 'z', ?)", task.ID, data)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return fmt.Sprintf("/api/tasks/%d/attachments/%d", task.ID, id)
}

func imageReq(uid int64, path string) *http.Request {
	req := freshUser(httptest.NewRequest("GET", path, nil), uid)
	parts := strings.Split(path, "/")
	req.SetPathValue("task", parts[3])
	req.SetPathValue("id", parts[5])
	return req
}

// stalledWriter is a client that stopped reading: a write blocks until the
// write deadline the handler set has passed — without one, forever.
type stalledWriter struct {
	h        http.Header
	mu       sync.Mutex
	deadline time.Time
}

func (w *stalledWriter) Header() http.Header { return w.h }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	return nil
}
func (w *stalledWriter) Write([]byte) (int, error) {
	for {
		w.mu.Lock()
		d := w.deadline
		w.mu.Unlock()
		if !d.IsZero() && time.Now().After(d) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A client that stops reading held what the response had in memory (an
// original of up to 15 MB, the backup's tasks) for as long as the
// connection lasted — there's no write timeout (SSE, backups), and nginx
// passes the backpressure on.
func TestLongResponsesGiveUpOnStalledClient(t *testing.T) {
	s := testServer(t)
	bigNotes(t, s, 50)
	image := imageFixture(t, s, testUID, 1<<20)
	defer func(d time.Duration) { writeIdle = d }(writeIdle)
	writeIdle = 200 * time.Millisecond
	for name, c := range map[string]struct {
		h   http.HandlerFunc
		req *http.Request
	}{
		"Original":      {s.handleAttachmentGet, imageReq(testUID, image)},
		"Backup":        {s.handleExport, freshUser(httptest.NewRequest("GET", "/api/export", nil), testUID)},
		"Aufgabenliste": {s.handleListTasks, freshUser(httptest.NewRequest("GET", "/api/tasks", nil), testUID)},
	} {
		done := make(chan struct{})
		go func() {
			c.h(&stalledWriter{h: http.Header{}}, c.req)
			close(done)
		}()
		waitDone(t, done, name+" an einen Client, der nicht liest")
	}
}

// Originals per person at a time: a few stalled downloads of one account
// hold their memory — not an unbounded number.
func TestImageDownloadsPerPersonAreLimited(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	mine := imageFixture(t, s, testUID, 1<<20)
	hers := imageFixture(t, s, bea, 1<<20)
	var drains []func()
	var stalled []chan struct{}
	for range downloadsPerPerson {
		drain, done := stalledResponse(t, s.handleAttachmentGet, imageReq(testUID, mine))
		drains = append(drains, drain)
		stalled = append(stalled, done)
	}
	waitFor(t, "hängende Downloads halten ihre Plätze", func() bool { return len(downloads.total) == downloadsPerPerson })
	extra := finished(s.handleAttachmentGet, imageReq(testUID, mine))
	waitDone(t, finished(s.handleAttachmentGet, imageReq(bea, hers)), "Beas Bild")
	if !stillRunning(extra) {
		t.Fatalf("mehr als %d Originale derselben Person gleichzeitig", downloadsPerPerson)
	}
	for _, d := range drains {
		d()
	}
	for _, done := range stalled {
		waitDone(t, done, "hängender Download")
	}
	waitDone(t, extra, "wartender Download")
}

// Every device of every account gets each notification, one after another
// on the scheduler: an account mustn't register any number of them.
func TestPushDevicesPerAccountAreLimited(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	for i := range maxPushDevicesPerUser {
		st.register(t, s.db, fmt.Sprintf("/dev%d", i), 0)
	}
	rec := httptest.NewRecorder()
	s.handlePushSubscribe(rec, pushReq(s, http.MethodPut, subBody(st, "/einer-zu-viel")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("Gerät über dem Limit: %d %s, erwartet 409", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.handlePushSubscribe(rec, pushReq(s, http.MethodPut, subBody(st, "/dev0")))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bekanntes Gerät auffrischen: %d %s", rec.Code, rec.Body)
	}
	// The browser renewed a subscription: the successor replaces the old one,
	// the limit doesn't stand in its way.
	renewed := strings.Replace(subBody(st, "/dev1-neu"), `{"endpoint"`, fmt.Sprintf(`{"old_endpoint":%q,"endpoint"`, st.endpoint("/dev1")), 1)
	rec = httptest.NewRecorder()
	s.handlePushSubscribe(rec, pushReq(s, http.MethodPut, renewed))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erneuerte Subscription am Limit: %d %s", rec.Code, rec.Body)
	}
	if n, _ := countPushSubs(s.db, testUID); n != maxPushDevicesPerUser {
		t.Fatalf("%d Geräte, erwartet %d", n, maxPushDevicesPerUser)
	}
}

// A push service on another port is no push service: the server would
// wait out every timeout there, one device after the other.
func TestPushHostsOnlyOnStandardPort(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/x":     true,
		"https://fcm.googleapis.com:443/fcm/send/x": true,
		"https://fcm.googleapis.com:81/fcm/send/x":  false,
		"https://wns2.notify.windows.com:9/w":       false,
	} {
		u, _ := url.Parse(raw)
		if got := pushHostAllowed(u); got != want {
			t.Errorf("%s: %v, erwartet %v", raw, got, want)
		}
	}
}

// steadyWriter is a slow client that keeps reading: rate bytes per second.
// A write that would take past the write deadline is cut off, like a real
// connection.
type steadyWriter struct {
	h        http.Header
	rate     int
	mu       sync.Mutex
	deadline time.Time
	body     bytes.Buffer
}

func (w *steadyWriter) Header() http.Header { return w.h }
func (w *steadyWriter) WriteHeader(int)     {}
func (w *steadyWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	return nil
}
func (w *steadyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	d := w.deadline
	w.mu.Unlock()
	took := time.Duration(len(p)) * time.Second / time.Duration(w.rate)
	if !d.IsZero() && time.Now().Add(took).After(d) {
		time.Sleep(time.Until(d))
		return 0, os.ErrDeadlineExceeded
	}
	time.Sleep(took)
	return w.body.Write(p)
}

// A client that keeps reading, just slowly, must get the whole backup: the
// write deadline is per chunk. An image went to the writer in one piece and
// had a single deadline for all of its up to 20 MB.
func TestSlowButSteadyClientGetsEverything(t *testing.T) {
	s := testServer(t)
	image := imageFixture(t, s, testUID, 1<<20)
	defer func(d time.Duration) { writeIdle = d }(writeIdle)
	writeIdle = 300 * time.Millisecond // 64 KB take 64 ms at 1 MB/s, the image 1.3 s

	w := &steadyWriter{h: http.Header{}, rate: 1 << 20}
	s.handleExport(w, freshUser(httptest.NewRequest("GET", "/api/export", nil), testUID))
	var got exportData
	if err := json.Unmarshal(w.body.Bytes(), &got); err != nil || len(got.Attachments) != 1 || len(got.Attachments[0].Data) != 1<<20 {
		t.Fatalf("Backup an einen langsamen Client abgebrochen nach %d Byte (%v)", w.body.Len(), err)
	}
	w = &steadyWriter{h: http.Header{}, rate: 1 << 20}
	s.handleAttachmentGet(w, imageReq(testUID, image))
	if w.body.Len() != 1<<20 {
		t.Fatalf("Original an einen langsamen Client: %d Byte", w.body.Len())
	}
}

// The task list holds one of two slots overall while it's written: a client
// reading just fast enough for every chunk mustn't keep it for good.
func TestTaskListResponseHasAnOverallDeadline(t *testing.T) {
	s := testServer(t)
	bigNotes(t, s, 100) // ~500 KB
	defer func(d, m time.Duration) { writeIdle, taskListWriteMax = d, m }(writeIdle, taskListWriteMax)
	writeIdle, taskListWriteMax = 300*time.Millisecond, 400*time.Millisecond

	w := &steadyWriter{h: http.Header{}, rate: 512 << 10} // a chunk in 125 ms, all of it in about a second
	done := make(chan struct{})
	go func() {
		s.handleListTasks(w, freshUser(httptest.NewRequest("GET", "/api/tasks", nil), testUID))
		close(done)
	}()
	waitDone(t, done, "Aufgabenliste an einen sehr langsamen Client")
	if json.Valid(w.body.Bytes()) {
		t.Fatalf("Aufgabenliste trotz Gesamtfrist vollständig geschrieben (%d Byte)", w.body.Len())
	}
}

// A request whose client is already gone doesn't take a free slot (and
// loads nothing for nobody).
func TestWaitSlotsSkipGoneRequests(t *testing.T) {
	ws := newWaitSlots(1, 1)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for range 20 {
		if release, ok := ws.acquire(gone, 1); ok {
			release()
			t.Fatal("Platz an eine abgebrochene Anfrage vergeben")
		}
	}
}

// A decoder that panics (net/http catches the panic) used to keep the
// decode slot: every upload after that hung in makeThumb, every missing
// thumbnail waited forever.
func TestThumbFreesDecodeSlotWhenDecoderPanics(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Bild"}`)
	res, err := s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', ?)", task.ID, pngBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	defer func(f func(io.Reader) (image.Image, string, error)) { decodeImage = f }(decodeImage)
	decodeImage = func(io.Reader) (image.Image, string, error) { panic("kaputter Decoder") }
	t.Cleanup(func() {
		select {
		case <-decodeSlot: // left behind by a failed run: don't hang the following tests
		default:
		}
	})

	req := freshUser(httptest.NewRequest("GET", "/", nil), testUID)
	req.SetPathValue("task", fmt.Sprint(task.ID))
	req.SetPathValue("id", fmt.Sprint(id))
	func() {
		defer func() { recover() }()
		s.handleAttachmentThumb(httptest.NewRecorder(), req)
	}()
	select {
	case decodeSlot <- struct{}{}:
		<-decodeSlot
	default:
		t.Fatal("Decode-Platz nach einer Panik im Decoder weiter belegt")
	}
}

// Owners above the (lowered) task limit can't add tasks anymore — the
// start says so in the log instead of leaving them to find out.
func TestStartupWarnsAboutOwnersAboveTaskLimit(t *testing.T) {
	s := testServer(t)
	list := defaultListID(t, s.db)
	tx, _ := s.db.Begin()
	for range maxTasksPerOwner + 1 {
		tx.Exec("INSERT INTO tasks (title, created_at, list_id) VALUES ('x', 'z', ?)", list)
	}
	tx.Commit()
	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	warnOverTaskLimit(s.db)
	if !strings.Contains(out.String(), fmt.Sprint(maxTasksPerOwner+1)) {
		t.Fatalf("keine Warnung im Log: %q", out.String())
	}
}
