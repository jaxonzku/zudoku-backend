package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	playerHost          = "host"
	playerJoin          = "join"
	boardColumns        = 9
	boardRows           = 13
	moveCooldownMS      = 500
	initialPowerupCount = 3
	maxInventorySize    = 7
	shotHighlightMS     = 450
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type position struct {
	Column int `json:"column"`
	Row    int `json:"row"`
}

type powerup struct {
	ID        string   `json:"id"`
	Direction string   `json:"direction"`
	Emoji     string   `json:"emoji"`
	Position  position `json:"position,omitempty"`
}

type shotState struct {
	Shooter   string     `json:"shooter"`
	Direction string     `json:"direction"`
	Cells     []position `json:"cells"`
	HitPlayer string     `json:"hitPlayer,omitempty"`
	ExpiresAt int64      `json:"expiresAt"`
}

type boardState struct {
	Host           position   `json:"host"`
	Join           position   `json:"join"`
	Ready          bool       `json:"ready"`
	HostAlive      bool       `json:"hostAlive"`
	JoinAlive      bool       `json:"joinAlive"`
	HostNextMoveAt int64      `json:"hostNextMoveAt"`
	JoinNextMoveAt int64      `json:"joinNextMoveAt"`
	HostInventory  []powerup  `json:"hostInventory"`
	JoinInventory  []powerup  `json:"joinInventory"`
	Powerups       []powerup  `json:"powerups"`
	LastShot       *shotState `json:"lastShot,omitempty"`
	GameOver       bool       `json:"gameOver"`
	Winner         string     `json:"winner,omitempty"`
}

type clientMessage struct {
	Type      string `json:"type"`
	Direction string `json:"direction,omitempty"`
	Message   string `json:"message,omitempty"`
	PowerID   string `json:"powerId,omitempty"`
}

type serverMessage struct {
	Type      string         `json:"type"`
	GameID    string         `json:"gameId,omitempty"`
	Player    string         `json:"player,omitempty"`
	Message   string         `json:"message,omitempty"`
	Board     *boardState    `json:"board,omitempty"`
	Sessions  map[string]int `json:"sessions,omitempty"`
	Timestamp int64          `json:"timestamp"`
}

type sessionSnapshot struct {
	ID          string
	Connections int
}

type client struct {
	conn    *websocket.Conn
	session *session
	role    string
	sendMu  sync.Mutex
}

type session struct {
	id      string
	host    *client
	join    *client
	board   boardState
	manager *sessionManager
	mu      sync.Mutex
}

type sessionManager struct {
	sessions     map[string]*session
	mu           sync.RWMutex
	powerCounter int64
}

type powerupTemplate struct {
	Direction string
	Emoji     string
}

var powerupTemplates = []powerupTemplate{
	{Direction: "up", Emoji: "↑"},
	{Direction: "right", Emoji: "→"},
	{Direction: "down", Emoji: "↓"},
	{Direction: "left", Emoji: "←"},
}

func newSessionManager() *sessionManager {
	rand.Seed(time.Now().UnixNano())

	return &sessionManager{
		sessions: make(map[string]*session),
	}
}

func (m *sessionManager) createSession(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[id]; exists {
		return nil, fmt.Errorf("session %q already exists", id)
	}

	now := time.Now().UnixMilli()
	s := &session{
		id: id,
		board: boardState{
			Host:           position{Column: 0, Row: 0},
			Join:           position{Column: boardColumns - 1, Row: boardRows - 1},
			Ready:          false,
			HostAlive:      true,
			JoinAlive:      true,
			HostNextMoveAt: now,
			JoinNextMoveAt: now,
			HostInventory:  []powerup{},
			JoinInventory:  []powerup{},
			Powerups:       []powerup{},
		},
		manager: m,
	}
	s.spawnPowerupsLocked(initialPowerupCount)
	m.sessions[id] = s

	return s, nil
}

func (m *sessionManager) getSession(id string) (*session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[id]
	return s, ok
}

func (m *sessionManager) removeSession(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, id)
}

func (m *sessionManager) nextPowerupID(sessionID string) string {
	value := atomic.AddInt64(&m.powerCounter, 1)
	return fmt.Sprintf("%s-power-%d", sessionID, value)
}

