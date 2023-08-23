package client

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"

	"k8s.io/client-go/tools/cache"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	v1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

const defaultcacheHoldTimeS int = 360
const defaultNodesMetricsCacheTimeS = 60

type K3sClient struct {
	config           *rest.Config
	clientset        *kubernetes.Clientset
	metricsClientset *metricsv.Clientset
	podCache         *sync.Map

	nodesStatus    map[string]*model.NodeMetrics
	nodesCacheTime int

	serviceMaintainerMap map[string]*model.MaintainerData

	cacheHoldTimeS int

	cacheMutex    *sync.RWMutex
	cronScheduler *cron.Cron
}

func NewSK3sClient(configFilePath string) (*K3sClient, error) {
	// connect to Kubernetes cluster
	config, err := clientcmd.BuildConfigFromFlags("", configFilePath)
	if err != nil {
		log.Println(err.Error())
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Println(err.Error())
		return nil, err
	}

	metricsClientset, err := metricsv.NewForConfig(config)
	if err != nil {
		log.Println(err.Error())
		return nil, err
	}

	cacheHoldTimeS, err := strconv.Atoi(os.Getenv("CACHE_HOLD_TIME_S"))
	if err != nil {
		cacheHoldTimeS = defaultcacheHoldTimeS
	}
	log.Println("CACHE_HOLD_TIME_S:", cacheHoldTimeS)

	nodesMetricsCacheTimeS, err := strconv.Atoi(os.Getenv("NODE_METRICS_CACHE_TIME_S"))
	if err != nil {
		nodesMetricsCacheTimeS = defaultNodesMetricsCacheTimeS
	}
	log.Println("NODE_METRICS_CACHE_TIME_S:", nodesMetricsCacheTimeS)

	client := &K3sClient{
		config:               config,
		clientset:            clientset,
		metricsClientset:     metricsClientset,
		podCache:             &sync.Map{},
		serviceMaintainerMap: make(map[string]*model.MaintainerData),
		nodesCacheTime:       nodesMetricsCacheTimeS,
		cacheHoldTimeS:       cacheHoldTimeS,
		cacheMutex:           &sync.RWMutex{},
	}
	client.startNodeStatusInfoRefresher()
	client.startPodInfoMaintainer()

	return client, nil
}

func (c *K3sClient) GetPodsForService(namespace string, serviceName string) ([]*model.PodInfo, map[string]string, string, error) {
	if cached, found := c.podCache.Load(serviceName); found {
		cachedData := cached.(*model.PodInfoCache)

		c.serviceMaintainerMap[serviceName].LastRequestTime = time.Now()

		log.Println("Returning cached data for service", serviceName)
		return cachedData.Pods, cachedData.Annotations, cachedData.TargetPort, nil
	}

	service, err := c.clientset.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get service %s: %v\n", serviceName, err)
		return nil, nil, "", err
	}

	defer c.startListener(namespace, serviceName, labels.Set(service.Spec.Selector).AsSelector())
	return c.initService(namespace, serviceName, service)
}

func (c *K3sClient) GetNodesStatus() (map[string]*model.NodeMetrics, error) {
	if c.nodesStatus != nil {
		c.cacheMutex.RLock()
		defer c.cacheMutex.RUnlock()

		returnValue := c.createNodeStatusMapCopy()

		return returnValue, nil
	}

	return nil, fmt.Errorf("Nodes status map is not initialized")
}

func (c *K3sClient) startNodeStatusInfoRefresher() {
	c.refreshNodesStatusInfo()

	c.cronScheduler = cron.New(cron.WithSeconds())
	_, _ = c.cronScheduler.AddFunc(fmt.Sprintf("@every %ds", c.nodesCacheTime), c.refreshNodesStatusInfo)

	c.cronScheduler.Start()
}

func (c *K3sClient) refreshNodesStatusInfo() {
	nodes, err := c.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Println("Failed to retrieve nodes on node status")
		return
	}

	nodeMetricsList, err := c.metricsClientset.MetricsV1beta1().NodeMetricses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Println("Failed to retrieve node metrices on node status")
		return
	}

	nodeMetricsMap := make(map[string]v1beta1.NodeMetrics)
	for _, nodeMetric := range nodeMetricsList.Items {
		nodeMetricsMap[nodeMetric.Name] = nodeMetric
	}

	hostMap := make(map[string]*model.NodeMetrics)
	for _, node := range nodes.Items {
		nodeMetric, exists := nodeMetricsMap[node.Name]
		if !exists {
			continue
		}

		cpuUsage := nodeMetric.Usage[corev1.ResourceCPU]
		cpuPercentage := float64(cpuUsage.MilliValue()) / float64(node.Status.Capacity.Cpu().MilliValue())

		memoryUsage := nodeMetric.Usage[corev1.ResourceMemory]
		memoryPercentage := float64(memoryUsage.Value()) / float64(node.Status.Capacity.Memory().Value())

		hostIP := getHostIp(node)
		if hostIP == "" {
			continue
		}

		hostMap[hostIP] = &model.NodeMetrics{
			CpuUsage: cpuPercentage,
			RamUsage: memoryPercentage,
		}
	}

	log.Println("Refreshed host nodes status ::")
	for key, val := range hostMap {
		log.Printf("%s: CPU=%v - RAM=%v\n", key, val.CpuUsage, val.RamUsage)
	}

	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()
	c.nodesStatus = hostMap
}

