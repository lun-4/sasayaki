package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/api/chat"
	"github.com/gin-gonic/gin"
	gonanoid "github.com/matoous/go-nanoid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/samber/lo"
)

type Config struct {
	ServerURL  string
	Port       string
	DBPath     string
	AppviewURL string
}

type DIDDocument struct {
	Context []string  `json:"@context"`
	ID      string    `json:"id"`
	Service []Service `json:"service"`
}

type Service struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ServiceEndpoint string `json:"serviceEndpoint"`
}

func newDIDDocument(serverURL string) DIDDocument {
	return DIDDocument{
		Context: []string{"https://www.w3.org/ns/did/v1"},
		ID:      fmt.Sprintf("did:web:%s", serverURL),
		Service: []Service{
			{
				ID:              "#bsky_chat",
				Type:            "BskyChatService",
				ServiceEndpoint: fmt.Sprintf("https://%s", serverURL),
			},
		},
	}
}

func requestDebugMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get the raw request body
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
			// Restore the body for later middleware/handlers
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		// Print request details
		fmt.Printf("\n==== Incoming Request ====\n")
		fmt.Printf("Method: %s\n", c.Request.Method)
		fmt.Printf("URL: %s\n", c.Request.URL.String())

		// Print headers
		fmt.Println("\nHeaders:")
		for name, values := range c.Request.Header {
			fmt.Printf("%s: %s\n", name, strings.Join(values, ", "))
		}

		// Print query parameters
		fmt.Println("\nQuery Parameters:")
		for key, values := range c.Request.URL.Query() {
			fmt.Printf("%s: %s\n", key, strings.Join(values, ", "))
		}

		// Print body if exists
		if len(bodyBytes) > 0 {
			fmt.Println("\nBody:")
			// Try to pretty print JSON
			var prettyJSON bytes.Buffer
			if err := json.Indent(&prettyJSON, bodyBytes, "", "  "); err == nil {
				fmt.Println(prettyJSON.String())
			} else {
				// If not JSON, print raw body
				fmt.Println(string(bodyBytes))
			}
		}

		fmt.Println("\n========================")

		c.Next()
	}
}

type Storage struct {
	db         *sql.DB
	appviewUrl string
}

func (st Storage) hydrateEverything() error {
	rows, err := st.db.Query("SELECT user_did FROM convo_members")
	if err != nil {
		return fmt.Errorf("error querying convo_members: %w", err)
	}
	for rows.Next() {
		var did string
		err = rows.Scan(&did)
		if err != nil {
			return fmt.Errorf("error fetching did: %w", err)
		}
		var handle string
		err = st.db.QueryRow("SELECT handle FROM users WHERE did = ?", did).Scan(&handle)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// fetch from appview
				req, err := http.NewRequest("GET", fmt.Sprintf("%s/xrpc/app.bsky.actor.getProfile?actor=%s", st.appviewUrl, did), nil)
				if err != nil {
					return fmt.Errorf("error creating request: %w", err)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					return fmt.Errorf("error executing request: %w", err)
				}
				if res.StatusCode != http.StatusOK {
					return fmt.Errorf("appview returned not 200, got %d", res.StatusCode)
				}
				body, err := io.ReadAll(res.Body)
				if err != nil {
					return fmt.Errorf("error reading response body: %w", err)
				}
				var profile map[string]any
				err = json.Unmarshal(body, &profile)
				if err != nil {
					return fmt.Errorf("error parsing response body: %w", err)
				}
				handle = profile["handle"].(string)
				_, err = st.db.Exec("INSERT INTO users (did, handle) VALUES (?, ?)", did, handle)
				if err != nil {
					return fmt.Errorf("error inserting user: %w", err)
				}
			} else {
				return fmt.Errorf("error fetching handle: %w", err)
			}
		}
	}
	return nil
}

type ConvoMessage struct {
	ConvoID     string
	AuthorDID   string
	ID          string
	SentAt      time.Time
	Deleted     bool
	MessageData ConvoMessageData
}

