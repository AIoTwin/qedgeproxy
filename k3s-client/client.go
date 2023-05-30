package client

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	podCache         map[string]*model.PodInfoCache

	nodesStatus    map[string]*model.NodeMetrics
	nodesCacheTime int
	nodesTime      time.Time

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
		podCache:         make(map[string]*model.PodInfoCache, 0),
		nodesCacheTime:   nodesMetricsCacheTimeS,
		cacheTimeS:       cacheTime,
	}, nil
}

func (c *K3sClient) GetPodsForService(namespace string, serviceName string) ([]*model.PodInfo, map[string]string, string, error) {
	if c.podCache[serviceName] != nil && int(time.Since(c.podCache[serviceName].CacheTime).Seconds()) < c.cacheTimeS {
		log.Println("Returning cached data for service", serviceName)
		return c.podCache[serviceName].Pods, c.podCache[serviceName].Annotations, c.podCache[serviceName].TargetPort, nil
	}

	podList := make([]*model.PodInfo, 0)

	service, err := c.clientset.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get service %s: %v\n", serviceName, err)
		return nil, nil, "", err
	}

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

	if c.podCache[serviceName] == nil {
		c.podCache[serviceName] = &model.PodInfoCache{}
	}

	c.podCache[serviceName].Pods = podList
	c.podCache[serviceName].Annotations = annotations
	c.podCache[serviceName].CacheTime = time.Now()
	c.podCache[serviceName].TargetPort = targetPort

	log.Println("Adjusting pods cache ::", serviceName)

	return podList, annotations, targetPort, nil
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

func getHostIp(node corev1.Node) string {
	for _, val := range node.Status.Addresses {
		if val.Type == corev1.NodeInternalIP {
			return val.Address
		}
	}

	return ""
}
