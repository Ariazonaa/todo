package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxAttachmentSize     = 15 << 20 // 15 MB (the client compresses to ~2048px JPEG beforehand)
	maxAttachmentsPerTask = 6
	thumbMax              = 512
	// Decompression bombs: file size says nothing about memory use during
	// decode. A 168 KB 12000x12000 PNG can take several hundred MB decoded,
	// and a progressive JPEG can cost ~14 bytes per pixel (7200x7200 from
	// 594 KB: ~740 MB). The client downsizes anything over 300 KB to 2048px
	// beforehand; 16 MP leaves plenty of headroom for that while capping a
	// decode at ~240 MB.
	maxImagePixels = 16 << 20
)

// decodeSlot allows only one image decode to run at a time. The list
// requests all thumbnails at once, and without a thumbnail (import, legacy
// data) each request decodes the original — running in parallel would
// multiply the limit above.
var decodeSlot = make(chan struct{}, 1)

// checkImageDimensions reads only the header and rejects oversized images
// before anything gets decoded (and thus allocated).
func checkImageDimensions(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return uerr("error.image_dimensions")
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return uerr("error.image_pixels", "w", cfg.Width, "h", cfg.Height)
	}
	return nil
}

// makeThumb creates a small JPEG thumbnail (max thumbMax px) for list rendering.
func makeThumb(data []byte) ([]byte, error) {
	// Check here too, not just on upload: the lazy backfill feeds this with
	// existing images from the DB that never went through this check.
	if err := checkImageDimensions(data); err != nil {
		return nil, err
	}
	decodeSlot <- struct{}{}
	defer func() { <-decodeSlot }()
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := max(w, h)
	scale := 1.0
	if longest > thumbMax {
		scale = float64(thumbMax) / float64(longest)
	}
	tw := int(float64(w)*scale + 0.5)
	th := int(float64(h)*scale + 0.5)
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	// JPEG can't do transparency — dark UI background instead of alpha
	xdraw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.RGBA{R: 28, G: 28, B: 30, A: 255}}, image.Point{}, xdraw.Src)
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var allowedImageMimes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/gif":  true,
}

// serveMime protects delivery: the stored mime type ends up as the
// Content-Type in the browser. On upload it's determined from the content,
// but an imported backup could contain "text/html" — that would be stored
// XSS on our own origin. Anything unknown is served as a download.
func serveMime(w http.ResponseWriter, mime string) {
	if allowedImageMimes[mime] {
		w.Header().Set("Content-Type", mime)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
}

// notModified sets an attachment's cache headers and answers a matching
// revalidation with 304. An ID's content never changes and IDs are never
// reused (AUTOINCREMENT), so an ETag derived from the ID is enough.
// no-cache instead of immutable: every use checks back with the server
// first and thus goes through requireAuth — after logging out, no image
// comes from the browser cache anymore. Within a loaded page the browser
// keeps the images in memory anyway, so this only costs one 304 per page
// load.
func notModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == "*" || strings.Contains(inm, etag)) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// attachmentPathIDs reads {task} and {id} from an image request's path.
func attachmentPathIDs(w http.ResponseWriter, r *http.Request) (id, taskID int64, ok bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return 0, 0, false
	}
	taskID, err = strconv.ParseInt(r.PathValue("task"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_task_id")
		return 0, 0, false
	}
	return id, taskID, true
}