type ConvoMessageData struct {
	Text   string                `json:"text"`
	Facets []*bsky.RichtextFacet `json:"facets"`
}

func (cm ConvoMessage) asDeleted() *chat.ConvoDefs_DeletedMessageView {
	if !cm.Deleted {
		panic("ConvoMessage is not deleted")
	}
	return &chat.ConvoDefs_DeletedMessageView{
		Id:     cm.ID,
		Rev:    "aaa",
		Sender: &chat.ConvoDefs_MessageViewSender{Did: cm.AuthorDID},
		SentAt: cm.SentAt.Format(time.RFC3339),
	}
}
func (cm ConvoMessage) asMessage() *chat.ConvoDefs_MessageView {
	if cm.Deleted {
		panic("can't call asMessage on a deleted message")
	}
	return &chat.ConvoDefs_MessageView{
		Id:     cm.ID,
		Rev:    "aaa",
		Sender: &chat.ConvoDefs_MessageViewSender{Did: cm.AuthorDID},
		SentAt: cm.SentAt.Format(time.RFC3339),
		Embed:  nil,
		Text:   cm.MessageData.Text,
		Facets: cm.MessageData.Facets,
	}
}

func (st Storage) getMessage(convoID string, messageID string) (*ConvoMessage, error) {
	row := st.db.QueryRow("SELECT author_did, data, sent_at, deleted FROM convo_messages WHERE convo_id = ? AND message_id = ?", convoID, messageID)
	var m ConvoMessage
	m.ConvoID = convoID
	m.ID = messageID
	var mJSON string
	var sentAt int64
	err := row.Scan(&m.AuthorDID, &mJSON, &sentAt, &m.Deleted)
	if err != nil {
		return nil, fmt.Errorf("error querying message: %w", err)
	}
	m.SentAt = time.Unix(sentAt, 0)
	if !m.Deleted {
		err = json.Unmarshal([]byte(mJSON), &m.MessageData)
		if err != nil {
			return nil, fmt.Errorf("error parsing message data: %w", err)
		}
	}
	return &m, nil
}

func randomId() string {
	return gonanoid.MustGenerate("abcdefghimnopqrstuvwxyz123456", 20)
}

