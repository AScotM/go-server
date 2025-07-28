package main

import (
    "encoding/json"
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
    defaultHost = "0.0.0.0"
    defaultPort = "3000"
)

type JsonResponse struct {
    Status   string      `json:"status"`
    Received interface{} `json:"received"`
}

var (
    fileCache = make(map[string]struct {
        ModTime time.Time
        IsDir   bool
    })
    cacheMutex sync.RWMutex
)

// Serve files and directories from current working directory
func handleBrowse(w http.ResponseWriter, r *http.Request) {
    baseDirectory, err := os.Getwd()
    if err != nil {
        http.Error(w, "Server error", http.StatusInternalServerError)
        log.Printf("Failed to get working directory: %v", err)
        return
    }
    reqPath := filepath.Clean(r.URL.Path)
    fsPath := filepath.Join(baseDirectory, reqPath)

    // Prevent path traversal
    if !strings.HasPrefix(fsPath, baseDirectory) {
        http.NotFound(w, r)
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
            if !cachedInfo.IsDir {
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
                log.Printf("Served file from cache: %s", fsPath)
                return
            }
        }
    }

    info, err := os.Stat(fsPath)
    if err != nil {
        http.NotFound(w, r)
        log.Printf("404: %s - %v", fsPath, err)
        return
    }

    // Update cache
    cacheMutex.Lock()
    fileCache[fsPath] = struct {
        ModTime time.Time
        IsDir   bool
    }{
        ModTime: info.ModTime(),
        IsDir:   info.IsDir(),
    }
    cacheMutex.Unlock()

    if !info.IsDir() {
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
        name := f.Name()
        link := filepath.Join(reqPath, name)
        if f.IsDir() {
            link += "/"
            name += "/"
        }
        fmt.Fprintf(w, `<li><a href="%s">%s</a></li>`, html.EscapeString(link), html.EscapeString(name))
    }
    fmt.Fprint(w, "</ul></body></html>")

    log.Printf("Listed directory: %s", fsPath)
}

// Handle basic POST requests with JSON body
func handlePost(w http.ResponseWriter, r *http.Request) {
    // Limit request body size to 1MB
    r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)

    var requestData interface{}
    err := json.NewDecoder(r.Body).Decode(&requestData)
    if err != nil {
        http.Error(w, "Invalid JSON", http.StatusBadRequest)
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
    // Get host and port from environment variables
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
    mux.HandleFunc("/", handleBrowse)

    server := &http.Server{
        Addr:         fmt.Sprintf("%s:%s", host, port),
        Handler:      mux,
        ReadTimeout:  10 * time.Second,
        WriteTimeout: 10 * time.Second,
    }

    log.Printf("Serving current directory on http://%s:%s", host, port)

    go func() {
        if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatalf("Server error: %v", err)
        }
    }()

    stop := make(chan os.Signal, 1)
    signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
    <-stop

    log.Println("Shutting down server...")
    server.Close()
    log.Println("Server stopped.")
}