func (m *sessionManager) snapshots() []sessionSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshots := make([]sessionSnapshot, 0, len(m.sessions))
	for id, session := range m.sessions {
		session.mu.Lock()
		connections := 0
		if session.host != nil {
			connections++
		}
		if session.join != nil {
			connections++
		}
		session.mu.Unlock()

		snapshots = append(snapshots, sessionSnapshot{
			ID:          id,
			Connections: connections,
		})
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ID < snapshots[j].ID
	})

	return snapshots
}

func (m *sessionManager) sessionsPayload() map[string]int {
	snapshots := m.snapshots()
	payload := make(map[string]int, len(snapshots))
	for _, snapshot := range snapshots {
		payload[snapshot.ID] = snapshot.Connections
	}

	return payload
}

func (m *sessionManager) broadcastSessions() {
	payload := serverMessage{
		Type:      "sessions_update",
		Sessions:  m.sessionsPayload(),
		Timestamp: time.Now().UnixMilli(),
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, session := range m.sessions {
		session.broadcast(payload)
	}
}

func (s *session) attachClient(c *client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch c.role {
	case playerHost:
		if s.host != nil {
			return errors.New("host is already connected")
		}
		s.host = c
	case playerJoin:
		if s.join != nil {
			return errors.New("join player is already connected")
		}
		s.join = c
	default:
		return fmt.Errorf("unknown role %q", c.role)
	}

	s.board.Ready = s.host != nil && s.join != nil

	return nil
}

func (s *session) detachClient(c *client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch c.role {
	case playerHost:
		if s.host == c {
			s.host = nil
		}
	case playerJoin:
		if s.join == c {
			s.join = nil
		}
	}

	s.board.Ready = s.host != nil && s.join != nil

	return s.host == nil && s.join == nil
}

func (s *session) currentBoard() boardState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.snapshotLocked(time.Now().UnixMilli())
}

func (s *session) handleMove(role string, direction string) (boardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validatePlayerActionLocked(role); err != nil {
		return s.snapshotLocked(time.Now().UnixMilli()), err
	}

	now := time.Now().UnixMilli()
	var target *position
	var nextMoveAt *int64
	switch role {
	case playerHost:
		target = &s.board.Host
		nextMoveAt = &s.board.HostNextMoveAt
	case playerJoin:
		target = &s.board.Join
		nextMoveAt = &s.board.JoinNextMoveAt
	default:
		return s.snapshotLocked(now), fmt.Errorf("unknown role %q", role)
	}

	if now < *nextMoveAt {
		return s.snapshotLocked(now), errors.New("player cooldown is still active")
	}

	next := *target
	switch direction {
	case "up":
		next.Row--
	case "down":
		next.Row++
	case "left":
		next.Column--
	case "right":
		next.Column++
	default:
		return s.snapshotLocked(now), fmt.Errorf("invalid direction %q", direction)
	}

	if next.Column < 0 || next.Column >= boardColumns || next.Row < 0 || next.Row >= boardRows {
		return s.snapshotLocked(now), errors.New("move is out of bounds")
	}

	*target = next
	*nextMoveAt = now + moveCooldownMS
	s.collectPowerupLocked(role, next)

	return s.snapshotLocked(now), nil
}

