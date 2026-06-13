package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"sendbigfiles/internal/app"
)

func main() {
	maxFileBytes := int64(512 * 1024 * 1024)
	if raw := os.Getenv("MAX_FILE_BYTES"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			log.Fatalf("invalid MAX_FILE_BYTES: %q", raw)
		}
		maxFileBytes = value
	}

	server, err := app.New(app.Config{
		Addr:          envOr("ADDR", ":8080"),
		DataDir:       envOr("DATA_DIR", "./data"),
		WebDir:        envOr("WEB_DIR", "./web/dist"),
		MaxFileBytes:  maxFileBytes,
		CleanupPeriod: time.Minute,
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("sendbigfiles listening on %s", server.Addr())
	log.Fatal(server.ListenAndServe())
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
