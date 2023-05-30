package balancer

import (
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	client "gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/k3s-client"
	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"
)

const defaultMaxLatency int = 300
const defaultPercentageQoS float64 = 0.3
const defaultNewLatencyWeight float64 = 0.2
const defaultCooldownBaseDuration int = 30
const defaultRealDataPeriod int = 360
const defaultPingTimeout int = 1
const defaultPingCacheTime int = 100
const defaultQosRecalculationCooldownS = 60
const defaultNewLatencyApprWeight float64 = 0.7

const pingURLSuffix string = "/echo?param1=value1&param2=value2"

type Balancer struct {
	ownIP     string
	k3sClient *client.K3sClient

	qosPercentage             float64
	qosRecalculationTime      map[string]time.Time
	qosRecalculationCooldownS int

	latencyWeight         float64
	latencyApprWeight     float64
	realDataPeriodS       int
	cooldownBaseDurationS int

	pingPort      string
	pingTimeout   int
	pingCacheTime int
	hostPingCache map[string]*model.PingCache

	randomMode bool

	serviceInit   map[string]bool
	hostLatency   map[string]map[string]*model.HostData
	maxLatencies  map[string]int
	channels      map[string]chan map[string]*model.HostData
	approxRunning map[string]*atomic.Bool
}

func NewBalancer(k3sClient *client.K3sClient, ownIP string, pingPort string) *Balancer {
	rand.Seed(time.Now().Unix())

	qosPercentage, err := strconv.ParseFloat(os.Getenv("QOS_PERC"), 64)
	if err != nil {
		qosPercentage = defaultPercentageQoS
	}
	log.Println("QOS_PERC:", qosPercentage)

	newLatencyWeight, err := strconv.ParseFloat(os.Getenv("LAT_WEIGHT"), 64)
	if err != nil {
		newLatencyWeight = defaultNewLatencyWeight
	}
	log.Println("LAT_WEIGHT:", newLatencyWeight)

	newLatencyApprWeight, err := strconv.ParseFloat(os.Getenv("LAT_APPR_WEIGHT"), 64)
	if err != nil {
		newLatencyApprWeight = defaultNewLatencyApprWeight
	}
	log.Println("LAT_APPR_WEIGHT:", newLatencyApprWeight)

	cooldownBaseDuration, err := strconv.Atoi(os.Getenv("COOLDOWN_BASE_DURATION_S"))
	if err != nil {
		cooldownBaseDuration = defaultCooldownBaseDuration
	}
	log.Println("COOLDOWN_BASE_DURATION_S:", cooldownBaseDuration)

	realDataPeriod, err := strconv.Atoi(os.Getenv("REAL_DATA_VALID_S"))
	if err != nil {
		realDataPeriod = defaultRealDataPeriod
	}
	log.Println("REAL_DATA_VALID_S:", realDataPeriod)

	pingTimeout, err := strconv.Atoi(os.Getenv("PING_TIMEOUT_S"))
	if err != nil {
		pingTimeout = defaultPingTimeout
	}
	log.Println("PING_TIMEOUT_S:", pingTimeout)

	pingCacheTime, err := strconv.Atoi(os.Getenv("PING_CACHE_TIME_S"))
	if err != nil {
		pingCacheTime = defaultPingCacheTime
	}
	log.Println("PING_CACHE_TIME_S:", pingCacheTime)

	qosRecalculationCooldownS, err := strconv.Atoi(os.Getenv("QOS_COOLDOWN_S"))
	if err != nil {
		qosRecalculationCooldownS = defaultQosRecalculationCooldownS
	}
	log.Println("QOS_COOLDOWN_S:", qosRecalculationCooldownS)

	randomMode, err := strconv.ParseBool(os.Getenv("RANDOM_MODE"))
	if err != nil {
		randomMode = true
	}
	log.Println("RANDOM_MODE:", randomMode)

	channels := make(map[string]chan map[string]*model.HostData)

	return &Balancer{
		ownIP:                     ownIP,
		k3sClient:                 k3sClient,
		qosPercentage:             qosPercentage,
		latencyWeight:             newLatencyWeight,
		latencyApprWeight:         newLatencyApprWeight,
		cooldownBaseDurationS:     cooldownBaseDuration,
		qosRecalculationCooldownS: qosRecalculationCooldownS,
		qosRecalculationTime:      make(map[string]time.Time),
		realDataPeriodS:           realDataPeriod,
		pingPort:                  pingPort,
		pingTimeout:               pingTimeout,
		pingCacheTime:             pingCacheTime,
		randomMode:                randomMode,
		hostPingCache:             make(map[string]*model.PingCache),
		hostLatency:               make(map[string]map[string]*model.HostData),
		serviceInit:               make(map[string]bool),
		maxLatencies:              make(map[string]int),
		channels:                  channels,
		approxRunning:             make(map[string]*atomic.Bool),
	}
}

