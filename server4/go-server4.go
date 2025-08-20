package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	addr          = flag.String("addr", ":8080", "HTTP network address")
	baseDir       = flag.String("dir", ".", "Base directory to serve")
	cacheTTL      = flag.Duration("cache", 10*time.Second, "Cache TTL")
	certFile      = flag.String("cert", "", "TLS certificate file")
	keyFile       = flag.String("key", "", "TLS key file")
)

type cacheEntry struct {
	info       os.FileInfo
	modTime    time.Time
	lastAccess time.Time
}

var (
	cache   = make(map[string]cacheEntry)
	cacheMu sync.Mutex
)

func getFromCache(path string) (os.FileInfo, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if entry, ok := cache[path]; ok {
		if time.Since(entry.lastAccess) < *cacheTTL {
			entry.lastAccess = time.Now()
			cache[path] = entry
			return entry.info, true
		}
		delete(cache, path)
	}
	return nil, false
}

func putInCache(path string, info os.FileInfo) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache[path] = cacheEntry{info: info, modTime: info.ModTime(), lastAccess: time.Now()}
}

func cleanCache(stop <-chan struct{}) {
	ticker := time.NewTicker(*cacheTTL)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cacheMu.Lock()
			for path, entry := range cache {
				if time.Since(entry.lastAccess) > *cacheTTL {
					delete(cache, path)
				}
			}
			cacheMu.Unlock()
		case <-stop:
			return
		}
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.status = code
	lrw.ResponseWriter.WriteHeader(code)
}

func logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		duration := time.Since(start)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lrw.status, duration)
	})
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func fileHandler(w http.ResponseWriter, r *http.Request) {
	relPath := filepath.Clean(r.URL.Path)
	fsPath := filepath.Join(*baseDir, relPath)

	// safer path traversal check
	rel, err := filepath.Rel(*baseDir, fsPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	var info os.FileInfo
	if cached, ok := getFromCache(fsPath); ok {
		info = cached
	} else {
		info, err = os.Stat(fsPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		putInCache(fsPath, info)
	}

	if info.IsDir() {
		dirList(w, r, fsPath, relPath)
		return
	}

	http.ServeFile(w, r, fsPath)
}

func dirList(w http.ResponseWriter, r *http.Request, fsPath, relPath string) {
	files, err := os.ReadDir(fsPath)
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir() != files[j].IsDir() {
			return files[i].IsDir()
		}
		return files[i].Name() < files[j].Name()
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<html><head><title>Index of %s</title></head><body>", html.EscapeString(relPath))
	fmt.Fprintf(w, "<h1>Index of %s</h1><ul>", html.EscapeString(relPath))

	if relPath != "/" {
		parent := filepath.Dir(relPath)
		if parent == "." {
			parent = "/"
		}
		fmt.Fprintf(w, `<li><a href="%s">..</a></li>`, template.HTMLEscapeString(parent))
	}

	for _, f := range files {
		name := f.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(relPath, name)
		if f.IsDir() {
			path += "/"
		}
		info, _ := f.Info()
		fmt.Fprintf(w, `<li><a href="%s">%s</a> %d bytes %s</li>`,
			template.HTMLEscapeString(path),
			template.HTMLEscapeString(name),
			info.Size(),
			info.ModTime().Format(time.RFC3339))
	}
	fmt.Fprint(w, "</ul></body></html>")
}

func apiHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	resp := map[string]interface{}{
		"received": payload,
		"time":     time.Now(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func main() {
	flag.Parse()

	stop := make(chan struct{})
	go cleanCache(stop)

	mux := http.NewServeMux()
	mux.HandleFunc("/api", apiHandler)
	mux.HandleFunc("/", fileHandler)

	handler := logger(secureHeaders(mux))

	srv := &http.Server{
		Addr:         *addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		var err error
		if *certFile != "" && *keyFile != "" {
			log.Printf("Starting HTTPS on %s", *addr)
			err = srv.ListenAndServeTLS(*certFile, *keyFile)
		} else {
			log.Printf("Starting HTTP on %s", *addr)
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server Shutdown: %v", err)
	}
	log.Println("Server gracefully stopped")
}
