package netplay

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
	"github.com/chrsmith/temporal-doom/internal/game"
)

// The protocol is JSON over a WebSocket. Every message has a type, "t".
//
// Client to server:
//
//	{"t":"join","name":"alice"}    claim a slot (yours, if you had one) and play
//	{"t":"cmds","cmds":[...]}      your next ticcmds
//	{"t":"pause"}                  step away; your marine stands still
//	{"t":"leave"}                  give up your slot
//	{"t":"resync"}                 send me a sync message again
//	{"t":"ping","ts":123.4}        answered with a pong
//
// Server to client:
//
//	sync    the workflow's sync query, plus "pending": the tics played since
//	tics    merged tics, numbered from "from", in order and without gaps
//	seat    your slot (-1 if none), whether the game is waiting for your
//	        input, and the tic your next ticcmd will run at
//	check   the workflow's checksum at a tic, once it has recorded it. If
//	        the segment isn't the one you synced from, resync instead: a game
//	        resumed from a savegame doesn't play out quite like one that
//	        never stopped (DOOM's savegames leave some state out)
//	roster  the players, and a bitmask of those holding up the game
//	error   what went wrong, with "code":"full" if the game had no slot
//	        for you
//	pong, over

type clientMsg struct {
	T    string          `json:"t"`
	Name string          `json:"name,omitempty"`
	Cmds []doomvm.TicCmd `json:"cmds,omitempty"`
	TS   float64         `json:"ts,omitempty"`
}

type syncMsg struct {
	T string `json:"t"`
	game.Sync
	Pending []doomvm.NetTic `json:"pending"`
}

type ticsMsg struct {
	T    string          `json:"t"`
	From int32           `json:"from"`
	Tics []doomvm.NetTic `json:"tics"`
}

type seatMsg struct {
	T       string `json:"t"`
	Slot    int    `json:"slot"`
	Playing bool   `json:"playing"`
	Tic     int32  `json:"tic"`
}

type checkMsg struct {
	T        string `json:"t"`
	Segment  int    `json:"segment"`
	Tic      int32  `json:"tic"`
	Checksum uint32 `json:"checksum"`
}

type rosterMsg struct {
	T       string            `json:"t"`
	Players []game.PlayerInfo `json:"players"`
	Waiting uint8             `json:"waiting"`
}

type pongMsg struct {
	T  string  `json:"t"`
	TS float64 `json:"ts"`
}

type errorMsg struct {
	T     string `json:"t"`
	Error string `json:"error"`
	// Code is "full" if a join found no free slot.
	Code string `json:"code,omitempty"`
}

type overMsg struct {
	T      string `json:"t"`
	Reason string `json:"reason"`
}

const (
	// pingEvery keeps idle connections open through load balancers (the
	// AWS ALB's default idle timeout is 60s), and notices dead ones.
	pingEvery   = 20 * time.Second
	readTimeout = 3 * pingEvery
	// outBuffer is how many messages can wait for a slow client. At 35
	// tics a second that's several seconds; a client further behind than
	// that is dropped, and resyncs when it reconnects.
	outBuffer = 256
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:    4096,
	WriteBufferSize:   16384,
	EnableCompression: true, // sync messages are mostly base64 and JSON
}

// client is one browser's connection to a room. Its fields belong to the
// room's goroutine, except conn, which belongs to readLoop and writeLoop.
type client struct {
	conn   *websocket.Conn
	player string
	name   string
	out    chan []byte
	closed bool

	synced  bool // following the game: gets tics
	syncs   int  // counts sync requests, so stale replies can be ignored
	slot    int  // as last told to the client
	playing bool
}

func newClient(conn *websocket.Conn, player string) *client {
	return &client{conn: conn, player: player, out: make(chan []byte, outBuffer), slot: -1}
}

func (c *client) send(m any) {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	c.sendRaw(b)
}

func (c *client) sendRaw(b []byte) {
	if c.closed {
		return
	}
	select {
	case c.out <- b:
	default:
		c.close() // too slow; it will reconnect and resync
	}
}

// close ends the connection once everything already sent has gone out.
func (c *client) close() {
	if !c.closed {
		c.closed = true
		close(c.out)
	}
}

func (c *client) writeLoop() {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	defer c.conn.Close()
	for {
		select {
		case b, ok := <-c.out:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}

// readLoop passes the client's messages to the room until the connection
// closes.
func (c *client) readLoop(r *room) {
	c.conn.SetReadLimit(64 << 10)
	c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	for {
		_, b, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		var m clientMsg
		if err := json.Unmarshal(b, &m); err != nil {
			return
		}
		if !r.do(func() { r.onMessage(c, m) }) {
			return
		}
	}
}