func (b *Balancer) ChoosePod(namespace string, service string) (string, string, string) {
	podsAll, annotations, targetPort, err := b.k3sClient.GetPodsForService(namespace, service)
	if err != nil {
		log.Println("Failed to retrieve pods for service :: ", err.Error())
		return "", "", ""
	}
	pods := b.filterHealthyPods(podsAll, service)
	if len(pods) == 0 {
		log.Println("No pods found for service, returning nil ::", service)
		return "", "", ""
	}

	log.Print("Filtered healthy pods :: ")
	for _, pd := range pods {
		log.Print(pd.HostIP, " ", pd.IP)
	}

	maxVal, err := strconv.Atoi(annotations["maxLatency"])
	if err != nil {
		maxVal = defaultMaxLatency
	}
	maxLatency := maxVal

	// start apporixmating the request latency on first request for a service
	if !b.serviceInit[service] {
		b.serviceInit[service] = true
		b.channels[service] = make(chan map[string]*model.HostData)
		b.maxLatencies[service] = maxLatency

		b.approxRunning[service] = &atomic.Bool{}
		b.approxRunning[service].Store(true)

		b.qosRecalculationTime[service] = time.Now()

		go b.ApproximateLatency(podsAll, service, maxLatency)
	} else {
		select {
		case x, ok := <-b.channels[service]:
			if ok {
				b.approxRunning[service].Store(false)
				b.adjustLatencies(service, x)
				log.Println("Adjusted latencies for service ::", service)
			} else {
				log.Println("Channel closed for service", service)
			}
		default:
			log.Println("No value ready for service", service, ", moving on.")
		}
	}

	var bestPodIPs []model.PodInfo
	var overloadedPodsIPs []model.PodInfo
	skipNodeStatus := false

	nodeStatus, err := b.k3sClient.GetNodesStatus()
	if err != nil {
		log.Println("Failed retrieving node status ::", nodeStatus)
		skipNodeStatus = true
	}

	for _, pod := range pods {
		if b.hostLatency[pod.HostIP] == nil || b.hostLatency[pod.HostIP][service] == nil {
			continue
		}

		serviceStatus := b.hostLatency[pod.HostIP][service]
		if serviceStatus.Latency < maxLatency {
			if !skipNodeStatus && (nodeStatus[pod.HostIP].CpuUsage > 0.9 || nodeStatus[pod.HostIP].RamUsage > 0.9) {
				overloadedPodsIPs = append(overloadedPodsIPs, model.PodInfo{IP: pod.IP, HostIP: pod.HostIP})
			} else {
				bestPodIPs = append(bestPodIPs, model.PodInfo{IP: pod.IP, HostIP: pod.HostIP})
			}
		}
	}

	// not enough QoS pods, recalculate!
	if !b.checkQoSMin(len(pods), len(bestPodIPs)+len(overloadedPodsIPs)) && int(time.Since(b.qosRecalculationTime[service]).Seconds()) > b.qosRecalculationCooldownS && b.approxRunning[service].CompareAndSwap(false, true) {
		log.Println("QoS Min check failed! Running approximation again")
		b.qosRecalculationTime[service] = time.Now()
		go b.ApproximateLatency(podsAll, service, maxLatency)
	}

	// if there are no good pod IPs with good latency, send to overloaded ones
	if len(bestPodIPs) == 0 {
		bestPodIPs = overloadedPodsIPs
	}

	// select a random pod from pods that satisfy QoS
	if len(bestPodIPs) > 0 {
		index := rand.Intn(len(bestPodIPs))
		if b.randomMode {
			log.Println("Choosing a random Pod IP from the list that satisify QoS")
			return bestPodIPs[index].IP, bestPodIPs[index].HostIP, targetPort
		} else {
			sort.SliceStable(bestPodIPs, func(i, j int) bool {
				return b.hostLatency[bestPodIPs[i].HostIP][service].Latency < b.hostLatency[bestPodIPs[j].HostIP][service].Latency
			})

			log.Println("Selected a pod based on Node resource usage and latency")

			return bestPodIPs[0].IP, bestPodIPs[0].HostIP, targetPort
		}
	}

	// if none are valid select on own pod
	for _, pod := range pods {
		if b.ownIP == pod.HostIP {
			log.Println("None satisfy the QoS, try to route to local")
			return pod.IP, pod.HostIP, targetPort
		}
	}

	log.Println("Other routing roules failed, routing random")
	// all else fails, revert to random
	index := rand.Intn(len(pods))
	return pods[index].IP, pods[index].HostIP, targetPort
}

