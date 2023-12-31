package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

func init() {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	switch logLevel {
	case "debug":
		logrus.SetLevel(logrus.DebugLevel)
	case "info":
		logrus.SetLevel(logrus.InfoLevel)
	case "warning":
		logrus.SetLevel(logrus.WarnLevel)
	case "error":
		logrus.SetLevel(logrus.ErrorLevel)
	default:
		logrus.SetLevel(logrus.InfoLevel)
	}

	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
}

func replaceDomainInResponse(originalSubdomain, replaceSubdomain, originalDomain string, buffer *bytes.Buffer) {
	body := buffer.String()
	fullReplace := replaceSubdomain + "." + "lunaroasis.net" // We know that statescale and snapscale are under this domain
	fullOriginal := originalSubdomain + "." + originalDomain // Original domain can vary
	replacedBody := strings.ReplaceAll(body, fullReplace, fullOriginal)
	buffer.Reset()
	buffer.WriteString(replacedBody)
}

func proxyRequest(fullSubdomain, path string, buffer *bytes.Buffer, r *http.Request) (int, map[string]string, error) {
	client := &http.Client{}
	target := "https://" + fullSubdomain + ".lunaroasis.net" + path
	newReq, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		logrus.Errorf("Failed to create request: %v", err)
		return 0, nil, err
	}
	newReq.Header = r.Header

	resp, err := client.Do(newReq)
	if err != nil {
		logrus.Errorf("Failed to send request: %v", err)
		return 0, nil, err
	}
	defer resp.Body.Close()

	headers := make(map[string]string)
	for key, values := range resp.Header {
		for _, value := range values {
			headers[key] = value
		}
	}

	encoding := resp.Header.Get("Content-Encoding")
	var reader io.Reader
	switch encoding {
	case "br":
		// Decompress Brotli data
		reader = brotli.NewReader(resp.Body)
	case "gzip":
		// Decompress Gzip data
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			logrus.Errorf("Failed to create gzip reader: %v", err)
			return 0, nil, err
		}
	case "deflate":
		// Decompress Deflate data
		reader = flate.NewReader(resp.Body)
	default:
		reader = resp.Body
	}
	io.Copy(buffer, reader)

	return resp.StatusCode, headers, nil
}

func handleHttpRequest(w http.ResponseWriter, r *http.Request) {
	logrus.Infof("Received request from %s", r.Host)

	hostParts := strings.Split(r.Host, ".")
	if len(hostParts) < 3 {
		logrus.Errorf("Invalid domain: %s", r.Host)
		http.Error(w, "Invalid domain", http.StatusBadRequest)
		return
	}

	subdomain := hostParts[0] // Extract original domain
	originalDomain := strings.Join(hostParts[1:], ".")

	// Check for WebSocket upgrade headers
	if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
		// Handle WebSocket requests by proxying to snapscale
		proxyWebSocketRequest(subdomain, w, r)
		return
	}

	buffer := new(bytes.Buffer)
	backupBuffer := new(bytes.Buffer)

	logrus.Debugf("Proxying request to %s", subdomain+"-statescale")
	statusCode, headers, err := proxyRequest(subdomain+"-statescale", r.RequestURI, buffer, r)
	logrus.Debugf("Received status code %d", statusCode)
	if err != nil || statusCode >= 400 {
		logrus.Debugf("Proxying request to %s", subdomain+"-snapscale")
		backupStatusCode, backupHeaders, _ := proxyRequest(subdomain+"-snapscale", r.RequestURI, backupBuffer, r)
		logrus.Debugf("Received status code %d", backupStatusCode)

		replaceDomainInResponse(subdomain, subdomain+"-snapscale", originalDomain, backupBuffer)

		for key, value := range backupHeaders {
			w.Header().Set(key, value)
		}
		w.WriteHeader(backupStatusCode)
		encoding := headers["Content-Encoding"]
		buffer = compressData(buffer, encoding)
		io.Copy(w, backupBuffer)
		return
	}

	replaceDomainInResponse(subdomain, subdomain+"-statescale", originalDomain, buffer)
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.WriteHeader(statusCode)
	// If the original response was Brotli-compressed, recompress the data
	encoding := headers["Content-Encoding"]
	buffer = compressData(buffer, encoding)
	io.Copy(w, buffer)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all connections
	},
}

func proxyWebSocketRequest(subdomain string, w http.ResponseWriter, r *http.Request) {
	// Build target URL
	fullSubdomain := subdomain + "-snapscale"
	target := "wss://" + fullSubdomain + ".lunaroasis.net" + r.RequestURI

	// Create a new WebSocket connection to the target
	dialer := websocket.Dialer{}
	targetConn, resp, err := dialer.Dial(target, nil)
	if err != nil {
		logrus.Errorf("Failed to connect to target: %v", err)
		if resp != nil {
			logrus.Errorf("Handshake response status: %s", resp.Status)
			// Log all response headers for debugging
			for k, v := range resp.Header {
				logrus.Errorf("%s: %s", k, v)
			}
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer targetConn.Close()

	// Upgrade the client connection to a WebSocket connection
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logrus.Errorf("Failed to upgrade client connection: %v", err)
		return // No need to send an error response, Upgrade already did if there was an error
	}
	defer clientConn.Close()

	// Start goroutines to copy data between the client and target
	go func() {
		for {
			messageType, message, err := targetConn.ReadMessage()
			if err != nil {
				logrus.Errorf("Failed to read from target: %v", err)
				return
			}
			err = clientConn.WriteMessage(messageType, message)
			if err != nil {
				logrus.Errorf("Failed to write to client: %v", err)
				return
			}
		}
	}()
	go func() {
		for {
			messageType, message, err := clientConn.ReadMessage()
			if err != nil {
				logrus.Errorf("Failed to read from client: %v", err)
				return
			}
			err = targetConn.WriteMessage(messageType, message)
			if err != nil {
				logrus.Errorf("Failed to write to target: %v", err)
				return
			}
		}
	}()

	// The goroutines will run until one of the connections is closed
	select {}
}

func compressData(buffer *bytes.Buffer, encoding string) *bytes.Buffer {
	var compressedData bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "br":
		writer = brotli.NewWriterLevel(&compressedData, brotli.DefaultCompression)
	case "gzip":
		writer = gzip.NewWriter(&compressedData)
	case "deflate":
		writer, _ = flate.NewWriter(&compressedData, flate.DefaultCompression)
	default:
		return buffer
	}
	io.Copy(writer, buffer)
	writer.Close()
	return &compressedData
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	handleHttpRequest(w, r)
}

func main() {
	logrus.Info("Starting server on :8080")
	http.HandleFunc("/", handleRequest)
	http.ListenAndServe(":8080", nil)
}
