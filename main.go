package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

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

func handleRequest(w http.ResponseWriter, r *http.Request) {
	hostParts := strings.Split(r.Host, ".")
	if len(hostParts) < 3 {
		http.Error(w, "Invalid domain", http.StatusBadRequest)
		return
	}

	subdomain := hostParts[0] // Extract original domain
	originalDomain := strings.Join(hostParts[1:], ".")

	buffer := new(bytes.Buffer)
	backupBuffer := new(bytes.Buffer)

	statusCode, headers, err := proxyRequest(subdomain+".statescale", r.RequestURI, buffer, r)
	if err != nil || statusCode >= 400 {
		backupStatusCode, backupHeaders, _ := proxyRequest(subdomain+".snapscale", r.RequestURI, backupBuffer, r)
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

func main() {
	http.HandleFunc("/", handleRequest)
	http.ListenAndServe(":8080", nil)
}
