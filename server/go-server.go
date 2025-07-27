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
    "strings"
    "syscall"
    "time"
)

const (
    HOST = "0.0.0.0"
    PORT = 3000
)

type JsonResponse struct {
    Status   string      `json:"status"`
    Received interface{} `json:"received"`
}

// Cache for file metadata
var fileCache = make(map[string]struct {
    ModTime time.Time
    IsDir   bool
})

// Serve files and directories from current working directory
func handleBrowse(w http.ResponseWriter, r *http.Request) {
    baseDirectory, _ := os.Getwd()
    reqPath := filepath.Clean(r.URL.Path)
    fsPath := filepath.Join(baseDirectory, reqPath)

    // Check if the requested path is within the base directory
    if !strings.HasPrefix(fsPath, baseDirectory) {
        http.NotFound(w, r)
        log.Printf("404: Path traversal attempt detected - %s", fsPath)
        return
    }

    // Check cache first
    if cachedInfo, exists := fileCache[fsPath]; exists {
        if !cachedInfo.IsDir {
            mimeType := mime.TypeByExtension(filepath.Ext(fsPath))
            if mimeType == "" {
                mimeType = "application/octet-stream"
            }
            w.Header().Set("Content-Type", mimeType)
            http.ServeFile(w, r, fsPath)
            log.Printf("Served file from cache: %s", fsPath)
            return
        }
    }

    info, err := os.Stat(fsPath)
    if err != nil {
        http.NotFound(w, r)
        log.Printf("404: %s", fsPath)
        return
    }

    // Update cache
    fileCache[fsPath] = struct {
        ModTime time.Time
        IsDir   bool
    }{
        ModTime: info.ModTime(),
        IsDir:   info.IsDir(),
    }

    if !info.IsDir() {
        mimeType := mime.TypeByExtension(filepath.Ext(fsPath))
        if mimeType == "" {
            mimeType = "application/octet-stream"
        }
        w.Header().Set("Content-Type", mimeType)
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

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    fmt.Fprintf(w, "<html><body><h2>Index of %s</h2><ul>", html.EscapeString(reqPath))

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
    var requestData map[string]interface{}
    err := json.NewDecoder(r.Body).Decode(&requestData)
    if err != nil {
        http.Error(w, "Invalid JSON", http.StatusBadRequest)
        log.Printf("Invalid JSON from %s", r.RemoteAddr)
        return
    }

    response := JsonResponse{
        Status:   "success",
        Received: requestData,
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(response)

    log.Printf("Handled POST request from %s - Data: %v", r.RemoteAddr, requestData)
}

func main() {
    mux := http.NewServeMux()
    mux.HandleFunc("/post", handlePost)
    mux.HandleFunc("/", handleBrowse)

    server := &http.Server{
        Addr:    fmt.Sprintf("%s:%d", HOST, PORT),
        Handler: mux,
    }

    log.Printf("Serving current directory on http://%s:%d", HOST, PORT)

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
