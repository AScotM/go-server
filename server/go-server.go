package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// Define host and port
const (
	HOST = "0.0.0.0"
	PORT = 3000
)

// JSON response structure
type JsonResponse struct {
	Status   string      `json:"status"`
	Received interface{} `json:"received"`
}

// Handle GET requests
func handleGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "<html><body><h1>Go Web Server</h1></body></html>")
	log.Printf("Handled GET request from %s", r.RemoteAddr)
}

// Handle POST requests (JSON parsing)
func handlePost(w http.ResponseWriter, r *http.Request) {
	var requestData map[string]interface{}

	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&requestData)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		log.Printf("Invalid JSON received from %s", r.RemoteAddr)
		return
	}

	response := JsonResponse{
		Status:   "success",
		Received: requestData,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)

	log.Printf("Handled POST request from %s - Data: %v", r.RemoteAddr, requestData)
}

// Main function: Start HTTP server
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleGet)
	mux.HandleFunc("/post", handlePost)

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", HOST, PORT),
		Handler: mux,
	}

	log.Printf("Serving on http://%s:%d", HOST, PORT)

	// Handle graceful shutdown
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("\nShutting down server...")
	server.Close()
	log.Println("Server stopped.")
}
