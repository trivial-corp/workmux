package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A one-pixel PNG, so the sniffing is exercised on real magic bytes.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	0x1f, 0x15, 0xc4, 0x89, 0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func upload(t *testing.T, h http.Handler, body []byte, contentType string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/upload", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Pasting a screenshot into a terminal: the image becomes a file, and the path is
// what gets typed. This is that half.
func TestUploadStoresAnImage(t *testing.T) {
	_, h, _, _ := withSessions(t)

	code, got := upload(t, h, onePixelPNG, "image/png")
	if code != 200 {
		t.Fatalf("code = %d, %v", code, got)
	}
	path, _ := got["path"].(string)
	if path == "" {
		t.Fatalf("no path: %v", got)
	}
	on, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("nothing at %s: %v", path, err)
	}
	if !bytes.Equal(on, onePixelPNG) {
		t.Error("the file on disk isn't what was uploaded")
	}
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("path = %q, want a .png extension from the sniffed type", path)
	}
	// It must land outside any repository: a pasted screenshot is not a change to
	// the project, and a file in a worktree would turn up in its diff.
	if strings.Contains(path, "proj") {
		t.Errorf("path = %q, want it outside the repo", path)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

// The header comes from the page; the bytes decide. Otherwise "Content-Type:
// image/png" on a shell script would pick the extension that goes in a command line.
func TestUploadSniffsRatherThanTrusts(t *testing.T) {
	_, h, _, _ := withSessions(t)

	code, got := upload(t, h, []byte("#!/bin/sh\nrm -rf /\n"), "image/png")
	if code == 200 {
		t.Fatalf("a script claiming to be a png was accepted: %v", got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "images") {
		t.Errorf("error = %q, want it to say only images", msg)
	}
	// And a real PNG with a wrong header still works, because the bytes are right.
	if code, got := upload(t, h, onePixelPNG, "text/plain"); code != 200 {
		t.Errorf("a real png was refused over its header: %d %v", code, got)
	} else if p, _ := got["path"].(string); p != "" {
		t.Cleanup(func() { _ = os.Remove(p) })
	}
}

func TestUploadLimits(t *testing.T) {
	_, h, _, _ := withSessions(t)

	if code, _ := upload(t, h, nil, "image/png"); code == 200 {
		t.Error("an empty upload should be refused")
	}
	// Larger than the cap, and still a real PNG at the front so the type check
	// isn't what rejects it.
	big := append(append([]byte(nil), onePixelPNG...), bytes.Repeat([]byte{0}, maxUpload+1)...)
	code, got := upload(t, h, big, "image/png")
	if code == 200 {
		t.Errorf("an oversized upload was accepted: %v", got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "too big") {
		t.Errorf("error = %q, want it to say what the limit is", msg)
	}
}

func TestUploadMethod(t *testing.T) {
	_, h, _, _ := withSessions(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/upload", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rec.Code)
	}
}

// Uploads are guarded like everything else: an off-box client without the token
// must not be able to write files onto the machine.
func TestUploadNeedsTheToken(t *testing.T) {
	s, h, _, _ := withSessions(t)
	s.Token = "sekret"

	req := httptest.NewRequest("POST", "/api/upload", bytes.NewReader(onePixelPNG))
	req.RemoteAddr = "192.168.1.50:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}