// attachmentBelongsToTask: the image belongs to this task, and uid is
// allowed to see the task.
func attachmentBelongsToTask(db *sql.DB, uid, attachmentID, taskID int64) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM attachments WHERE id = ? AND task_id = ? "+
		"AND task_id IN (SELECT id FROM tasks WHERE list_id "+visibleLists+")", attachmentID, taskID, uid).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *server) handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	extendReadDeadline(w, 5*time.Minute) // 15 MB over a mobile connection
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentSize)
	data, err := io.ReadAll(r.Body)
	if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
		writeError(w, r, http.StatusRequestEntityTooLarge, "error.image_too_big")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.upload_aborted")
		return
	}
	if len(data) == 0 {
		writeError(w, r, http.StatusBadRequest, "error.empty_file")
		return
	}
	// Determine mime type from the content, don't trust the header
	mime := http.DetectContentType(data)
	if !allowedImageMimes[mime] {
		writeError(w, r, http.StatusBadRequest, "error.image_type")
		return
	}
	// Before decoding: otherwise a small file is enough to push the
	// process into OOM via a decompression bomb.
	if err := checkImageDimensions(data); err != nil {
		if _, ok := errors.AsType[*userError](err); ok {
			writeErr(w, r, http.StatusBadRequest, err)
		} else {
			// Image library decode error: technical, but helpful
			writeError(w, r, http.StatusBadRequest, "error.image_unprocessable", "detail", err.Error())
		}
		return
	}
	thumb, _ := makeThumb(data) // error ok: /thumb then serves the original
	// The task may have been deleted during the upload (seconds, for
	// 15 MB). Without the check in the INSERT itself, the image would end
	// up on whichever task gets the same ID next — tasks.id is reused.
	res, err := s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data, thumb) "+
		"SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM tasks WHERE id = ? AND list_id "+visibleLists+")",
		t.ID, mime, time.Now().UTC().Format(time.RFC3339), data, thumb, t.ID, userID(r))
	if err != nil {
		// The attachments_limit trigger aborts with RAISE(ABORT, …)
		if strings.Contains(err.Error(), "attachment limit reached") {
			writeError(w, r, http.StatusConflict, "error.max_images", "n", maxAttachmentsPerTask)
		} else {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
		}
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, r, http.StatusNotFound, "error.task_not_found")
		return
	}
	id, _ := res.LastInsertId()
	s.changed(userID(r))
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *server) handleAttachmentGet(w http.ResponseWriter, r *http.Request) {
	id, taskID, ok := attachmentPathIDs(w, r)
	if !ok {
		return
	}
	belongs, err := attachmentBelongsToTask(s.db, userID(r), id, taskID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if !belongs {
		writeError(w, r, http.StatusNotFound, "error.image_not_found")
		return
	}
	if notModified(w, r, fmt.Sprintf(`"a%d"`, id)) {
		return
	}
	var mime string
	var data []byte
	// task_id here too: between the check and the load, the image and its
	// task may have been deleted and the ID reassigned.
	err = s.db.QueryRow("SELECT mime, data FROM attachments WHERE id = ? AND task_id = ? "+
		"AND task_id IN (SELECT id FROM tasks WHERE list_id "+visibleLists+")", id, taskID, userID(r)).Scan(&mime, &data)
	if err != nil {
		writeError(w, r, http.StatusNotFound, "error.image_not_found")
		return
	}
	serveMime(w, mime)
	w.Write(data)
}

// handleAttachmentThumb serves the small preview; for existing images
// without a thumbnail, one is generated and stored on first request
// (lazy backfill).
func (s *server) handleAttachmentThumb(w http.ResponseWriter, r *http.Request) {
	id, taskID, ok := attachmentPathIDs(w, r)
	if !ok {
		return
	}
	belongs, err := attachmentBelongsToTask(s.db, userID(r), id, taskID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if !belongs {
		writeError(w, r, http.StatusNotFound, "error.image_not_found")
		return
	}
	if notModified(w, r, fmt.Sprintf(`"t%d"`, id)) {
		return
	}
	var mime string
	var thumb, data []byte
	if err := s.db.QueryRow("SELECT mime, thumb, data FROM attachments WHERE id = ? AND task_id = ? "+
		"AND task_id IN (SELECT id FROM tasks WHERE list_id "+visibleLists+")", id, taskID, userID(r)).Scan(&mime, &thumb, &data); err != nil {
		writeError(w, r, http.StatusNotFound, "error.image_not_found")
		return
	}
	if thumb == nil {
		if t, err := makeThumb(data); err == nil {
			thumb = t
			s.db.Exec("UPDATE attachments SET thumb = ? WHERE id = ?", t, id)
		}
	}
	if thumb != nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(thumb)
		return
	}
	serveMime(w, mime)
	w.Write(data)
}

func (s *server) handleAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	id, taskID, ok := attachmentPathIDs(w, r)
	if !ok {
		return
	}
	res, err := s.db.Exec("DELETE FROM attachments WHERE id = ? AND task_id = ? "+
		"AND task_id IN (SELECT id FROM tasks WHERE list_id "+visibleLists+")", id, taskID, userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, r, http.StatusNotFound, "error.image_not_found")
		return
	}
	s.changed(userID(r))
	w.WriteHeader(http.StatusNoContent)
}
