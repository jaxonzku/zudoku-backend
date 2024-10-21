package main

import (
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
	defer mu.Unlock()

	conns := sessions[sessionID]
	for i, c := range conns {
		if c == conn {
			sessions[sessionID] = append(conns[:i], conns[i+1:]...)
			break
		}
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
	} else {
		http.Error(w, "Session not found", http.StatusNotFound)
	}
}

func main() {
	http.HandleFunc("/host", handleHost)
	http.HandleFunc("/join", handleJoin)
	http.HandleFunc("/close", handleClose)

	fmt.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
