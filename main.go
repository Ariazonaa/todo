package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // bundle zone data in the binary — the container needs no tzdata package
)

//go:embed static
var staticFS embed.FS

type config struct {
	port           string
	dbPath         string
	insecureCookie bool
}

type server struct {
	db    *sql.DB
	store *settingsStore
	cer   *ceremonies
	hub   *hub
	cfg   config
	// shutdown is closed on shutdown. Without this, srv.Shutdown would wait
	// for the open SSE streams until the timeout kicks in — those are
	// intentionally endless.
	shutdown chan struct{}

	// Setup code for an instance without a passkey (setupcode.go).
	setupMu   sync.Mutex
	setupCode string

	// Only one import at a time: it stages images in import_attachments.
	importMu sync.Mutex

	// Sender key for Web Push (webpush.go).
	vapid *vapidKey
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := config{
		port:           env("PORT", "8080"),
		dbPath:         env("DB_PATH", "todo.db"),
		insecureCookie: os.Getenv("INSECURE_COOKIE") == "1",
	}
	db, err := openDB(cfg.dbPath)
	if err != nil {
		log.Fatalf("open DB (%s): %v", cfg.dbPath, err)
	}
	defer db.Close()

	store, err := newSettingsStore(db)
	if err != nil {
		log.Fatalf("load settings: %v", err)
	}

	vapid, err := loadOrCreateVAPID(db)
	if err != nil {
		log.Fatalf("push key: %v", err)
	}

	mime.AddExtensionType(".webmanifest", "application/manifest+json")

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	s := &server{db: db, store: store, cer: newCeremonies(), hub: newHub(), cfg: cfg,
		vapid: vapid, shutdown: make(chan struct{})}
	if err := s.prepareSetupCode(); err != nil {
		log.Fatalf("setup code: %v", err)
	}
	sched := newScheduler(db, store, s.hub, vapid)
	go sched.run(ctx)

	mux := http.NewServeMux()

	// Two limiters: one strict on the unauthenticated auth surface, one
	// generous over everything else. The app itself never fires 20
	// requests at startup — 300/min lets every real use through while
	// still stopping a flood against the single DB connection.
	authLimit := newRateLimiter(20, 20)
	globalLimit := newRateLimiter(300, 300)

	// Public: setup/login (WebAuthn), health check, and everything the
	// browser must be able to load without credentials for PWA
	// installation.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("POST /api/auth/login/begin", authLimit.limit(s.handleLoginBegin))
	mux.HandleFunc("POST /api/auth/login/finish", authLimit.limit(s.handleLoginFinish))
	mux.HandleFunc("POST /api/setup/begin", authLimit.limit(s.handleSetupBegin))
	mux.HandleFunc("POST /api/setup/finish", authLimit.limit(s.handleSetupFinish))
	mux.HandleFunc("GET /api/setup/mode", authLimit.limit(s.handleSetupMode))
	mux.HandleFunc("GET /style.css", staticFile("static/style.css"))
	mux.HandleFunc("GET /app.js", staticFile("static/app.js"))
	mux.HandleFunc("GET /dateparse.js", staticFile("static/dateparse.js"))
	mux.HandleFunc("GET /webauthn.js", staticFile("static/webauthn.js"))
	// i18n.js loads as the first script on every page; login and setup
	// need the catalogs too, without being logged in.
	mux.HandleFunc("GET /i18n.js", staticFile("static/i18n.js"))
	mux.HandleFunc("GET /locales/{file}", handleLocale)
	// The login/setup scripts live in separate files because the CSP
	// (script-src 'self') blocks inline scripts — otherwise no button
	// there would work.
	mux.HandleFunc("GET /login-page.js", staticFile("static/login-page.js"))
	mux.HandleFunc("GET /setup-page.js", staticFile("static/setup-page.js"))
	mux.HandleFunc("GET /sw.js", staticFile("static/sw.js"))
	mux.HandleFunc("GET /manifest.webmanifest", staticFile("static/manifest.webmanifest"))
	mux.Handle("GET /icons/", iconHandler())

	// Protected: app, task API, and control panel
	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleIndex)))
	mux.Handle("POST /api/logout", s.requireAuth(http.HandlerFunc(s.handleLogout)))
	mux.Handle("POST /api/logout-all", s.requireAuth(http.HandlerFunc(s.handleLogoutAll)))
	mux.Handle("GET /api/tasks", s.requireAuth(http.HandlerFunc(s.handleListTasks)))
	mux.Handle("POST /api/tasks", s.requireAuth(http.HandlerFunc(s.handleCreateTask)))
	mux.Handle("PUT /api/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handleUpdateTask)))
	mux.Handle("POST /api/tasks/{id}/complete", s.requireAuth(http.HandlerFunc(s.handleCompleteTask)))
	mux.Handle("POST /api/tasks/{id}/uncomplete", s.requireAuth(http.HandlerFunc(s.handleUncompleteTask)))
	mux.Handle("POST /api/tasks/{id}/snooze", s.requireAuth(http.HandlerFunc(s.handleSnoozeTask)))
	mux.Handle("POST /api/tasks/{id}/progress", s.requireAuth(http.HandlerFunc(s.handleToggleProgress)))
	mux.Handle("POST /api/tasks/{id}/links", s.requireAuth(http.HandlerFunc(s.handleAddLink)))
	mux.Handle("DELETE /api/tasks/{id}/links/{other}", s.requireAuth(http.HandlerFunc(s.handleRemoveLink)))
	mux.Handle("DELETE /api/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteTask)))
	mux.Handle("GET /api/lists", s.requireAuth(http.HandlerFunc(s.handleListLists)))
	mux.Handle("POST /api/lists", s.requireAuth(http.HandlerFunc(s.handleCreateList)))
	mux.Handle("PUT /api/lists/{id}", s.requireAuth(http.HandlerFunc(s.handleRenameList)))
	mux.Handle("DELETE /api/lists/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteList)))
	mux.Handle("PUT /api/lists/{id}/members/{uid}", s.requireAuth(http.HandlerFunc(s.handleAddMember)))
	mux.Handle("DELETE /api/lists/{id}/members/{uid}", s.requireAuth(http.HandlerFunc(s.handleRemoveMember)))
	mux.Handle("GET /api/people", s.requireAuth(http.HandlerFunc(s.handlePeople)))
	mux.Handle("GET /api/stats", s.requireAuth(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /api/events", s.requireAuth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("PUT /api/tasks/reorder", s.requireAuth(http.HandlerFunc(s.handleReorder)))
	mux.Handle("GET /api/export", s.requireAuth(http.HandlerFunc(s.handleExport)))
	mux.Handle("POST /api/import", s.requireAuth(http.HandlerFunc(s.handleImport)))
	mux.Handle("POST /api/tasks/{id}/attachments", s.requireAuth(http.HandlerFunc(s.handleAttachmentUpload)))
	mux.Handle("GET /api/tasks/{task}/attachments/{id}", s.requireAuth(http.HandlerFunc(s.handleAttachmentGet)))
	mux.Handle("GET /api/tasks/{task}/attachments/{id}/thumb", s.requireAuth(http.HandlerFunc(s.handleAttachmentThumb)))
	mux.Handle("DELETE /api/tasks/{task}/attachments/{id}", s.requireAuth(http.HandlerFunc(s.handleAttachmentDelete)))
	mux.Handle("GET /api/users", s.requireAuth(http.HandlerFunc(s.handleListUsers)))
	mux.Handle("POST /api/users", s.requireAuth(http.HandlerFunc(s.handleCreateUser)))
	mux.Handle("POST /api/users/{id}/code", s.requireAuth(http.HandlerFunc(s.handleNewUserCode)))
	mux.Handle("DELETE /api/users/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteUser)))
	mux.Handle("GET /api/settings", s.requireAuth(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("PUT /api/settings", s.requireAuth(http.HandlerFunc(s.handlePutSettings)))
	mux.Handle("PUT /api/settings/language", s.requireAuth(http.HandlerFunc(s.handleSetLanguage)))
	mux.Handle("POST /api/settings/test", s.requireAuth(http.HandlerFunc(s.handleTestWebhook)))
	mux.Handle("GET /api/passkeys", s.requireAuth(http.HandlerFunc(s.handlePasskeyList)))
	mux.Handle("POST /api/passkeys/begin", s.requireAuth(http.HandlerFunc(s.handlePasskeyBegin)))
	mux.Handle("POST /api/passkeys/finish", s.requireAuth(http.HandlerFunc(s.handlePasskeyFinish)))
	mux.Handle("DELETE /api/passkeys/{id}", s.requireAuth(http.HandlerFunc(s.handlePasskeyDelete)))
	mux.Handle("GET /api/push/key", s.requireAuth(http.HandlerFunc(s.handlePushKey)))
	mux.Handle("PUT /api/push/subscription", s.requireAuth(http.HandlerFunc(s.handlePushSubscribe)))
	mux.Handle("DELETE /api/push/subscription", s.requireAuth(http.HandlerFunc(s.handlePushUnsubscribe)))
	mux.Handle("POST /api/push/test", s.requireAuth(http.HandlerFunc(s.handlePushTest)))

	srv := newHTTPServer(":"+cfg.port, secHeaders(globalLimit.middleware(mux)))
	go func() {
		log.Printf("todo listening on :%s (DB %s)", cfg.port, cfg.dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server: %v", err)
		}
	}()

	<-ctx.Done()
	// From here on, a second signal takes hard effect again — if shutdown
	// hangs, Ctrl-C still gets you out.
	stopSignals()
	log.Print("shutting down …")
	close(s.shutdown) // end open SSE streams, otherwise Shutdown waits for nothing

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown: %v", err)
	}
	// Wait for the scheduler, so it doesn't write into a closed db mid-
	// write. A hanging Discord POST must not hold us up though (its client
	// has a 10s timeout) — a push tick can take longer (several devices,
	// one POST each), so once this budget runs out it keeps running
	// interrupted. That costs at most one duplicate notification after a
	// restart, never a missed one: stamps (task_reminders) are only
	// written after successful delivery, so a cut-off tick never leaves
	// anything marked done that wasn't.
	select {
	case <-sched.done:
	case <-time.After(12 * time.Second):
		log.Print("scheduler not responding, shutting down anyway")
	}
	log.Print("bye")
}

func secHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		// Own browsing-context group (no window.opener access from other
		// pages), resources restricted to our own origin, unused APIs
		// disabled.
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), payment=(), usb=(), serial=(), bluetooth=()")
		// base-uri and form-action do NOT fall back to default-src and
		// therefore must be set individually.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// staticFile serves an embedded file with no-cache, so the service
// worker/browser always gets fresh assets after a deploy.
func staticFile(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticFS, path)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, staticFS, "static/index.html")
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if none, err := s.noPasskeys(); err == nil && none {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if s.authed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, staticFS, "static/login.html")
}

// handleSetupPage: first account or an account with a setup code —
// public, as long as you're not logged in.
func (s *server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if s.authed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, staticFS, "static/setup.html")
}

// newHTTPServer: without a read and idle timeout, keep-alive connections
// and trickling bodies would live forever. Import and image upload extend
// their own read deadline (extendReadDeadline). No WriteTimeout: that
// would terminate SSE streams and long exports.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// extendReadDeadline gives a large upload more time than ReadTimeout.
func extendReadDeadline(w http.ResponseWriter, d time.Duration) {
	// Error only if the writer can't do this (tests using ResponseRecorder).
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
}

// iconHandler serves the icons, but no directory listing.
func iconHandler() http.Handler {
	icons, err := fs.Sub(staticFS, "static/icons")
	if err != nil {
		panic(err) // embedded: can only be a build error
	}
	files := http.StripPrefix("/icons/", http.FileServerFS(icons))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