func (s *session) handleUsePower(role string, powerID string) (boardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validatePlayerActionLocked(role); err != nil {
		return s.snapshotLocked(time.Now().UnixMilli()), err
	}
	if powerID == "" {
		return s.snapshotLocked(time.Now().UnixMilli()), errors.New("missing power id")
	}

	var inventory *[]powerup
	var origin position
	var targetPos position
	var targetAlive *bool
	switch role {
	case playerHost:
		inventory = &s.board.HostInventory
		origin = s.board.Host
		targetPos = s.board.Join
		targetAlive = &s.board.JoinAlive
	case playerJoin:
		inventory = &s.board.JoinInventory
		origin = s.board.Join
		targetPos = s.board.Host
		targetAlive = &s.board.HostAlive
	default:
		return s.snapshotLocked(time.Now().UnixMilli()), fmt.Errorf("unknown role %q", role)
	}

	index := -1
	var selected powerup
	for i, item := range *inventory {
		if item.ID == powerID {
			index = i
			selected = item
			break
		}
	}
	if index == -1 {
		return s.snapshotLocked(time.Now().UnixMilli()), errors.New("powerup not found in inventory")
	}

	*inventory = append((*inventory)[:index], (*inventory)[index+1:]...)

	path := s.computeShotPath(origin, selected.Direction)
	shot := &shotState{
		Shooter:   role,
		Direction: selected.Direction,
		Cells:     path,
		ExpiresAt: time.Now().UnixMilli() + shotHighlightMS,
	}

	if *targetAlive {
		for _, cell := range path {
			if cell == targetPos {
				shot.HitPlayer = s.otherRole(role)
				*targetAlive = false
				s.board.GameOver = true
				s.board.Winner = role
				break
			}
		}
	}

	s.board.LastShot = shot

	return s.snapshotLocked(time.Now().UnixMilli()), nil
}

func (s *session) validatePlayerActionLocked(role string) error {
	if !s.board.Ready {
		return errors.New("waiting for both players to connect")
	}
	if s.board.GameOver {
		return errors.New("game is already over")
	}

	switch role {
	case playerHost:
		if !s.board.HostAlive {
			return errors.New("host is dead")
		}
	case playerJoin:
		if !s.board.JoinAlive {
			return errors.New("join player is dead")
		}
	default:
		return fmt.Errorf("unknown role %q", role)
	}

	return nil
}

func (s *session) otherRole(role string) string {
	if role == playerHost {
		return playerJoin
	}
	return playerHost
}

func (s *session) collectPowerupLocked(role string, pos position) {
	if s.board.GameOver {
		return
	}

	var inventory *[]powerup
	switch role {
	case playerHost:
		inventory = &s.board.HostInventory
	case playerJoin:
		inventory = &s.board.JoinInventory
	default:
		return
	}

	if len(*inventory) >= maxInventorySize {
		return
	}

	index := -1
	for i, item := range s.board.Powerups {
		if item.Position == pos {
			index = i
			break
		}
	}
	if index == -1 {
		return
	}

	collected := s.board.Powerups[index]
	collected.Position = position{}
	*inventory = append(*inventory, collected)
	s.board.Powerups = append(s.board.Powerups[:index], s.board.Powerups[index+1:]...)

	for len(s.board.Powerups) < initialPowerupCount {
		s.spawnPowerupLocked()
	}
}

func (s *session) computeShotPath(origin position, direction string) []position {
	path := make([]position, 0, max(boardColumns, boardRows))
	current := origin

	for {
		switch direction {
		case "up":
			current.Row--
		case "down":
			current.Row++
		case "left":
			current.Column--
		case "right":
			current.Column++
		default:
			return path
		}

		if current.Column < 0 || current.Column >= boardColumns || current.Row < 0 || current.Row >= boardRows {
			return path
		}

		path = append(path, current)
	}
}

func (s *session) spawnPowerupsLocked(count int) {
	for i := 0; i < count; i++ {
		s.spawnPowerupLocked()
	}
}

func (s *session) spawnPowerupLocked() {
	free := s.randomFreePositionLocked()
	template := powerupTemplates[rand.Intn(len(powerupTemplates))]
	s.board.Powerups = append(s.board.Powerups, powerup{
		ID:        s.manager.nextPowerupID(s.id),
		Direction: template.Direction,
		Emoji:     template.Emoji,
		Position:  free,
	})
}

func (s *session) randomFreePositionLocked() position {
	occupied := make(map[position]bool, len(s.board.Powerups)+2)
	occupied[s.board.Host] = true
	occupied[s.board.Join] = true
	for _, item := range s.board.Powerups {
		occupied[item.Position] = true
	}

	for {
		candidate := position{
			Column: rand.Intn(boardColumns),
			Row:    rand.Intn(boardRows),
		}
		if !occupied[candidate] {
			return candidate
		}
	}
}