func (st Storage) createConvo(members []string) (*chat.ConvoDefs_ConvoView, error) {
	id := randomId()
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	_, err = tx.Exec(`INSERT INTO convos (id, rev, last_message_id) VALUES (?, ?, ?)`, id, "whatever", nil)
	if err != nil {
		return nil, fmt.Errorf("error inserting convo: %w", err)
	}

	for _, memberDid := range members {
		_, err = tx.Exec(`INSERT INTO convo_members (convo_id, user_did) VALUES (?, ?)`, id, memberDid)
		if err != nil {
			return nil, fmt.Errorf("error inserting convo member: %w", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("error committing tx: %w", err)
	}

	err = st.hydrateEverything()
	if err != nil {
		return nil, fmt.Errorf("error rehydrating inner users: %w", err)
	}
	return st.getConvo(members[0], id)

}
func (st Storage) getConvo(userDID, convoID string) (*chat.ConvoDefs_ConvoView, error) {
	convos, err := st.getAllConvos(userDID)
	if err != nil {
		return nil, err
	}
	for _, c := range convos {
		if c.Id == convoID {
			return c, nil
		}
	}
	return nil, nil
}

func (st Storage) getAllConvos(userDID string) ([]*chat.ConvoDefs_ConvoView, error) {
	rows, err := st.db.Query("SELECT convo_id FROM convo_members WHERE user_did = ?", userDID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := make([]*chat.ConvoDefs_ConvoView, 0)
	for rows.Next() {
		view := chat.ConvoDefs_ConvoView{}
		err = rows.Scan(&view.Id)
		if err != nil {
			return nil, fmt.Errorf("error scanning convos: %w", err)
		}
		views = append(views, &view)
	}

	for _, view := range views {
		var lastMessageId *string
		row := st.db.QueryRow("SELECT rev, last_message_id FROM convos WHERE id = ?", view.Id)
		if err != nil {
			return nil, fmt.Errorf("failed to query single convo row: %w", err)
		}
		err = row.Scan(&view.Rev, &lastMessageId)
		if err != nil {
			return nil, fmt.Errorf("failed to scan convo data: %w", err)
		}
		rows, err := st.db.Query("SELECT user_did, muted, unread_count FROM convo_members WHERE convo_id = ?", view.Id)
		if err != nil {
			return nil, fmt.Errorf("Failed to query members: %w", err)
		}
		defer rows.Close()
		members := make([]string, 0)
		for rows.Next() {
			var did string
			var muted bool
			var unreadCount int64
			err := rows.Scan(&did, &muted, &unreadCount)
			if err != nil {
				return nil, fmt.Errorf("Failed to scan user did for member list: %v", err)
			}
			members = append(members, did)
			var handle string
			err = st.db.QueryRow("SELECT handle FROM users WHERE did = ?", did).Scan(&handle)
			if err != nil {
				return nil, fmt.Errorf("Failed to scan user handle for member list: %v", err)
			}
			view.Members = append(view.Members, &chat.ActorDefs_ProfileViewBasic{
				Did:    did,
				Handle: handle,
			})

			if did == userDID {
				// muted and unread is per-did
				view.Muted = muted
				view.UnreadCount = unreadCount
			}
			if lastMessageId != nil {
				m, err := st.getMessage(view.Id, *lastMessageId)
				if err != nil {
					return nil, fmt.Errorf("Failed to get message for member: %v", err)
				}

				view.LastMessage = &chat.ConvoDefs_ConvoView_LastMessage{}
				if m.Deleted {
					view.LastMessage.ConvoDefs_DeletedMessageView = m.asDeleted()
				} else {
					view.LastMessage.ConvoDefs_MessageView = m.asMessage()
				}
			}
		}
	}
	v, err := json.Marshal(views)
	fmt.Println(string(v))
	return views, nil
}

func (st Storage) getMessages(convoId string, limit int64, cursorString string) (*chat.ConvoGetMessages_Output, error) {
	var cursor int64
	if cursorString != "" {
		cursorP, err := strconv.ParseInt(cursorString, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cursor: %v", err)
		}
		cursor = cursorP
	} else {
		cursor = math.MaxInt64
	}
	rows, err := st.db.Query(`SELECT message_id
		FROM convo_messages
		WHERE convo_id = ?
			AND cursor < ?
		ORDER BY cursor DESC
		LIMIT ?`, convoId, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %v", err)
	}
	defer rows.Close()
	msgs := make([]*ConvoMessage, 0)
	var smallestId string
	for rows.Next() {
		var msgId string
		err = rows.Scan(&msgId)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch message id: %v", err)
		}
		msg, err := st.getMessage(convoId, msgId)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch message id %s: %v", msgId, err)
		}
		if msg.ID < smallestId {
			smallestId = msg.ID
		}
		msgs = append(msgs, msg)
	}
	msgView := make([]*chat.ConvoGetMessages_Output_Messages_Elem, 0)
	for _, m := range msgs {
		if m.Deleted {
			msgView = append(msgView, &chat.ConvoGetMessages_Output_Messages_Elem{
				ConvoDefs_DeletedMessageView: m.asDeleted(),
			})
		} else {
			msgView = append(msgView, &chat.ConvoGetMessages_Output_Messages_Elem{
				ConvoDefs_MessageView: m.asMessage(),
			})
		}
	}

	return &chat.ConvoGetMessages_Output{
		Cursor:   &smallestId,
		Messages: msgView,
	}, nil
}

func (st Storage) createMessage(convoID, userDID string, message *chat.ConvoDefs_MessageInput) (*ConvoMessage, error) {
	id := randomId()
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	msgData, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %v", err)
	}

	var maxCursor *int64
	err = tx.QueryRow(`SELECT MAX(cursor) FROM convo_messages WHERE convo_id = ?`, convoID).Scan(&maxCursor)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %v", err)
	}

	if maxCursor == nil {
		maxCursor = lo.ToPtr(int64(0))
	}

	_, err = tx.Exec(`INSERT INTO convo_messages
		(convo_id, message_id, author_did, sent_at, cursor, data, deleted) 
		VALUES (?, ?, ?, ?, ?, ?, ?)`, convoID, id, userDID, time.Now().Unix(), (*maxCursor)+1, string(msgData), false)
	if err != nil {
		return nil, fmt.Errorf("error inserting message: %w", err)
	}

	_, err = tx.Exec(`UPDATE convos SET last_message_id = ? WHERE id = ?`, id, convoID)
	if err != nil {
		return nil, fmt.Errorf("error updating convo: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("error committing tx: %w", err)
	}

	return st.getMessage(convoID, id)
}

