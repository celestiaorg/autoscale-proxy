package main

import (
	"bytes"
	"context"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"
)

var (
	debugLog *log.Logger
	infoLog  *log.Logger
	errorLog *log.Logger
)

func init() {
	debugLog = log.New(os.Stdout, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)
	infoLog = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	errorLog = log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
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
		return 0, nil, err
	}
	newReq.Header = r.Header

	resp, err := client.Do(newReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	headers := make(map[string]string)
	for key, values := range resp.Header {
		for _, value := range values {
			headers[key] = value
		}
	}

	io.Copy(buffer, resp.Body)
	return resp.StatusCode, headers, nil
}

func handleHttpRequest(w http.ResponseWriter, r *http.Request) {
	infoLog.Printf("Received request from %s", r.Host)
	hostParts := strings.Split(r.Host, ".")
	if len(hostParts) < 3 {
		errorLog.Printf("Invalid domain: %s", r.Host)
		http.Error(w, "Invalid domain", http.StatusBadRequest)
		return
	}

	subdomain := hostParts[0] // Extract original domain
	originalDomain := strings.Join(hostParts[1:], ".")

	buffer := new(bytes.Buffer)
	backupBuffer := new(bytes.Buffer)

	debugLog.Printf("Proxying request to %s", subdomain+".statescale")
	statusCode, headers, err := proxyRequest(subdomain+".statescale", r.RequestURI, buffer, r)
	debugLog.Printf("Received status code %d", statusCode)
	if err != nil || statusCode >= 400 {
		debugLog.Printf("Proxying request to %s", subdomain+".snapscale")
		backupStatusCode, backupHeaders, _ := proxyRequest(subdomain+".snapscale", r.RequestURI, backupBuffer, r)
		debugLog.Printf("Received status code %d", backupStatusCode)

		replaceDomainInResponse(subdomain, subdomain+".snapscale", originalDomain, backupBuffer)

		for key, value := range backupHeaders {
			w.Header().Set(key, value)
		}
		w.WriteHeader(backupStatusCode)
		io.Copy(w, backupBuffer)
		return
	}

	replaceDomainInResponse(subdomain, subdomain+".statescale", originalDomain, buffer)
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.WriteHeader(statusCode)
	io.Copy(w, buffer)
}

func proxyGrpcRequest(ctx context.Context, fullSubdomain, method string, r *http.Request) (codes.Code, map[string]string, []byte, error) {
	target := "https://" + fullSubdomain + ".lunaroasis.net" + method
	conn, err := grpc.Dial(target, grpc.WithInsecure())
	if err != nil {
		return 0, nil, nil, err
	}
	defer conn.Close()

	// Convert http.Header to map[string]string
	headerMap := make(map[string]string)
	for key, values := range r.Header {
		headerMap[key] = strings.Join(values, ",")
	}

	// Create metadata from the header map
	md := metadata.New(headerMap)

	// Create a new outgoing context with the metadata
	ctx = metadata.NewOutgoingContext(ctx, md)

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method)
	if err != nil {
		return 0, nil, nil, err
	}

	// Assume r.Body is a io.Reader containing the serialized request message
	if err := stream.SendMsg(r.Body); err != nil {
		return 0, nil, nil, err
	}

	// Create a buffer to hold the serialized response message
	buffer := new(bytes.Buffer)
	if err := stream.RecvMsg(buffer); err != nil {
		return 0, nil, nil, err
	}

	// Collect headers from the gRPC response
	headers, _ := metadata.FromIncomingContext(ctx)
	responseHeaderMap := make(map[string]string)
	for key, values := range headers {
		responseHeaderMap[key] = strings.Join(values, ",")
	}

	if err := stream.RecvMsg(buffer); err != nil {
		st, ok := status.FromError(err)
		if ok {
			return st.Code(), nil, buffer.Bytes(), err
		}
		return codes.Unknown, nil, nil, err
	}
	return codes.Unknown, nil, nil, fmt.Errorf("unexpected error")
}

func handleGrpcRequest(w http.ResponseWriter, r *http.Request) {
	infoLog.Printf("Received gRPC request from %s", r.Host)
	hostParts := strings.Split(r.Host, ".")
	if len(hostParts) < 3 {
		errorLog.Printf("Invalid domain: %s", r.Host)
		http.Error(w, "Invalid domain", http.StatusBadRequest)
		return
	}

	// This line signifies the start of the HTTP response with a status code of 200 OK.
	// The actual gRPC status will be conveyed in the trailers.
	w.WriteHeader(http.StatusOK)

	subdomain := hostParts[0] // Extract original domain
	//originalDomain := strings.Join(hostParts[1:], ".")

	statusCode, headers, responseBody, err := proxyGrpcRequest(r.Context(), subdomain+".statescale", r.RequestURI, r)
	debugLog.Printf("Received status code %d", statusCode)
	if err != nil || statusCode != codes.OK {
		debugLog.Printf("Proxying request to %s", subdomain+".snapscale")
		backupStatusCode, backupHeaders, backupResponseBody, _ := proxyGrpcRequest(r.Context(), subdomain+".snapscale", r.RequestURI, r)
		debugLog.Printf("Received status code %d", backupStatusCode)

		for key, value := range backupHeaders {
			w.Header().Set(key, value)
		}
		w.Write(backupResponseBody) // Write the response body directly
		return
	}

	for key, value := range headers {
		w.Header().Set(key, value)
	}

	// Assume response body is in buffer
	w.Write(responseBody) // Write the response body directly
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") == "application/grpc" {
		handleGrpcRequest(w, r)
	} else {
		handleHttpRequest(w, r)
	}
}

func main() {
	infoLog.Println("Starting server on :8080")
	http.HandleFunc("/", handleRequest)
	http.ListenAndServe(":8080", nil)
}