func (s *session) snapshotLocked(now int64) boardState {
	snapshot := s.board
	snapshot.HostInventory = append(
		make([]powerup, 0, len(s.board.HostInventory)),
		s.board.HostInventory...,
	)
	snapshot.JoinInventory = append(
		make([]powerup, 0, len(s.board.JoinInventory)),
		s.board.JoinInventory...,
	)
	snapshot.Powerups = append(
		make([]powerup, 0, len(s.board.Powerups)),
		s.board.Powerups...,
	)

	if s.board.LastShot != nil && s.board.LastShot.ExpiresAt > now {
		cells := append([]position(nil), s.board.LastShot.Cells...)
		snapshot.LastShot = &shotState{
			Shooter:   s.board.LastShot.Shooter,
			Direction: s.board.LastShot.Direction,
			Cells:     cells,
			HitPlayer: s.board.LastShot.HitPlayer,
			ExpiresAt: s.board.LastShot.ExpiresAt,
		}
	} else {
		snapshot.LastShot = nil
	}

	return snapshot
}

func (s *session) broadcast(message serverMessage) {
	s.mu.Lock()
	clients := []*client{s.host, s.join}
	s.mu.Unlock()

	for _, c := range clients {
		if c == nil {
			continue
		}
		if err := c.send(message); err != nil {
			log.Printf("broadcast error for session %s: %v", s.id, err)
		}
	}
}

func (c *client) send(message serverMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	return c.conn.WriteJSON(message)
}

func (c *client) readLoop() {
	defer c.cleanup()

	for {
		var message clientMessage
		if err := c.conn.ReadJSON(&message); err != nil {
			log.Printf("read error (%s:%s): %v", c.session.id, c.role, err)
			return
		}

		log.Printf(
			"message (%s:%s) type=%s direction=%s powerId=%s message=%q",
			c.session.id,
			c.role,
			message.Type,
			message.Direction,
			message.PowerID,
			message.Message,
		)

		if err := c.handleMessage(message); err != nil {
			log.Printf("message error (%s:%s): %v", c.session.id, c.role, err)
			_ = c.send(serverMessage{
				Type:      "error",
				GameID:    c.session.id,
				Player:    c.role,
				Message:   err.Error(),
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}
}

func (c *client) handleMessage(message clientMessage) error {
	switch message.Type {
	case "move":
		board, err := c.session.handleMove(c.role, message.Direction)
		if err != nil {
			return err
		}

		c.session.broadcast(serverMessage{
			Type:      "state_update",
			GameID:    c.session.id,
			Player:    c.role,
			Board:     boardPointer(board),
			Timestamp: time.Now().UnixMilli(),
		})
	case "use_power":
		board, err := c.session.handleUsePower(c.role, message.PowerID)
		if err != nil {
			return err
		}

		c.session.broadcast(serverMessage{
			Type:      "state_update",
			GameID:    c.session.id,
			Player:    c.role,
			Board:     boardPointer(board),
			Timestamp: time.Now().UnixMilli(),
		})
	case "ping":
		return c.send(serverMessage{
			Type:      "pong",
			GameID:    c.session.id,
			Player:    c.role,
			Timestamp: time.Now().UnixMilli(),
		})
	case "chat":
		c.session.broadcast(serverMessage{
			Type:      "chat",
			GameID:    c.session.id,
			Player:    c.role,
			Message:   message.Message,
			Timestamp: time.Now().UnixMilli(),
		})
	default:
		return fmt.Errorf("unsupported message type %q", message.Type)
	}

	return nil
}

func (c *client) cleanup() {
	isEmpty := c.session.detachClient(c)
	if isEmpty {
		c.session.manager.removeSession(c.session.id)
	} else {
		c.session.broadcast(serverMessage{
			Type:      "player_left",
			GameID:    c.session.id,
			Player:    c.role,
			Board:     boardPointer(c.session.currentBoard()),
			Timestamp: time.Now().UnixMilli(),
		})
	}

	c.session.manager.broadcastSessions()

	if err := c.conn.Close(); err != nil {
		log.Printf("close error (%s:%s): %v", c.session.id, c.role, err)
	}
}

func boardPointer(board boardState) *boardState {
	copy := board
	return &copy
}

func upgradeConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}

	conn.SetReadLimit(4096)
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("set read deadline error: %v", err)
	}

	return conn, nil
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	}); err != nil {
		log.Printf("http error response write failed: %v", err)
	}
}

