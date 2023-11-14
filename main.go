package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/balancer"
	client "gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/k3s-client"
)

var edgeBalancer *balancer.Balancer

var ownIP string
var namespace string

func getOriginServer(service string) (*url.URL, string) {
	selectedIP, hostIP, targetPort := edgeBalancer.ChoosePod(namespace, service)
	if selectedIP == "" {
		return nil, ""
	}

	log.Println("Selected pod IP ::", selectedIP+":"+targetPort)

	originServerURL, err := url.Parse("http://" + selectedIP + ":" + targetPort + "/")
	if err != nil {
		log.Println("Invalid origin server URL")
		return nil, ""
	}

	return originServerURL, hostIP
}

func forwardRequest(req *http.Request, originServerURL *url.URL, service string, hostIP string) (*http.Response, error) {
	// set req Host, URL and Request URI to forward a request to the origin server
	req.Host = originServerURL.Host
	req.URL.Host = originServerURL.Host
	req.URL.Scheme = originServerURL.Scheme
	req.RequestURI = ""

	return http.DefaultClient.Do(req)
}

func reverseProxyHandler(rw http.ResponseWriter, req *http.Request) {
	log.Printf("\n\n[reverse proxy server] received request at: %s\n", time.Now())

	service := strings.Split(req.Host, ".")[0]
	originServerURL, hostIP := getOriginServer(service)

	if originServerURL == nil {
		rw.WriteHeader(404)
		_, _ = fmt.Fprint(rw, "No server for Host\n")
		return
	}
	// get the response from the origin server
	//start := time.Now()
	originServerResponse, err := forwardRequest(req, originServerURL, service, hostIP)
	if err != nil {
		edgeBalancer.SetReqFailed(hostIP, service)
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(rw, err)
		return
	}
	//edgeBalancer.SetLatency(hostIP, int(time.Since(start).Milliseconds()), service)

	// return response to the client
	rw.WriteHeader(http.StatusOK)
	io.Copy(rw, originServerResponse.Body)
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintf(w, "Method not allowed")
		return
	}

	// Get the query parameters from the request
	queryParams := r.URL.Query()

	// Set the Content-Type header to match the request
	contentType := r.Header.Get("Content-Type")
	w.Header().Set("Content-Type", contentType)

	// Create a response body by encoding the query parameters as JSON
	response := make(map[string]interface{})
	for key, values := range queryParams {
		if len(values) > 0 {
			response[key] = values[0]
		}
	}

	// Encode the response as JSON
	responseJSON, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Error encoding response: %v", err)
		return
	}

	// Write the response body back as the response
	w.WriteHeader(http.StatusOK)
	w.Write(responseJSON)
}

func main() {
	port := flag.String("p", "9090", "Port of reverse proxy")
	flag.Parse()

	ownIP = os.Getenv("NODE_IP")
	namespace = os.Getenv("NAMESPACE")

	if ownIP == "" || namespace == "" {
		log.Fatal("ERROR :: Own IP or namespace not detected!")
		return
	}

	k3sClient, err := client.NewSK3sClient("/etc/secret-volume/config")
	if err != nil {
		log.Fatal("Error while initializing k3s client ::", err.Error())
		return
	}

	edgeBalancer = balancer.NewBalancer(k3sClient, ownIP, "30090")

	reverseProxy := http.HandlerFunc(reverseProxyHandler)

	mux := http.NewServeMux()
	mux.Handle("/", reverseProxy)
	mux.HandleFunc("/echo", echoHandler)

	log.Println("Starting proxy at port " + *port)
	log.Fatal(http.ListenAndServe(":"+(*port), mux))
}