func (b *Balancer) SetLatency(hostIP string, latency int, service string) {
	latencyHost := b.hostLatency[hostIP]
	if latencyHost == nil {
		b.hostLatency[hostIP] = make(map[string]*model.HostData)
		b.hostLatency[hostIP][service] = &model.HostData{
			Latency:          latency,
			IsServiceHealthy: true,
			IsApproximated:   false,
			FailedReqCounter: 0,
			ReqTime:          time.Now(),
		}
		log.Println("Adjust latency data for |", hostIP, service, latency, "| => |", b.hostLatency[hostIP][service], "|")
		return
	}

	if latencyHost[service] == nil {
		latencyHost[service] = &model.HostData{
			Latency:          latency,
			IsServiceHealthy: true,
			IsApproximated:   false,
			FailedReqCounter: 0,
			ReqTime:          time.Now(),
		}
		log.Println("Adjust latency data for |", hostIP, service, latency, "| => |", b.hostLatency[hostIP][service], "|")
		return
	}

	if latencyHost[service].IsApproximated {
		log.Println("Last latency for service", service, "was approximated. Using weight ::", b.latencyApprWeight)
		latencyHost[service].Latency = int((1-b.latencyApprWeight)*float64(latencyHost[service].Latency) + b.latencyApprWeight*float64(latency))
	} else {
		latencyHost[service].Latency = int((1-b.latencyWeight)*float64(latencyHost[service].Latency) + b.latencyWeight*float64(latency))
	}

	latencyHost[service].FailedReqCounter = 0
	latencyHost[service].IsApproximated = false
	latencyHost[service].IsServiceHealthy = true
	latencyHost[service].ReqTime = time.Now()

	log.Println("Adjust latency data for |", hostIP, service, latency, "| => |", latencyHost[service], "|")
}

func (b *Balancer) SetReqFailed(hostIP string, service string) {
	if b.hostLatency[hostIP] == nil {
		b.hostLatency[hostIP] = make(map[string]*model.HostData)
	}

	if b.hostLatency[hostIP][service] == nil {
		b.hostLatency[hostIP][service] = &model.HostData{
			IsServiceHealthy: false,
			ReqTime:          time.Now(),
			FailedReqCounter: 1,
		}
	} else {
		b.hostLatency[hostIP][service].IsServiceHealthy = false
		b.hostLatency[hostIP][service].ReqTime = time.Now()
		b.hostLatency[hostIP][service].FailedReqCounter++
	}

	log.Println("Request failed, sending on cooldown ::", b.hostLatency[hostIP][service])
}