func withSessionID(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeHTTPError(w, http.StatusBadRequest, "missing query param: id")
			return
		}

		next(w, r, id)
	}
}

func main() {
	manager := newSessionManager()

	http.HandleFunc("/host", withSessionID(func(w http.ResponseWriter, r *http.Request, id string) {
		session, err := manager.createSession(id)
		if err != nil {
			writeHTTPError(w, http.StatusConflict, err.Error())
			return
		}

		conn, err := upgradeConnection(w, r)
		if err != nil {
			manager.removeSession(id)
			log.Printf("host upgrade error: %v", err)
			return
		}

		client := &client{
			conn:    conn,
			session: session,
			role:    playerHost,
		}
		if err := session.attachClient(client); err != nil {
			_ = conn.Close()
			manager.removeSession(id)
			writeHTTPError(w, http.StatusConflict, err.Error())
			return
		}

		if err := client.send(serverMessage{
			Type:      "connected",
			GameID:    id,
			Player:    playerHost,
			Board:     boardPointer(session.currentBoard()),
			Timestamp: time.Now().UnixMilli(),
		}); err != nil {
			log.Printf("host initial send error: %v", err)
		}

		manager.broadcastSessions()
		go client.readLoop()
	}))

	http.HandleFunc("/join", withSessionID(func(w http.ResponseWriter, r *http.Request, id string) {
		session, ok := manager.getSession(id)
		if !ok {
			writeHTTPError(w, http.StatusNotFound, "session not found")
			return
		}

		conn, err := upgradeConnection(w, r)
		if err != nil {
			log.Printf("join upgrade error: %v", err)
			return
		}

		client := &client{
			conn:    conn,
			session: session,
			role:    playerJoin,
		}
		if err := session.attachClient(client); err != nil {
			_ = conn.Close()
			writeHTTPError(w, http.StatusConflict, err.Error())
			return
		}

		board := session.currentBoard()
		if err := client.send(serverMessage{
			Type:      "connected",
			GameID:    id,
			Player:    playerJoin,
			Board:     boardPointer(board),
			Timestamp: time.Now().UnixMilli(),
		}); err != nil {
			log.Printf("join initial send error: %v", err)
		}

		session.broadcast(serverMessage{
			Type:      "player_joined",
			GameID:    id,
			Player:    playerJoin,
			Board:     boardPointer(board),
			Timestamp: time.Now().UnixMilli(),
		})
		manager.broadcastSessions()
		go client.readLoop()
	}))

	http.HandleFunc("/close", withSessionID(func(w http.ResponseWriter, r *http.Request, id string) {
		session, ok := manager.getSession(id)
		if !ok {
			writeHTTPError(w, http.StatusNotFound, "session not found")
			return
		}

		session.broadcast(serverMessage{
			Type:      "session_closed",
			GameID:    id,
			Timestamp: time.Now().UnixMilli(),
		})

		session.mu.Lock()
		host := session.host
		join := session.join
		session.host = nil
		session.join = nil
		session.board.Ready = false
		session.mu.Unlock()

		if host != nil {
			_ = host.conn.Close()
		}
		if join != nil {
			_ = join.conn.Close()
		}

		manager.removeSession(id)
		manager.broadcastSessions()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status": "closed",
			"id":     id,
		}); err != nil {
			log.Printf("close response write error: %v", err)
		}
	}))

	http.HandleFunc("/all-ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgradeConnection(w, r)
		if err != nil {
			log.Printf("all-ws upgrade error: %v", err)
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				log.Printf("all-ws close error: %v", err)
			}
		}()

		for {
			if err := conn.WriteJSON(manager.sessionsPayload()); err != nil {
				log.Printf("all-ws write error: %v", err)
				return
			}
			time.Sleep(2 * time.Second)
		}
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
		}); err != nil {
			log.Printf("healthz response write error: %v", err)
		}
	})

	log.Println("multiplayer websocket server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func max(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
