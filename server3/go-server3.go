package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"mime"
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

const (
	defaultHost      = "0.0.0.0"
	defaultPort      = "3000"
	cacheTTL         = 5 * time.Minute
)

type JsonResponse struct {
	Status   string      `json:"status"`
	Received interface{} `json:"received"`
}

type cacheEntry struct {
	ModTime    time.Time
	IsDir      bool
	LastAccess time.Time
}

var (
	fileCache  = make(map[string]cacheEntry)
	cacheMutex sync.RWMutex
)

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Clean expired cache entries
func cleanCache() {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	for path, entry := range fileCache {
		if time.Since(entry.LastAccess) > cacheTTL {
			delete(fileCache, path)
		}
	}
}

// Logging middleware
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("%s %s %d %v", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

// Serve files and directories from base directory
func handleBrowse(baseDirectory string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqPath := filepath.Clean(r.URL.Path)
		fsPath := filepath.Join(baseDirectory, reqPath)

		// Prevent path traversal
		if !strings.HasPrefix(fsPath, baseDirectory) {
			http.Error(w, "Not found", http.StatusNotFound)
			log.Printf("404: Path traversal attempt detected - %s", fsPath)
			return
		}

		// Check cache
		cacheMutex.RLock()
		cachedInfo, exists := fileCache[fsPath]
		cacheMutex.RUnlock()
		if exists {
			info, err := os.Stat(fsPath)
			if err == nil && info.ModTime().Equal(cachedInfo.ModTime) {
				cacheMutex.Lock()
				fileCache[fsPath] = cacheEntry{ModTime: info.ModTime(), IsDir: info.IsDir(), LastAccess: time.Now()}
				cacheMutex.Unlock()
				if !cachedInfo.IsDir {
					serveFile(w, r, fsPath, info)
					return
				}
			}
		}

		info, err := os.Stat(fsPath)
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			log.Printf("404: %s - %v", fsPath, err)
			return
		}

		// Update cache
		cacheMutex.Lock()
		fileCache[fsPath] = cacheEntry{ModTime: info.ModTime(), IsDir: info.IsDir(), LastAccess: time.Now()}
		cacheMutex.Unlock()

		if !info.IsDir() {
			serveFile(w, r, fsPath, info)
			return
		}

		files, err := os.ReadDir(fsPath)
		if err != nil {
			http.Error(w, "Failed to read directory", http.StatusInternalServerError)
			log.Printf("Error reading directory: %s - %v", fsPath, err)
			return
		}

		// Sort files for consistent display
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name() < files[j].Name()
		})

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		fmt.Fprintf(w, `
			<html>
			<head>
				<title>Index of %s</title>
				<style>
					body { font-family: Arial, sans-serif; margin: 20px; }
					h2 { color: #333; }
					ul { list-style: none; padding: 0; }
					li { padding: 5px 0; }
					a { text-decoration: none; color: #0066cc; }
					a:hover { text-decoration: underline; }
				</style>
			</head>
			<body>
				<h2>Index of %s</h2>
				<ul>`, html.EscapeString(reqPath), html.EscapeString(reqPath))

		if reqPath != "/" {
			parent := filepath.Dir(reqPath)
			if !strings.HasSuffix(parent, "/") {
				parent += "/"
			}
			fmt.Fprintf(w, `<li><a href="%s">..</a></li>`, html.EscapeString(parent))
		}

		for _, f := range files {
			if strings.HasPrefix(f.Name(), ".") {
				continue // Skip hidden files
			}
			name := f.Name()
			link := filepath.Join(reqPath, name)
			info, _ := f.Info()
			size := info.Size()
			modTime := info.ModTime().Format("2006-01-02 15:04:05")
			if f.IsDir() {
				link += "/"
				name += "/"
			}
			fmt.Fprintf(w, `<li><a href="%s">%s</a> (%d bytes, %s)</li>`, html.EscapeString(link), html.EscapeString(name), size, modTime)
		}
		fmt.Fprint(w, "</ul></body></html>")

		log.Printf("Listed directory: %s", fsPath)
	}
}

func serveFile(w http.ResponseWriter, r *http.Request, fsPath string, info os.FileInfo) {
	mimeType := mime.TypeByExtension(filepath.Ext(fsPath))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'")
	http.ServeFile(w, r, fsPath)
	log.Printf("Served file: %s", fsPath)
}

// Handle basic POST requests with JSON body
func handlePost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var requestData struct {
		Data string `json:"data"`
	}
	err := json.NewDecoder(r.Body).Decode(&requestData)
	if err != nil || requestData.Data == "" {
		http.Error(w, "Invalid or empty JSON data", http.StatusBadRequest)
		log.Printf("Invalid JSON from %s: %v", r.RemoteAddr, err)
		return
	}

	response := JsonResponse{
		Status:   "success",
		Received: requestData,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'")
	json.NewEncoder(w).Encode(response)

	log.Printf("Handled POST request from %s - Data: %v", r.RemoteAddr, requestData)
}

func main() {
	var baseDir, certFile, keyFile string
	flag.StringVar(&baseDir, "dir", ".", "Base directory to serve files from")
	flag.StringVar(&certFile, "cert", "", "TLS certificate file")
	flag.StringVar(&keyFile, "key", "", "TLS key file")
	flag.Parse()

	baseDirectory, err := filepath.Abs(baseDir)
	if err != nil {
		log.Fatalf("Failed to resolve base directory: %v", err)
	}

	host := os.Getenv("HOST")
	if host == "" {
		host = defaultHost
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/post", handlePost)
	mux.HandleFunc("/", handleBrowse(baseDirectory))

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", host, port),
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Periodic cache cleanup
	go func() {
		for range time.Tick(cacheTTL) {
			cleanCache()
		}
	}()

	log.Printf("Serving directory %s on http://%s:%s", baseDirectory, host, port)
	if certFile != "" && keyFile != "" {
		log.Printf("Using HTTPS with cert: %s, key: %s", certFile, keyFile)
		go func() {
			if err := server.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server error: %v", err)
			}
		}()
	} else {
		go func() {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server error: %v", err)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Shutdown error: %v", err)
	}
	log.Println("Server stopped.")
}
