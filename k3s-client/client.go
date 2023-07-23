package client

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"

	"k8s.io/client-go/tools/cache"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	v1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

const defaultCacheTimeS int = 20
const defaultNodesMetricsCacheTimeS = 60

type K3sClient struct {
	config           *rest.Config
	clientset        *kubernetes.Clientset
	metricsClientset *metricsv.Clientset
	podCache         *sync.Map

	nodesStatus    map[string]*model.NodeMetrics
	nodesCacheTime int
	nodesTime      time.Time

	serviceListener map[string]chan struct{}

	cacheTimeS int
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

	cacheTime, err := strconv.Atoi(os.Getenv("CACHE_TIME_S"))
	if err != nil {
		cacheTime = defaultCacheTimeS
	}
	log.Println("CACHE_TIME_S:", cacheTime)

	nodesMetricsCacheTimeS, err := strconv.Atoi(os.Getenv("NODE_METRICS_CACHE_TIME_S"))
	if err != nil {
		nodesMetricsCacheTimeS = defaultNodesMetricsCacheTimeS
	}
	log.Println("NODE_METRICS_CACHE_TIME_S:", nodesMetricsCacheTimeS)

	return &K3sClient{
		config:           config,
		clientset:        clientset,
		metricsClientset: metricsClientset,
		podCache:         &sync.Map{},
		serviceListener:  make(map[string]chan struct{}, 0),
		nodesCacheTime:   nodesMetricsCacheTimeS,
		cacheTimeS:       cacheTime,
	}, nil
}

func (c *K3sClient) GetPodsForService(namespace string, serviceName string) ([]*model.PodInfo, map[string]string, string, error) {
	if cached, found := c.podCache.Load(serviceName); found {
		cachedData := cached.(*model.PodInfoCache)

		log.Println("Returning cached data for service", serviceName)
		return cachedData.Pods, cachedData.Annotations, cachedData.TargetPort, nil
	}

	service, err := c.clientset.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get service %s: %v\n", serviceName, err)
		return nil, nil, "", err
	}

	defer c.startListener(namespace, serviceName, labels.Set(service.Spec.Selector).AsSelector())
	return c.callKubePodApi(namespace, serviceName, service)
}

func (c *K3sClient) GetNodesStatus() (map[string]*model.NodeMetrics, error) {
	if c.nodesStatus != nil && int(time.Since(c.nodesTime).Seconds()) < c.nodesCacheTime {
		log.Println("Using node status cache")
		return c.nodesStatus, nil
	}

	nodes, err := c.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Println("Failed to retrieve nodes on node status")
		return nil, err
	}

	nodeMetricsList, err := c.metricsClientset.MetricsV1beta1().NodeMetricses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Println("Failed to retrieve node metrices on node status")
		return nil, err
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

	log.Println("Returning host nodes status ::")
	for key, val := range hostMap {
		log.Printf("%s: CPU=%v - RAM=%v\n", key, val.CpuUsage, val.RamUsage)
	}

	c.nodesStatus = hostMap
	c.nodesTime = time.Now()

	return hostMap, nil
}

func (c *K3sClient) callKubePodApi(namespace string, serviceName string, service *corev1.Service) ([]*model.PodInfo, map[string]string, string, error) {
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
		CacheTime:   time.Now(),
		TargetPort:  targetPort,
	}

	c.podCache.Store(serviceName, cacheData)
	log.Println("Manually updated pods cache for service:", serviceName)

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
	c.serviceListener[serviceName] = stopCh

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
