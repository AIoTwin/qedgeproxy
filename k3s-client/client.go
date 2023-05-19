package client

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

const defaultCacheTimeS int = 20

type K3sClient struct {
	config           *rest.Config
	clientset        *kubernetes.Clientset
	metricsClientset *metricsv.Clientset
	podCache         map[string]*model.PodInfoCache
	cacheTimeS       int
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
	log.Println("Cache time set to", cacheTime, "s")

	return &K3sClient{
		config:           config,
		clientset:        clientset,
		metricsClientset: metricsClientset,
		podCache:         make(map[string]*model.PodInfoCache, 0),
		cacheTimeS:       cacheTime,
	}, nil
}

func (c *K3sClient) GetPodsForService(namespace string, serviceName string) ([]*model.PodInfo, *metav1.LabelSelector, map[string]string, error) {
	if c.podCache[serviceName] != nil && int(time.Since(c.podCache[serviceName].CacheTime).Seconds()) < c.cacheTimeS {
		log.Println("Returning cached data for service", serviceName)
		return c.podCache[serviceName].Pods, c.podCache[serviceName].PodSelector, c.podCache[serviceName].Annotations, nil
	}

	podList := make([]*model.PodInfo, 0)

	service, err := c.clientset.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get service %s: %v\n", serviceName, err)
		return nil, nil, nil, err
	}

	annotations := service.Annotations

	podSelector := &metav1.LabelSelector{MatchLabels: service.Spec.Selector}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(podSelector)})
	if err != nil {
		log.Printf("Failed to list pods for service %s: %v\n", serviceName, err)
		return nil, nil, nil, err
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

	return podList, podSelector, annotations, nil
}

func (c *K3sClient) GetPodsStatus(namespace string, podSelector *metav1.LabelSelector, podsHostMap map[string]string) (map[string]float64, error) {
	podMetricsList, err := c.metricsClientset.MetricsV1beta1().PodMetricses(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(podSelector)})
	if err != nil {
		log.Printf("Failed to list pod metrics: %v\n", err)
		return nil, err
	}

	maxCpuUsage, maxRamUsage := 1.0, 1.0
	minCpuUsage, minRamUsage := 0.0, 0.0
	var metrics = make(map[string]*model.PodMetrics, 0)
	for _, podMetrics := range podMetricsList.Items {

		var cpuUsageQ, ramUsageQ resource.Quantity
		for _, container := range podMetrics.Containers {
			cpuUsageQ.Add(container.Usage[corev1.ResourceCPU])
			ramUsageQ.Add(container.Usage[corev1.ResourceMemory])
		}
		cpuUsage := cpuUsageQ.AsApproximateFloat64()
		ramUsage := ramUsageQ.AsApproximateFloat64()

		if cpuUsage > maxCpuUsage {
			maxCpuUsage = cpuUsage
		}
		if cpuUsage < minCpuUsage {
			minCpuUsage = cpuUsage
		}

		if ramUsage > maxRamUsage {
			maxRamUsage = ramUsage
		}
		if ramUsage < minRamUsage {
			minRamUsage = ramUsage
		}

		if val, ok := metrics[podsHostMap[podMetrics.ObjectMeta.Name]]; !ok {
			metrics[podsHostMap[podMetrics.ObjectMeta.Name]] = &model.PodMetrics{
				CPUUsage: cpuUsage,
				RAMUsage: ramUsage,
			}
		} else {
			val.CPUUsage = (val.CPUUsage + cpuUsage) / 2
			val.RAMUsage = (val.RAMUsage + ramUsage) / 2
		}
	}

	var resultMetrics = make(map[string]float64)
	for k, v := range metrics {
		resultMetrics[k] = calculateWeight(v.RAMUsage, v.CPUUsage, maxRamUsage, maxCpuUsage, minRamUsage, minCpuUsage)
	}

	return resultMetrics, nil
}

func calculateWeight(ramValue float64, cpuValue float64, maxRamValue float64, maxCpuValue float64, minRamValue float64, minCpuValue float64) float64 {
	ram := (ramValue - minRamValue) / (maxRamValue - minRamValue)
	cpu := (cpuValue - minCpuValue) / (maxCpuValue - minCpuValue)

	// we add 2 so that values are in range [1,2]
	return (ram + cpu + 2) / 2
}