type State struct {
	storage *Storage
}

func (s State) listConvos(c *gin.Context) {
	userDID := c.GetString("user_did")
	convos, err := s.storage.getAllConvos(userDID)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	out := chat.ConvoListConvos_Output{Convos: convos}
	c.JSON(200, out)
}

func (s State) getConvo(c *gin.Context) {
	userDID := c.GetString("user_did")
	convoID := c.Query("convoId")
	convo, err := s.storage.getConvo(userDID, convoID)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	out := chat.ConvoGetConvo_Output{}
	out.Convo = convo
	c.JSON(200, out)
}

func (s State) getConvoForMembers(c *gin.Context) {
	userDID := c.GetString("user_did")
	memberDIDs := c.QueryArray("members")
	memberDIDs = append(memberDIDs, userDID)
	out := chat.ConvoGetConvoForMembers_Output{}
	convos, err := s.storage.getAllConvos(userDID)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	for _, convo := range convos {
		containsAllMembersExactly := true
		for _, member := range convo.Members {
			if len(convo.Members) != len(memberDIDs) {
				containsAllMembersExactly = false
			}
			for _, memberDID := range memberDIDs {
				if member.Did != memberDID {
					containsAllMembersExactly = false
				}
			}
		}
		if containsAllMembersExactly {
			out.Convo = convo
			break
		}
	}
	if out.Convo == nil {
		// create convo automatically
		newConvo, err := s.storage.createConvo(memberDIDs)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
		out.Convo = newConvo
	}
	c.JSON(200, out)
}
func (s State) getLog(c *gin.Context) {
	userDID := c.GetString("user_did")
	convos, err := s.storage.getAllConvos(userDID)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	out := chat.ConvoGetLog_Output{
		Logs: make([]*chat.ConvoGetLog_Output_Logs_Elem, 0),
	}
	for _, convo := range convos {
		out.Logs = append(out.Logs, &chat.ConvoGetLog_Output_Logs_Elem{
			ConvoDefs_LogBeginConvo: &chat.ConvoDefs_LogBeginConvo{
				ConvoId: convo.Id,
				Rev:     convo.Rev,
			},
		})
	}
	c.JSON(200, out)
}

func (s State) updateRead(c *gin.Context) {
	userDID := c.GetString("user_did")
	var input chat.ConvoUpdateRead_Input
	v, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	err = json.Unmarshal(v, &input)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}
	convo, err := s.storage.getConvo(userDID, input.ConvoId)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	c.JSON(200, chat.ConvoUpdateRead_Output{Convo: convo})
}

func (s State) getMessages(c *gin.Context) {
	userDID := c.GetString("user_did")
	convoID := c.Query("convoId")
	limitStr := c.Query("limit")
	cursor := c.Query("cursor")
	if limitStr == "" {
		limitStr = "10"
	}
	limit, err := strconv.ParseInt(limitStr, 10, 64)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	convo, err := s.storage.getConvo(userDID, convoID)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	if convo == nil {
		c.AbortWithError(http.StatusForbidden, fmt.Errorf("Convo not found"))
		return
	}
	out, err := s.storage.getMessages(convoID, limit, cursor)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	c.JSON(200, out)
}

