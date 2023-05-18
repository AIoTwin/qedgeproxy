package model

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PodInfo struct {
	Namespace string
	Name      string
	IP        string
	HostIP    string
}

type PodMetrics struct {
	CPUUsage float64
	RAMUsage float64
}

type LatencyInfo struct {
	AverageLatency int
	ReqCount       int
}

type PodInfoCache struct {
	CacheTime   time.Time
	Pods        []*PodInfo
	Annotations map[string]string
	PodSelector *metav1.LabelSelector
}
