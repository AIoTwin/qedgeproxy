package balancer

import (
	"log"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/go-ping/ping"
	client "gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/k3s-client"
	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultMaxLatency int = 300
const defaultPercentageQoS float64 = 0.3

type Balancer struct {
	ownIP         string
	k3sClient     *client.K3sClient
	qosPercentage float64
	serviceInit   map[string]bool
	hostLatency   map[string]map[string]*model.LatencyInfo
	channels      map[string]chan map[string]*model.LatencyInfo
	approxRunning map[string]bool
}

func NewBalancer(k3sClient *client.K3sClient, ownIP string) *Balancer {
	rand.Seed(time.Now().Unix())

	qosPercentage, err := strconv.ParseFloat(os.Getenv("QOS_PERC"), 64)
	if err != nil {
		qosPercentage = defaultPercentageQoS
	}

	channels := make(map[string]chan map[string]*model.LatencyInfo)

	return &Balancer{
		ownIP:         ownIP,
		k3sClient:     k3sClient,
		qosPercentage: qosPercentage,
		hostLatency:   make(map[string]map[string]*model.LatencyInfo),
		serviceInit:   make(map[string]bool),
		channels:      channels,
		approxRunning: make(map[string]bool),
	}
}

func (b *Balancer) ChoosePod(namespace string, service string) (string, string) {
	pods, podSelector, annotations, err := b.k3sClient.GetPodsForService(namespace, service)
	if err != nil {
		log.Println("Failed to retrieve pods for service :: ", err.Error())
		return "", ""
	}

	// start apporixmating the request latency on first request for a service
	if !b.serviceInit[service] {
		b.serviceInit[service] = true
		b.channels[service] = make(chan map[string]*model.LatencyInfo)

		podsHostMap := podsHostMap(pods)
		b.approxRunning[service] = true
		go b.ApproximateLatency(namespace, podSelector, service, podsHostMap)
	} else {
		select {
		case x, ok := <-b.channels[service]:
			if ok {
				b.approxRunning[service] = false
				b.adjustLatencies(service, x)
			} else {
				log.Println("Channel closed for service", service)
			}
		default:
			log.Println("No value ready for service", service, ", moving on.")
		}
	}

	maxVal, err := strconv.Atoi(annotations["maxLatency"])
	if err != nil {
		maxVal = defaultMaxLatency
	}
	maxLatency := maxVal
	var bestPodIPs []model.PodInfo

	for _, pod := range pods {
		if b.hostLatency[pod.HostIP] == nil || b.hostLatency[pod.HostIP][service] == nil {
			continue
		}

		value := b.hostLatency[pod.HostIP][service].AverageLatency
		if value < maxLatency {
			bestPodIPs = append(bestPodIPs, model.PodInfo{IP: pod.IP, HostIP: pod.HostIP})
		}
	}

	// select a random pod from pods that satisfy QoS
	if len(bestPodIPs) > 0 {
		log.Println("Choosing a random Pod IP from the list that satisify QoS")
		index := rand.Intn(len(bestPodIPs))
		return bestPodIPs[index].IP, bestPodIPs[index].HostIP
	}

	// not enough QoS pods, recalculate!
	if !b.approxRunning[service] && b.checkQoSMin(len(pods), len(bestPodIPs)) {
		podsHostMap := podsHostMap(pods)
		b.approxRunning[service] = true
		go b.ApproximateLatency(namespace, podSelector, service, podsHostMap)
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

func (b *Balancer) ApproximateLatency(namespace string, podSelector *metav1.LabelSelector, service string, podsHostMap map[string]string) {
	podsStatus, err := b.k3sClient.GetPodsStatus(namespace, podSelector, podsHostMap)
	if err != nil {
		log.Println("Failed to retrieve status of pods")
	}

	hostAverages := b.getHostAverages()
	hostLatency := make(map[string]*model.LatencyInfo)

	for k, weight := range podsStatus {
		var latency float64

		if val, ok := hostAverages[k]; ok {
			latency = float64(val)
		} else {
			pinger, err := ping.NewPinger("188.184.21.108")
			if err != nil {
				latency = float64(defaultMaxLatency)
			} else {
				pinger.Count = 1
				pinger.Run()

				stats := pinger.Statistics()

				latency = float64(stats.AvgRtt.Milliseconds())
			}
		}

		if hostLatency[k] == nil {
			hostLatency[k] = &model.LatencyInfo{
				AverageLatency: int(weight * latency),
				ReqCount:       1,
			}
		} else {
			hostLatency[k].AverageLatency = int(weight * latency)
			hostLatency[k].ReqCount = 1
		}
	}

	// new latencies calculated, give it to the main thread
	b.channels[service] <- hostLatency
}

func (b *Balancer) getHostAverages() map[string]int {
	result := make(map[string]int)
	for kHost, vHost := range b.hostLatency {
		for _, vSer := range vHost {
			if result[kHost] > vSer.AverageLatency {
				result[kHost] = vSer.AverageLatency
			}
		}
	}

	return result
}

func (b *Balancer) adjustLatencies(service string, x map[string]*model.LatencyInfo) {
	for k, v := range x {
		if b.hostLatency[k] == nil {
			b.hostLatency[k] = make(map[string]*model.LatencyInfo)
		}
		if b.hostLatency[k][service] == nil {
			b.hostLatency[k][service] = &model.LatencyInfo{
				AverageLatency: v.AverageLatency,
				ReqCount:       v.ReqCount,
			}
		} else {
			b.hostLatency[k][service].AverageLatency = v.AverageLatency
			b.hostLatency[k][service].ReqCount = v.ReqCount
		}
	}
}

func (b *Balancer) checkQoSMin(podNum int, goodPodsNum int) bool {
	return float64(goodPodsNum)/float64(podNum) > b.qosPercentage
}

func podsHostMap(pods []*model.PodInfo) map[string]string {
	result := make(map[string]string)
	for _, pod := range pods {
		result[pod.Name] = pod.HostIP
	}
	return result
}
