package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
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

	var subdomain, originalDomain, fullSubdomain string
	subdomain = hostParts[0] // Extract first part of domain

	if hostParts[1] != "lunaroasis" {
		// If there's an additional subdomain (e.g. kepler)
		originalDomain = strings.Join(hostParts[1:], ".")
		fullSubdomain = subdomain + "." + hostParts[1]
	} else {
		originalDomain = strings.Join(hostParts[1:], ".")
		fullSubdomain = subdomain
	}

	buffer := new(bytes.Buffer)
	backupBuffer := new(bytes.Buffer)

	debugLog.Printf("Proxying request to %s", fullSubdomain+"-statescale")
	statusCode, headers, err := proxyRequest(fullSubdomain+"-statescale", r.RequestURI, buffer, r)
	debugLog.Printf("Received status code %d", statusCode)
	if err != nil || statusCode >= 400 {
		debugLog.Printf("Proxying request to %s", fullSubdomain+"-snapscale")
		backupStatusCode, backupHeaders, _ := proxyRequest(fullSubdomain+"-snapscale", r.RequestURI, backupBuffer, r)
		debugLog.Printf("Received status code %d", backupStatusCode)

		replaceDomainInResponse(fullSubdomain, fullSubdomain+"-snapscale", originalDomain, backupBuffer)

		for key, value := range backupHeaders {
			w.Header().Set(key, value)
		}
		w.WriteHeader(backupStatusCode)
		io.Copy(w, backupBuffer)
		return
	}

	replaceDomainInResponse(fullSubdomain, fullSubdomain+"-statescale", originalDomain, buffer)
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.WriteHeader(statusCode)
	io.Copy(w, buffer)
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	handleHttpRequest(w, r)
}

func main() {
	infoLog.Println("Starting server on :8080")
	http.HandleFunc("/", handleRequest)
	http.ListenAndServe(":8080", nil)
}
