package balancer

import (
	"log"
	"math/rand"
	"os"
	"strconv"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"
)

const defaultMaxLatency int = 300
const defaultGracePeriod int = 100

var requestCounter int = 0

type Balancer struct {
	ownIP       string
	gracePeriod int
	hostLatency map[string]map[string]*model.LatencyInfo
}

func NewBalancer(ownIP string) *Balancer {
	rand.Seed(time.Now().Unix())

	graceVal, err := strconv.Atoi(os.Getenv("GRACE_PERIOD"))
	if err != nil {
		graceVal = defaultGracePeriod
	}

	graceEnv := int(graceVal)
	return &Balancer{
		ownIP:       ownIP,
		gracePeriod: graceEnv,
		hostLatency: make(map[string]map[string]*model.LatencyInfo, 0),
	}
}

func (b *Balancer) ChoosePod(pods []*model.PodInfo, annotations map[string]string, service string) (string, string) {
	// first requests are randomly distributed
	if requestCounter < b.gracePeriod {
		requestCounter++
		log.Println("Grace period :: Pod IP chosen randomly")

		index := rand.Intn(len(pods))
		return pods[index].IP, pods[index].HostIP
	}

	minVal, err := strconv.Atoi(annotations["maxLatency"])
	if err != nil {
		minVal = defaultMaxLatency
	}

	minLatency := minVal

	var bestPodIPs []model.BestPodInfo

	// then try to find node with least latency
	for _, pod := range pods {
		if b.hostLatency[pod.HostIP] == nil || b.hostLatency[pod.HostIP][service] == nil {
			log.Println("No data for IP", pod.HostIP, "sending request to test it out")
			return pod.IP, pod.HostIP
		}

		value := b.hostLatency[pod.HostIP][service].AverageLatency
		if value < minLatency {
			bestPodIPs = append(bestPodIPs, model.BestPodInfo{IP: pod.IP, HostIP: pod.HostIP})
		}
	}

	// select a random pod from pods that satisfy QoS
	if len(bestPodIPs) > 0 {
		log.Println("Choosing a random Pod IP from the list that satisify QoS")
		index := rand.Intn(len(bestPodIPs))
		return bestPodIPs[index].IP, bestPodIPs[index].HostIP
	}

	// if none are valid select on own pod
	for _, pod := range pods {
		if b.ownIP == pod.HostIP {
			log.Println("None satisfy the QoS, try to route to local")
			return pod.IP, pod.HostIP
		}
	}

	// all else fails, revert to random
	index := rand.Intn(len(pods))
	return pods[index].IP, pods[index].HostIP
}

// potentially add logic that "resets" reqCount after 100 or after certain period
// so more weight is given to the more recent calculations
func (b *Balancer) SetLatency(hostIP string, latency int, service string) {
	latencyHost := b.hostLatency[hostIP]
	if latencyHost == nil {
		b.hostLatency[hostIP] = make(map[string]*model.LatencyInfo)
		b.hostLatency[hostIP][service] = &model.LatencyInfo{
			AverageLatency: latency,
			ReqCount:       1,
		}
		return
	}

	latencyInfo := latencyHost[service]
	if latencyInfo == nil {
		latencyHost[service] = &model.LatencyInfo{
			AverageLatency: latency,
			ReqCount:       1,
		}
		return
	}

	latencyInfo.AverageLatency = (latencyInfo.AverageLatency*latencyInfo.ReqCount + latency) / (latencyInfo.ReqCount + 1)
	latencyInfo.ReqCount++

	log.Println("Adjust latency data for |", hostIP, service, "| => |", latencyInfo.AverageLatency, latencyInfo.ReqCount, "|")
}
