package client

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"gitlab.tel.fer.hr/vjukanovic/k3s-custom-routing/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultCacheTimeS int = 20

type K3sClient struct {
	config     *rest.Config
	clientset  *kubernetes.Clientset
	podCache   map[string]*model.PodInfoCache
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

	cacheTime, err := strconv.Atoi(os.Getenv("CACHE_TIME_S"))
	if err != nil {
		cacheTime = defaultCacheTimeS
	}
	log.Println("Cache time set to", cacheTime, "s")

	return &K3sClient{
		config:     config,
		clientset:  clientset,
		podCache:   make(map[string]*model.PodInfoCache, 0),
		cacheTimeS: cacheTime,
	}, nil
}

func (c *K3sClient) GetPodsForService(namespace string, serviceName string) ([]*model.PodInfo, map[string]string, error) {
	if c.podCache[serviceName] != nil && int(time.Since(c.podCache[serviceName].CacheTime).Seconds()) < c.cacheTimeS {
		log.Println("Returning cached data for service", serviceName)
		return c.podCache[serviceName].Pods, c.podCache[serviceName].Annotations, nil
	}

	podList := make([]*model.PodInfo, 0)

	service, err := c.clientset.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get service %s: %v\n", serviceName, err)
		return nil, nil, err
	}

	annotations := service.Annotations

	podSelector := &metav1.LabelSelector{MatchLabels: service.Spec.Selector}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(podSelector)})
	if err != nil {
		log.Printf("Failed to list pods for service %s: %v\n", serviceName, err)
		return nil, nil, err
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

	return podList, annotations, nil
}
