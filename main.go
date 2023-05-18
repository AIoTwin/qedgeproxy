package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
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
	selectedIP, hostIP := edgeBalancer.ChoosePod(namespace, service)
	log.Println("Selected pod IP ::", selectedIP)

	originServerURL, err := url.Parse("http://188.184.21.108/") //url.Parse("http://" + selectedIP + "/")
	if err != nil {
		log.Fatal("Invalid origin server URL")
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

	var start time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			edgeBalancer.SetLatency(hostIP, int(time.Since(start).Milliseconds()), service)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	start = time.Now()

	return http.DefaultClient.Do(req)
}

func reverseProxyHandler(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[reverse proxy server] received request at: %s\n", time.Now())

	service := strings.Split(req.Host, ".")[0]
	originServerURL, hostIP := getOriginServer(service)

	if originServerURL == nil {
		rw.WriteHeader(404)
		_, _ = fmt.Fprint(rw, "No server for Host\n")
		return
	}
	// get the response from the origin server
	originServerResponse, err := forwardRequest(req, originServerURL, service, hostIP)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(rw, err)
		return
	}

	// return response to the client
	rw.WriteHeader(http.StatusOK)
	io.Copy(rw, originServerResponse.Body)
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

	edgeBalancer = balancer.NewBalancer(k3sClient, ownIP)

	reverseProxy := http.HandlerFunc(reverseProxyHandler)

	log.Println("Starting proxy at port " + *port)
	log.Fatal(http.ListenAndServe(":"+(*port), reverseProxy))
}