func (s State) sendMessage(c *gin.Context) {
	userDID := c.GetString("user_did")

	var input chat.ConvoSendMessage_Input
	v, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	err = json.Unmarshal(v, &input)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}
	m, err := s.storage.createMessage(input.ConvoId, userDID, input.Message)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	c.JSON(200, m.asMessage())
}

func main() {
	// Initialize configuration
	config := Config{
		ServerURL:  getEnvOrDefault("SERVER_URL", "localhost:3000"),
		Port:       getEnvOrDefault("PORT", "3000"),
		DBPath:     getEnvOrDefault("DB_PATH", "data.db"),
		AppviewURL: getEnvOrDefault("APPVIEW_URL", ""),
	}

	db, err := sql.Open("sqlite3", config.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
	PRAGMA journal_mode=WAL;
	PRAGMA busy_timeout = 5000;
	PRAGMA synchronous = NORMAL;
	PRAGMA cache_size = 1000000000;
	PRAGMA foreign_keys = true;
	PRAGMA temp_store = memory;

	CREATE TABLE IF NOT EXISTS users (
		did text primary key,
		handle text
	) STRICT;

	CREATE TABLE IF NOT EXISTS convos (
		id text primary key,
		rev text,
		last_message_id text
	) STRICT;

	CREATE TABLE IF NOT EXISTS convo_members (
		convo_id text,
		user_did text,
		unread_count int default 0,
		muted int default 0,
		primary key (convo_id, user_did)
	) STRICT;

	CREATE TABLE IF NOT EXISTS convo_logs (
		convo_id text,
		user_did text,
		cursor int,
		data text -- json encoded
	) STRICT;
	CREATE TABLE IF NOT EXISTS convo_messages (
		convo_id text,
		message_id text,
		author_did text,
		sent_at int,
		cursor int,
		deleted int,
		data text, -- json encoded
		primary key (convo_id, message_id)
	) STRICT;
	CREATE INDEX IF NOT EXISTS convo_messages_convo_id on convo_messages (convo_id);
	CREATE INDEX IF NOT EXISTS convo_messages_convo_id_cursor on convo_messages (convo_id, cursor);
	`)
	if err != nil {
		log.Fatalf("Error creating tables: %v", err)
	}

	storage := Storage{db: db, appviewUrl: config.AppviewURL}
	state := State{storage: &storage}

	err = storage.hydrateEverything()
	if err != nil {
		panic(fmt.Errorf("Error hydrating everything: %v", err))
	}

	// Create Gin router
	r := gin.New()

	// Middleware
	r.Use(gin.Recovery())
	r.Use(gin.Logger())
	r.Use(requestDebugMiddleware())

	serviceWebDID := "did:web:" + config.ServerURL
	auther, err := NewAuth(
		100_000,
		time.Hour*12,
		5,
		serviceWebDID,
	)
	if err != nil {
		log.Fatalf("Failed to create Auth: %v", err)
	}
	authGroup := r.Group("/")
	authGroup.Use(auther.AuthenticateGinRequestViaJWT)
	authGroup.GET("/xrpc/chat.bsky.convo.listConvos", state.listConvos)
	authGroup.GET("/xrpc/chat.bsky.convo.getConvoForMembers", state.getConvoForMembers)
	authGroup.GET("/xrpc/chat.bsky.convo.getConvo", state.getConvo)
	authGroup.GET("/xrpc/chat.bsky.convo.getLog", state.getLog)
	authGroup.GET("/xrpc/chat.bsky.convo.getMessages", state.getMessages)
	authGroup.POST("/xrpc/chat.bsky.convo.updateRead", state.updateRead)
	authGroup.POST("/xrpc/chat.bsky.convo.sendMessage", state.sendMessage)

	// Routes
	r.GET("/", func(c *gin.Context) {
		c.String(200, "Welcome to the debug server!")
	})

	// DID Document endpoint
	r.GET("/.well-known/did.json", func(c *gin.Context) {
		didDoc := newDIDDocument(config.ServerURL)
		c.JSON(200, didDoc)
	})

	// Start server
	addr := ":" + config.Port
	fmt.Printf("Server starting on %s\n", addr)
	r.Run(addr)
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
