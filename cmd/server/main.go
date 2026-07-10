// Command server runs the WhatsApp MCP server: it pairs with WhatsApp via
// QR code (whatsmeow/multi-device), persists chat history to SQLite, and
// exposes send/list/search tools to MCP clients over HTTP/SSE.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/flix/whatsapp-mcp/internal/config"
	"github.com/flix/whatsapp-mcp/internal/mcpserver"
	"github.com/flix/whatsapp-mcp/internal/restapi"
	"github.com/flix/whatsapp-mcp/internal/store"
	"github.com/flix/whatsapp-mcp/internal/webhook"
	"github.com/flix/whatsapp-mcp/internal/whatsapp"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(cfg.WhatsAppDBPath), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.HistoryDBPath), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	history, err := store.Open(ctx, cfg.HistoryDBPath)
	if err != nil {
		log.Fatalf("open history store: %v", err)
	}
	defer history.Close()

	// Track the latest QR code as a PNG for the /qr HTTP endpoint, so pairing
	// doesn't require terminal access.
	qrHandler := newQRHandler()

	webhookDispatcher := webhook.New(cfg.WebhookURL, cfg.WebhookFromNumbers, cfg.WebhookSecret)
	if cfg.WebhookURL != "" {
		if len(cfg.WebhookFromNumbers) > 0 {
			log.Printf("webhook: dispatching incoming messages from %v to %s", cfg.WebhookFromNumbers, cfg.WebhookURL)
		} else {
			log.Printf("webhook: dispatching all incoming messages to %s", cfg.WebhookURL)
		}
	}

	waClient, err := whatsapp.New(ctx, cfg.WhatsAppDBPath, history, cfg.LogLevel, func(code string) {
		log.Printf("Scan this QR code with WhatsApp (Linked Devices) to pair. It also renders at %s/qr", cfg.PublicBaseURL)
		printQRToTerminal(code)
		qrHandler.set(code)
	}, webhookDispatcher.Dispatch)
	if err != nil {
		log.Fatalf("start whatsapp client: %v", err)
	}
	defer waClient.Close()

	mcpSrv := mcpserver.Build(mcpserver.Deps{WA: waClient, History: history})

	// Two transports, same MCP server: SSE is kept for older/other clients,
	// but Streamable HTTP is what current n8n (and most other MCP clients)
	// expect now -- n8n's MCP Client node marks SSE deprecated in favor of it.
	sseSrv := server.NewSSEServer(mcpSrv,
		server.WithBaseURL(cfg.PublicBaseURL),
		server.WithKeepAlive(true),
	)
	streamableSrv := server.NewStreamableHTTPServer(mcpSrv,
		server.WithEndpointPath("/mcp"),
		server.WithHeartbeatInterval(15*time.Second),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/qr", qrHandler.serveHTTP(waClient))
	mux.Handle("/api/", mcpserver.AuthMiddleware(cfg.AuthToken, restapi.Handler(restapi.Deps{WA: waClient, History: history})))
	mux.Handle("/mcp", mcpserver.AuthMiddleware(cfg.AuthToken, streamableSrv))
	mux.Handle("/", mcpserver.AuthMiddleware(cfg.AuthToken, sseSrv))

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("whatsapp-mcp listening on %s (MCP Streamable HTTP: %s/mcp, MCP SSE: %s/sse, REST: %s/api/v1)",
			cfg.HTTPAddr, cfg.PublicBaseURL, cfg.PublicBaseURL, cfg.PublicBaseURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = sseSrv.Shutdown(shutdownCtx)
	_ = streamableSrv.Shutdown(shutdownCtx)
}

func printQRToTerminal(code string) {
	// Compact terminal QR rendering, good enough to scan from most terminals.
	art, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		log.Printf("(could not render QR to terminal: %v) raw code: %s", err, code)
		return
	}
	os.Stdout.WriteString(art.ToSmallString(false))
}

// qrHandler serves the current pairing QR code as a PNG at /qr, so a headless
// deployment can be paired by opening a browser instead of watching logs.
type qrHandler struct {
	code chan string // buffered, always holds the latest code (or is empty)
}

func newQRHandler() *qrHandler {
	return &qrHandler{code: make(chan string, 1)}
}

func (h *qrHandler) set(code string) {
	select {
	case <-h.code: // drain stale value
	default:
	}
	h.code <- code
}

func (h *qrHandler) current() (string, bool) {
	select {
	case c := <-h.code:
		h.code <- c // put it back
		return c, true
	default:
		return "", false
	}
}

func (h *qrHandler) serveHTTP(waClient *whatsapp.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if waClient.IsPaired() {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("Already paired with WhatsApp. Nothing to scan."))
			return
		}
		code, ok := h.current()
		if !ok {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("No QR code yet, still starting up. Refresh in a moment."))
			return
		}
		png, err := qrcode.Encode(code, qrcode.Medium, 512)
		if err != nil {
			http.Error(w, "failed to render QR code", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	}
}
