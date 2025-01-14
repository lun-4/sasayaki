package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type Config struct {
	ServerURL string `json:"server_url"`
	Port      string `json:"port"`
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

func main() {
	// Initialize configuration
	config := Config{
		ServerURL: getEnvOrDefault("SERVER_URL", "localhost:3000"),
		Port:      getEnvOrDefault("PORT", "3000"),
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
	authGroup.GET("/xrpc/chat.bsky.convo.listConvos", listConvos)

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

func listConvos(c *gin.Context) {
	c.JSON(200, map[string]any{
		"convos": []any{},
	})
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
