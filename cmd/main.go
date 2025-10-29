package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/a-h/templ"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"whiteboard/internal/views"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type Client struct {
	id   string
	conn *websocket.Conn
	send chan []byte
	room *Room
}

func (c *Client) readPump() {
	defer func() {
		c.room.unregister <- c
		c.conn.Close()
	}()
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}
		c.room.broadcast <- message
	}
}

func (c *Client) writePump() {
	defer func() {
		c.conn.Close()
	}()
	for message := range c.send {
		err := c.conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			log.Printf("write error: %v", err)
			break
		}
	}
}

type Room struct {
	id         string
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	hub        *Hub

	History []json.RawMessage `json:"lines"`
}

func newRoom(id string, hub *Hub) *Room {
	return &Room{
		id:         id,
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		hub:        hub,
		History:    make([]json.RawMessage, 0),
	}
}

func (r *Room) run() {
	for {
		select {
		case client := <-r.register:
			r.clients[client] = true
			log.Printf("Client %s registered to room %s", client.id, r.id)

			welcomeMsg, err := json.Marshal(map[string]interface{}{
				"type":     "welcome",
				"clientID": client.id,
			})
			if err != nil {
				log.Printf("Error marshaling welcome message: %v", err)
			} else {
				client.send <- welcomeMsg
			}

			historyMsg, err := json.Marshal(map[string]interface{}{
				"type":  "history",
				"lines": r.History,
			})

			if err != nil {
				log.Printf("Error marshaling history: %v", err)
			} else {
				client.send <- historyMsg
			}

		case client := <-r.unregister:
			if _, ok := r.clients[client]; ok {
				delete(r.clients, client)
				close(client.send)
				log.Printf("Client %s unregistered from room %s", client.id, r.id)
				if len(r.clients) == 0 {
					r.hub.unregister <- r
					log.Printf("Room %s is empty, closing.", r.id)
					return
				}
			}

		case message := <-r.broadcast:
			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err == nil {
				if t, ok := msg["type"].(string); ok {
					switch t {
					case "draw":
						r.History = append(r.History, message)
					case "reset":
						r.History = make([]json.RawMessage, 0)
					}
				}
			}

			for client := range r.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(r.clients, client)
				}
			}
		}
	}
}

type Hub struct {
	rooms      map[string]*Room
	register   chan *Room
	unregister chan *Room
	mu         sync.Mutex
}

func newHub() *Hub {
	return &Hub{
		rooms:      make(map[string]*Room),
		register:   make(chan *Room),
		unregister: make(chan *Room),
	}
}

func (h *Hub) run() {
	for {
		select {
		case room := <-h.register:
			h.mu.Lock()
			h.rooms[room.id] = room
			h.mu.Unlock()
			log.Printf("Room %s registered", room.id)
		case room := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.rooms[room.id]; ok {
				delete(h.rooms, room.id)
			}
			h.mu.Unlock()
			log.Printf("Room %s unregistered", room.id)
		}
	}
}

func (h *Hub) getOrCreateRoom(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()

	if room, ok := h.rooms[id]; ok {
		return room
	}

	room := newRoom(id, h)
	h.rooms[room.id] = room
	go room.run()

	return room
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("roomID")
	if roomID == "" {
		http.Error(w, "Room ID is required", http.StatusBadRequest)
		return
	}

	room := hub.getOrCreateRoom(roomID)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}

	client := &Client{
		id:   uuid.NewString(),
		conn: conn,
		send: make(chan []byte, 256),
		room: room,
	}
	client.room.register <- client

	go client.writePump()
	go client.readPump()
}

func main() {
	hub := newHub()
	go hub.run()

	http.Handle("/", templ.Handler(views.LobbyPage()))

	http.HandleFunc("/room/{roomID}", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("roomID")
		component := views.CanvasPage(roomID)
		templ.Handler(component).ServeHTTP(w, r)
	})

	http.HandleFunc("/ws/{roomID}", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Println("Server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
