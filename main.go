package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Upgrader to convert HTTP to WebSocket
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Store active WebSocket connections for each game session
var sessions = make(map[string][]*websocket.Conn)
var mu sync.Mutex // Mutex to protect sessions map

// Store connections for /all-ws endpoint to send real-time updates
var allWsConnections = make([]*websocket.Conn, 0)
var allWsMu sync.Mutex // Mutex to protect allWsConnections

// Create a new host
func handleHost(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Host upgrade error:", err)
		return
	}

	mu.Lock()
	sessions[id] = append(sessions[id], conn)
	mu.Unlock()

	log.Printf("Host connected to session %s", id)
	broadcastSessionUpdate()
	handleMessages(conn, id)
}

// Join an existing session
func handleJoin(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Join upgrade error:", err)
		return
	}

	mu.Lock()
	sessions[id] = append(sessions[id], conn)
	mu.Unlock()

	log.Printf("Player joined session %s", id)
	broadcastSessionUpdate()
	handleMessages(conn, id)
}

// Handle incoming messages and broadcast them
func handleMessages(conn *websocket.Conn, sessionID string) {
	defer func() {
		conn.Close()
		removeConnection(sessionID, conn)
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			break
		}

		log.Printf("Message from session %s: %s", sessionID, msg)
		broadcastMessage(sessionID, msg)
	}
}

// Broadcast message to all connections in the session
func broadcastMessage(sessionID string, message []byte) {
	mu.Lock()
	defer mu.Unlock()

	for _, conn := range sessions[sessionID] {
		if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
			log.Println("Write error:", err)
		}
	}
}

// Remove a connection from the session
func removeConnection(sessionID string, conn *websocket.Conn) {
	mu.Lock()
	conns := sessions[sessionID]
	for i, c := range conns {
		if c == conn {
			sessions[sessionID] = append(conns[:i], conns[i+1:]...)
			// Clean up empty sessions
			if len(sessions[sessionID]) == 0 {
				delete(sessions, sessionID)
			}
			break
		}
	}
	mu.Unlock()

	// Broadcast session update outside of lock to avoid deadlock
	broadcastSessionUpdate()
}

// Broadcast current session state to all /all-ws connections
func broadcastSessionUpdate() {
	mu.Lock()
	sessionInfo := make(map[string]int)
	for id, conns := range sessions {
		sessionInfo[id] = len(conns)
	}
	mu.Unlock()

	data, err := json.Marshal(sessionInfo)
	if err != nil {
		log.Println("Error marshalling session info:", err)
		return
	}

	allWsMu.Lock()
	defer allWsMu.Unlock()

	// Collect failed connections to remove
	var failedIndices []int
	for i, conn := range allWsConnections {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Println("Write error to /all-ws:", err)
			failedIndices = append(failedIndices, i)
		}
	}

	// Remove failed connections in reverse order to maintain indices
	for i := len(failedIndices) - 1; i >= 0; i-- {
		idx := failedIndices[i]
		allWsConnections = append(allWsConnections[:idx], allWsConnections[idx+1:]...)
	}
}

func handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	// Close all WebSocket connections in the session
	if conns, exists := sessions[id]; exists {
		for _, conn := range conns {
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Game closed"))
			conn.Close()
		}
		delete(sessions, id) // Remove the session from the map
		log.Printf("Game session %s closed", id)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Game closed"))

		// Broadcast session update
		go broadcastSessionUpdate()
	} else {
		http.Error(w, "Session not found", http.StatusNotFound)
	}
}

func all(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	// Create a map of session IDs to connection counts
	sessionInfo := make(map[string]int)
	for id, conns := range sessions {
		sessionInfo[id] = len(conns)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessionInfo)
}

// WebSocket handler for real-time session updates
func allWs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("All-ws upgrade error:", err)
		return
	}

	allWsMu.Lock()
	allWsConnections = append(allWsConnections, conn)
	allWsMu.Unlock()

	log.Println("Client connected to /all-ws")

	// Send initial session state
	broadcastSessionUpdate()

	// Keep connection open and listen for disconnections
	defer func() {
		allWsMu.Lock()
		defer allWsMu.Unlock()
		for i, c := range allWsConnections {
			if c == conn {
				allWsConnections = append(allWsConnections[:i], allWsConnections[i+1:]...)
				break
			}
		}
		conn.Close()
		log.Println("Client disconnected from /all-ws")
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func main() {
	http.HandleFunc("/host", handleHost)
	http.HandleFunc("/join", handleJoin)
	http.HandleFunc("/all", all)
	http.HandleFunc("/all-ws", allWs)

	http.HandleFunc("/close", handleClose)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	fmt.Println("Server started on :8000")
	log.Fatal(http.ListenAndServe(":8000", nil))
}