func (c *K3sClient) startPodInfoMaintainer() {
	c.cronScheduler = cron.New(cron.WithSeconds())
	_, _ = c.cronScheduler.AddFunc("@every 60s", c.maintainServiceInfo)

	c.cronScheduler.Start()
}

func (c *K3sClient) maintainServiceInfo() {
	var clearedServices []string
	for serviceName, maintenanceData := range c.serviceMaintainerMap {
		if time.Since(maintenanceData.LastRequestTime).Seconds() > float64(c.cacheHoldTimeS) {
			close(maintenanceData.Channel)
			c.podCache.Delete(serviceName)

			clearedServices = append(clearedServices, serviceName)
		}
	}

	for _, serviceName := range clearedServices {
		delete(c.serviceMaintainerMap, serviceName)
	}
}

func (c *K3sClient) initService(namespace string, serviceName string, service *corev1.Service) ([]*model.PodInfo, map[string]string, string, error) {
	podList := make([]*model.PodInfo, 0)

	targetPort := strconv.Itoa(int(service.Spec.Ports[0].TargetPort.IntVal))
	annotations := service.Annotations

	podSelector := &metav1.LabelSelector{MatchLabels: service.Spec.Selector}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(podSelector)})
	if err != nil {
		log.Printf("Failed to list pods for service %s: %v\n", serviceName, err)
		return nil, nil, "", err
	}

	for _, pod := range pods.Items {
		podList = append(podList, &model.PodInfo{Name: pod.Name, Namespace: pod.Namespace, IP: pod.Status.PodIP, HostIP: pod.Status.HostIP})
	}

	cacheData := &model.PodInfoCache{
		Pods:        podList,
		Annotations: annotations,
		TargetPort:  targetPort,
	}

	c.podCache.Store(serviceName, cacheData)
	log.Println("Manually updated pods cache for service:", serviceName)

	c.serviceMaintainerMap[serviceName] = &model.MaintainerData{
		LastRequestTime: time.Now(),
	}

	return podList, annotations, targetPort, nil
}

func (c *K3sClient) startListener(namespace string, serviceName string, labelSelector labels.Selector) {
	watchList := cache.NewFilteredListWatchFromClient(
		c.clientset.CoreV1().RESTClient(),
		"pods",
		namespace,
		func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		},
	)

	_, controller := cache.NewInformer(
		watchList,
		&corev1.Pod{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				c.onPodAdd(obj, serviceName)
			},
			DeleteFunc: func(obj interface{}) {
				c.onPodDelete(obj, serviceName)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				c.onPodChange(newObj, serviceName)
			},
		},
	)

	stopCh := make(chan struct{})
	c.serviceMaintainerMap[serviceName].Channel = stopCh

	go controller.Run(stopCh)
}

func (c *K3sClient) onPodChange(obj interface{}, serviceName string) {
	pod := obj.(*corev1.Pod)

	podInfo := &model.PodInfo{Name: pod.Name, Namespace: pod.Namespace, IP: pod.Status.PodIP, HostIP: pod.Status.HostIP}
	podCache, _ := c.podCache.Load(serviceName)

	podIndex := indexOfPods(podCache.(*model.PodInfoCache).Pods, func(p *model.PodInfo) bool {
		return p.Name == podInfo.Name
	})

	if podIndex >= 0 {
		podCache.(*model.PodInfoCache).Pods[podIndex] = podInfo

		c.podCache.Store(serviceName, podCache)
	}
}

func (c *K3sClient) onPodAdd(obj interface{}, serviceName string) {
	pod := obj.(*corev1.Pod)
	podCache, _ := c.podCache.Load(serviceName)

	podIndex := indexOfPods(podCache.(*model.PodInfoCache).Pods, func(p *model.PodInfo) bool {
		return p.Name == pod.Name
	})

	if podIndex != -1 {
		return
	}

	adjustedPods := append(podCache.(*model.PodInfoCache).Pods, &model.PodInfo{Name: pod.Name, Namespace: pod.Namespace, IP: pod.Status.PodIP, HostIP: pod.Status.HostIP})
	podCache.(*model.PodInfoCache).Pods = adjustedPods
	c.podCache.Store(serviceName, podCache)
}

func (c *K3sClient) onPodDelete(obj interface{}, serviceName string) {
	pod := obj.(*corev1.Pod)
	podCache, _ := c.podCache.Load(serviceName)

	filteredPods := filterPods(podCache.(*model.PodInfoCache).Pods, func(p *model.PodInfo) bool {
		return p.Name != pod.Name
	})
	podCache.(*model.PodInfoCache).Pods = filteredPods
	c.podCache.Store(serviceName, podCache)
}

func (c *K3sClient) createNodeStatusMapCopy() map[string]*model.NodeMetrics {
	copiedMap := make(map[string]*model.NodeMetrics, len(c.nodesStatus))
	for key, val := range c.nodesStatus {
		copiedMap[key] = val
	}
	return copiedMap
}

func filterPods(pods []*model.PodInfo, condition func(*model.PodInfo) bool) []*model.PodInfo {
	var filtered []*model.PodInfo
	for _, pod := range pods {
		if condition(pod) {
			filtered = append(filtered, pod)
		}
	}
	return filtered
}

func indexOfPods(pods []*model.PodInfo, condition func(*model.PodInfo) bool) int {
	index := 0
	for _, pod := range pods {
		if condition(pod) {
			return index
		}
		index++
	}
	return -1
}

func getHostIp(node corev1.Node) string {
	for _, val := range node.Status.Addresses {
		if val.Type == corev1.NodeInternalIP {
			return val.Address
		}
	}

	return ""
}