func (b *Balancer) ApproximateLatency(pods []*model.PodInfo, service string, maxLatency int) {
	hostLatency := make(map[string]*model.HostData)
	log.Println("GO: Approximating latency for service", service)

	for _, pod := range pods {
		var latency int

		if hostLatency[pod.HostIP] != nil {
			continue
		}

		if val, ok := b.hostPingCache[pod.HostIP]; ok && int(time.Since(val.CacheTime).Seconds()) < b.pingCacheTime {
			latency = val.Latency
			log.Println("GO: Using cached latency for host", pod.HostIP)
		} else {
			latency = pingHost("http://"+pod.HostIP+":"+b.pingPort+pingURLSuffix, b.pingTimeout)

			if latency == -1 {
				latency = maxLatency
			} else {
				b.hostPingCache[pod.HostIP] = &model.PingCache{
					CacheTime: time.Now(),
					Latency:   latency,
				}
			}
		}

		hostLatency[pod.HostIP] = &model.HostData{
			Latency:          latency,
			IsApproximated:   true,
			IsServiceHealthy: true,
			FailedReqCounter: 0,
			ReqTime:          time.Now(),
		}

	}

	// new latencies calculated, give it to the main thread
	b.channels[service] <- hostLatency
}

func (b *Balancer) adjustLatencies(service string, x map[string]*model.HostData) {
	for k, v := range x {
		if b.hostLatency[k] == nil {
			b.hostLatency[k] = make(map[string]*model.HostData)
		}

		if b.hostLatency[k][service] == nil {
			b.hostLatency[k][service] = v
		} else {
			if int(time.Since(b.hostLatency[k][service].ReqTime).Seconds()) > b.realDataPeriodS || b.hostLatency[k][service].IsApproximated {
				b.hostLatency[k][service].Latency = v.Latency
				b.hostLatency[k][service].IsApproximated = v.IsApproximated
			}
		}
	}
}

func (b *Balancer) checkQoSMin(podNum int, goodPodsNum int) bool {
	validQosMin := float64(goodPodsNum)/float64(podNum) >= b.qosPercentage
	log.Println("Check if QoS Min is satisifed ::", validQosMin)

	return validQosMin
}

func (b *Balancer) isServiceInTimeout(serviceStatus *model.HostData) bool {
	isInTimeout := time.Since(serviceStatus.ReqTime).Seconds() < float64(b.cooldownBaseDurationS)*float64(serviceStatus.FailedReqCounter)

	return !serviceStatus.IsServiceHealthy && isInTimeout
}

func (b *Balancer) filterHealthyPods(pods []*model.PodInfo, service string) []*model.PodInfo {
	result := make([]*model.PodInfo, 0)
	for _, pod := range pods {
		serviceStatus := b.hostLatency[pod.HostIP][service]
		if serviceStatus == nil || !b.isServiceInTimeout(serviceStatus) {
			result = append(result, pod)
		}
	}

	return result
}

func pingHost(hostUrl string, timeoutS int) int {
	start := time.Now()
	client := &http.Client{
		Timeout: time.Duration(timeoutS) * time.Second, // Set the timeout duration
	}

	request, err := http.NewRequest("GET", hostUrl, nil)
	if err != nil {
		log.Println("GO: Error creating GET request:", err.Error())
		return -1
	}

	response, err := client.Do(request)
	if err != nil {
		log.Println("GO: Error sending GET request:", err.Error())
		return -1
	}
	defer response.Body.Close()

	// Read the response body
	_, err = ioutil.ReadAll(response.Body)
	if err != nil {
		log.Println("GO: Error reading response body:", err.Error())
		return -1
	}

	log.Println("GO: Host", hostUrl, "pinged! Result", time.Since(start))

	return int(time.Since(start).Milliseconds())
}
