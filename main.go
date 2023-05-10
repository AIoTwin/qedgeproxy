package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"time"

	client "gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/k3s-client"
)

var k3sClient *client.K3sClient
var ownIP string

func getOriginServer() *url.URL {

	pods, err := k3sClient.GetPodsForService("default", "whoami-prx")
	if err != nil {
		log.Println("Failed to retrieve pods for service :: ", err.Error())
		return nil
	}

	log.Println("Own IP is ::", ownIP)

	var selectedIP string
	for _, pod := range pods {
		log.Println("Checking if", pod.HostIP, "is equal to Own IP")
		if ownIP == pod.HostIP {
			selectedIP = pod.IP
		}
	}

	if selectedIP == "" {
		log.Println("No pod found on self node, routing random")
		index := rand.Intn(len(pods))
		selectedIP = pods[index].IP
	}

	log.Println("Selected pod IP ::", selectedIP)

	originServerURL, err := url.Parse("http://" + selectedIP + "/")
	if err != nil {
		log.Fatal("invalid origin server URL")
	}

	return originServerURL
}

func reverseProxyHandler(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[reverse proxy server] received request at: %s\n", time.Now())

	originServerURL := getOriginServer()

	// set req Host, URL and Request URI to forward a request to the origin server
	req.Host = originServerURL.Host
	req.URL.Host = originServerURL.Host
	req.URL.Scheme = originServerURL.Scheme
	req.RequestURI = ""

	// save the response from the origin server
	originServerResponse, err := http.DefaultClient.Do(req)

	log.Println("Complete URL was ::", req.URL.Host+req.URL.Path)
	log.Println("Selected Pod responded with body ::", originServerResponse.Body)

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

	var err error
	k3sClient, err = client.NewSK3sClient("/etc/secret-volume/config")
	if err != nil {
		log.Fatal("Error while initializing k3s client ::", err.Error())
		return
	}

	ownIP = os.Getenv("NODE_IP")

	rand.Seed(time.Now().Unix())

	reverseProxy := http.HandlerFunc(reverseProxyHandler)

	log.Println("Starting proxy at port " + *port)
	log.Fatal(http.ListenAndServe(":"+(*port), reverseProxy))
}
